package testfixtures

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Environment variables that configure a fixture running as a child process.
// A fixture is spawned by the gateway's own process constructor, which passes
// the server's configured environment through, so this is the channel a test
// has for tuning one.
const (
	EnvMode             = "MCPFIXTURE_MODE"
	EnvRevision         = "MCPFIXTURE_REVISION"
	EnvNegotiateOnce    = "MCPFIXTURE_NEGOTIATE_ONCE"
	EnvStderr           = "MCPFIXTURE_STDERR"
	EnvChildPIDFile     = "MCPFIXTURE_CHILD_PIDFILE"
	EnvEnvDumpFile      = "MCPFIXTURE_ENV_DUMP_FILE"
	EnvIgnoreStdinClose = "MCPFIXTURE_IGNORE_STDIN_CLOSE"
	EnvAnnounceChanges  = "MCPFIXTURE_ANNOUNCE_CHANGES"
	EnvExitOnCall       = "MCPFIXTURE_EXIT_ON_CALL"
)

// FromEnv reads a fixture's mode and options from the environment.
func FromEnv() (Mode, Options, error) {
	mode := Mode(os.Getenv(EnvMode))
	if _, ok := serverNames[mode]; !ok {
		return "", Options{}, fmt.Errorf("%w: %q in %s", ErrUnknownMode, mode, EnvMode)
	}
	return mode, Options{
		Revision:         os.Getenv(EnvRevision),
		NegotiateOnce:    os.Getenv(EnvNegotiateOnce) != "",
		Stderr:           os.Getenv(EnvStderr),
		ChildPIDFile:     os.Getenv(EnvChildPIDFile),
		EnvDumpFile:      os.Getenv(EnvEnvDumpFile),
		IgnoreStdinClose: os.Getenv(EnvIgnoreStdinClose) != "",
		ExitOnCall:       os.Getenv(EnvExitOnCall) != "",
		AnnounceChanges:  os.Getenv(EnvAnnounceChanges) != "",
	}, nil
}

// Env renders a mode and options as the environment of a fixture process.
func Env(mode Mode, opts Options) map[string]string {
	env := map[string]string{EnvMode: string(mode)}
	for k, v := range map[string]string{
		EnvRevision:     opts.Revision,
		EnvStderr:       opts.Stderr,
		EnvChildPIDFile: opts.ChildPIDFile,
		EnvEnvDumpFile:  opts.EnvDumpFile,
	} {
		if v != "" {
			env[k] = v
		}
	}
	for k, set := range map[string]bool{
		EnvNegotiateOnce:    opts.NegotiateOnce,
		EnvIgnoreStdinClose: opts.IgnoreStdinClose,
		EnvExitOnCall:       opts.ExitOnCall,
		EnvAnnounceChanges:  opts.AnnounceChanges,
	} {
		if set {
			env[k] = "1"
		}
	}
	return env
}

// DispatchFromEnv runs this process as a fixture and exits, if it was started
// as one. Otherwise it returns and the process goes on with its own work.
//
// A test package that spawns fixtures calls this first thing in TestMain: the
// gateway starts a backend by executing a command, and the command tests give
// it is the test binary itself. Without this, the binary would re-run its own
// tests instead of serving MCP.
func DispatchFromEnv() {
	if os.Getenv(EnvMode) == "" {
		return
	}
	mode, opts, err := FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testfixtures:", err)
		os.Exit(1)
	}
	// Signals reach the whole process group, which is how the gateway
	// escalates when closing stdin was not enough.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := RunWith(ctx, mode, opts, os.Stdin, os.Stdout); err != nil &&
		!errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "testfixtures:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
