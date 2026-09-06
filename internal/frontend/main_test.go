package frontend_test

import (
	"os"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// TestMain doubles as the fixture entry point: the backends these tests put
// behind the gateway are this binary, re-executed by the process pool.
func TestMain(m *testing.M) {
	testfixtures.DispatchFromEnv()
	os.Exit(m.Run())
}
