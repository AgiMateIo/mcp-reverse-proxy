package backend_test

import (
	"encoding/json"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// Tasks 10.2 and 10.3: a backend of either era reports a change, and the
// gateway hears the same thing from both.
//
// The eras deliver it differently — a modern backend over the
// subscriptions/listen stream the gateway opened during connect, a legacy one
// as a bare notification with no stream and no subscription id — and the point
// is that neither difference survives to the caller. That fold is what
// normalizing a legacy notification to revision 2026-07-28 amounts to here:
// the shapes meet at one callback carrying one kind.
func TestChangeNotificationsFromBothEras(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode testfixtures.Mode
		kind string
		want backend.ChangeKind
	}{
		{"modern backend, tools", testfixtures.ModeModern, "tools", backend.ChangeTools},
		{"modern backend, prompts", testfixtures.ModeModern, "prompts", backend.ChangePrompts},
		{"legacy backend, tools", testfixtures.ModeLegacy, "tools", backend.ChangeTools},
		{"legacy backend, resources", testfixtures.ModeLegacy, "resources", backend.ChangeResources},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := dialWith(t, tt.mode, testfixtures.Options{AnnounceChanges: true},
				config.Server{ID: "srv", Era: config.EraAuto})
			if err != nil {
				t.Fatalf("connect to the %s fixture: %v", tt.mode, err)
			}
			// The call is what makes the fixture announce; it answers the call
			// first, so the notification cannot arrive before its cause.
			if _, err := s.conn.CallTool(t.Context(), "change",
				json.RawMessage(`{"kind":"`+tt.kind+`"}`)); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			got := s.changes.await(t)
			if got.Kind != tt.want {
				t.Errorf("kind = %q, want %q", got.Kind, tt.want)
			}
			if got.ServerID != "srv" {
				t.Errorf("serverID = %q, want the backend it came from", got.ServerID)
			}
		})
	}
}
