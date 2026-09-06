// Command mcp-reverse-proxy exposes one modern HTTP Streamable MCP endpoint in
// front of several local stdio MCP servers.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-reverse-proxy:", err)
		os.Exit(1)
	}
}

// run is the whole program, with its inputs and its one output passed in so
// that a test can start the gateway, learn where it is listening, and stop it
// by cancelling the context — the same way a signal stops it in production.
func run(ctx context.Context, args []string, out io.Writer) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	gw, err := build(ctx, opts)
	if err != nil {
		return err
	}
	return gw.serve(ctx, out)
}
