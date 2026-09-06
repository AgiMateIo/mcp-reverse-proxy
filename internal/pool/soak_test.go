//go:build unix

package pool_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"golang.org/x/sync/errgroup"
)

// soak sizes the run: many more subjects than slots, so that every request
// after the first few has to evict somebody.
const (
	soakSubjects = 48
	soakRounds   = 8
	soakSlots    = 4
)

// soakBudget is how long one request may spend waiting for a slot before the
// run calls it a failure rather than contention.
const soakBudget = 60 * time.Second

// Task 11.4: the pool under sustained load leaks neither processes nor
// descriptors.
//
// The two leaks are different failures. A process left behind is a backend the
// gateway forgot to stop — and, because backends run in their own process
// group, one that may itself hold descendants. A descriptor left behind is a
// pipe of a process that was stopped correctly but not reaped, which is the
// cheaper mistake and the one that takes a host down more slowly.
//
// Deliberately not parallel: both counts are process-global, so a test running
// alongside would be counted as this one's leak.
func TestSoakUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("the soak spawns dozens of processes")
	}
	fingerprints, err := config.NewFingerprinter()
	if err != nil {
		t.Fatalf("NewFingerprinter: %v", err)
	}
	limits := config.Limits{
		MaxProcesses:           soakSlots,
		MaxProcessesPerSubject: 2,
		// Long enough that nothing expires: eviction under pressure is what
		// this measures, and an idle reaper firing in the middle would make it
		// measure two things at once.
		IdleTTL:      config.Duration(time.Hour),
		ProbeTimeout: config.Duration(probeTimeout),
	}
	newPool := func() *pool.Pool {
		return pool.New(backend.NewConnector("soak", probeTimeout, nil),
			fingerprints, limits, testPolicy, nil)
	}

	// One full cycle before the baseline. The runtime opens descriptors lazily
	// — the poller, the signal pipe, /dev/urandom — and the first spawn is
	// where that happens, so counting before it would report the runtime's
	// own start-up as a leak.
	warm := newPool()
	if _, err := warm.For(subject("warm-up"), fixtureBackend(t, "warm", nil)).ListTools(t.Context()); err != nil {
		t.Fatalf("warm-up request: %v", err)
	}
	if err := warm.Close(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("close the warm-up pool: %v", err)
	}
	baseline := openDescriptors(t)

	p := newPool()
	t.Cleanup(func() { _ = p.Close(context.WithoutCancel(t.Context())) })

	var seen pids
	var group errgroup.Group
	// Bounded so the load stays pressure on the pool rather than on the host's
	// process table.
	group.SetLimit(soakSlots * 2)
	for i := range soakSubjects {
		s := subject(strconv.Itoa(i))
		server := fixtureBackend(t, "soak", nil)
		group.Go(func() error {
			for range soakRounds {
				h := p.For(s, server)
				if err := serveWithRetry(t.Context(), h); err != nil {
					return fmt.Errorf("%s: %w", s, err)
				}
				seen.add(h.PID())
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatalf("the soak failed: %v", err)
	}

	stats := p.Stats()
	t.Logf("soak: %+v over %d distinct processes", stats, len(seen.list()))
	if stats.Evicted == 0 {
		t.Errorf("nothing was evicted under %d subjects in %d slots: %+v",
			soakSubjects, soakSlots, stats)
	}
	if stats.Size > soakSlots {
		t.Errorf("the pool holds %d backends, over its limit of %d", stats.Size, soakSlots)
	}
	if len(seen.list()) < soakSlots {
		t.Fatalf("the run observed %d processes, too few to say anything about leaks", len(seen.list()))
	}

	if err := p.Close(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("close the pool: %v", err)
	}

	// Every process the run saw is gone, and so is anything it spawned: the
	// check is on the process group, which is what Setpgid exists to make
	// terminable as a whole.
	for _, pid := range seen.list() {
		if alive := waitForGroupToGo(pid); alive {
			t.Errorf("process group %d outlived the pool", pid)
		}
	}
	if after := openDescriptors(t); after > baseline {
		t.Errorf("%d descriptors open after the soak, %d before", after, baseline)
	}
}

// serveWithRetry makes one request, waiting out the moments when every slot is
// busy.
//
// Refusing a request for want of a free slot is the pool working as specified,
// and a client's answer to it is to come back. Treating the refusal as the end
// of the round instead would let the soak finish without the pool ever having
// to make room, which is the very thing being measured.
func serveWithRetry(ctx context.Context, h *pool.Handle) error {
	// A budget rather than a number of attempts: how long one request takes
	// varies by an order of magnitude between a plain run and one under the
	// race detector, and a fixed count would make the soak fail on the slower
	// of the two for no reason but its speed.
	ctx, cancel := context.WithTimeout(ctx, soakBudget)
	defer cancel()
	var last error
	for ctx.Err() == nil {
		_, err := h.ListTools(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pool.ErrPoolExhausted) && !errors.Is(err, pool.ErrSubjectQuotaExhausted) {
			return err
		}
		last = err
		time.Sleep(5 * time.Millisecond)
	}
	return last
}

// A pids is the set of processes a soak observed, from several goroutines.
type pids struct {
	mu  sync.Mutex
	set map[int]struct{}
}

func (p *pids) add(pid int) {
	if pid == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.set == nil {
		p.set = map[int]struct{}{}
	}
	p.set[pid] = struct{}{}
}

func (p *pids) list() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.set))
	for pid := range p.set {
		out = append(out, pid)
	}
	return out
}

func subject(sub string) auth.Subject {
	return auth.Subject{Issuer: "https://issuer.example/", Sub: "soak-" + sub}
}

// waitForGroupToGo reports whether a backend's process group is still alive
// after being given a moment to finish going away. Close waits for the
// processes it stopped, so the first look should already find the group gone;
// the poll is there because a signal reaching a descendant is not
// instantaneous.
func waitForGroupToGo(pid int) bool {
	for range 100 {
		// Signal 0 checks for existence without delivering anything. The
		// negative pid addresses the whole group, so a surviving descendant
		// counts as well as the backend itself.
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// openDescriptors counts this process's open files. /dev/fd is the one place
// both Linux and the BSDs expose them.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors on this system: %v", err)
	}
	// The directory handle opened to read it is itself one of the entries, and
	// it is closed on return; counting it every time keeps the two counts
	// comparable.
	return len(entries)
}
