// Command mcpfixture runs one of the stdio MCP fixture servers as a real child
// process, for the tests that need process behavior rather than wire behavior.
//
// Usage: mcpfixture <legacy|modern|silent|misbehaving>
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcpfixture:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: mcpfixture <%s>", joinModes())
	}
	// Signals reach the whole process group. A fixture idle on stdin stays put
	// until stdin closes, so this ends a fixture mid-conversation, not a
	// wedged one — the shutdown escalation is exercised by a fixture that
	// deliberately ignores the stdin close.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return testfixtures.Run(ctx, testfixtures.Mode(os.Args[1]), os.Stdin, os.Stdout)
}

func joinModes() string {
	names := make([]string, 0, len(testfixtures.Modes))
	for _, m := range testfixtures.Modes {
		names = append(names, string(m))
	}
	return strings.Join(names, "|")
}
