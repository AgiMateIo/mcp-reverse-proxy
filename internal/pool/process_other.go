//go:build !unix

package pool

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// The gateway isolates a backend in a process group so that termination reaches
// its descendants. Windows offers Job Objects for the same purpose; wiring them
// up is outside this version, and starting a backend that could be orphaned is
// worse than refusing to start it.

func isolate(*exec.Cmd) {}

func signalGroup(int, syscall.Signal) error {
	return fmt.Errorf("signal a process group: %w", errors.ErrUnsupported)
}
