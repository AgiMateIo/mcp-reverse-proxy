package pool_test

import (
	"os"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// TestMain doubles as the fixture entry point. Tests spawn this very binary
// through pool.Spawn, so the one process constructor the gateway has is the one
// under test — building a separate fixture binary would exercise a second way
// of starting a process.
func TestMain(m *testing.M) {
	testfixtures.DispatchFromEnv()
	os.Exit(m.Run())
}
