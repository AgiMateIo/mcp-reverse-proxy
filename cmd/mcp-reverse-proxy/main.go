// Command mcp-reverse-proxy exposes one modern HTTP Streamable MCP endpoint in
// front of several local stdio MCP servers.
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-reverse-proxy:", err)
		os.Exit(1)
	}
}

func run() error {
	// Wiring — configuration, policy, auth, pool, frontend — lands with the
	// work that builds those layers. The skeleton only fixes the module layout.
	return errors.New("not implemented")
}
