//go:build unix

package pool

import (
	"fmt"
	"os/exec"
	"syscall"
)

// isolate puts the child in its own process group, so that a signal sent to the
// group reaches every descendant it goes on to spawn and not only the child
// itself.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the whole process group led by pid. Signalling the
// group is the point: a backend that spawned helpers must not leave them
// behind.
func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil {
		return fmt.Errorf("signal process group %d with %v: %w", pid, sig, err)
	}
	return nil
}
