package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
)

// Task 11.5: the deployment document names every option a deployment can set.
//
// The check is mechanical rather than a reading, because the failure it guards
// against is mechanical: a field added to the configuration and never
// documented. An operator meeting an undocumented option has to read the source
// to learn what it does, and the two options that matter most here — the policy
// mode and the command allowlist — are the ones that decide whether an
// authenticated subject can run programs on the host.
func TestDeploymentDocumentNamesEveryOption(t *testing.T) {
	t.Parallel()
	doc := deploymentDoc(t)

	for _, v := range []any{
		config.File{}, config.Server{}, config.Limits{}, config.PolicyConfig{},
		// The header carries the same shape back, and its own options are as
		// much a part of the deployment's surface as the file's.
		config.Header{}, config.ServerPatch{},
	} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			if !strings.Contains(doc, "`"+name+"`") {
				t.Errorf("%s.%s is configurable as %q and the deployment document does not name it",
					typ.Name(), typ.Field(i).Name, name)
			}
		}
	}

	// The values of the one option whose choice is a security decision, and
	// the scopes that gate them.
	for _, mode := range []policy.Mode{
		policy.ModeOff, policy.ModeEnvOnly, policy.ModeOverrideKnown, policy.ModeDefineNew,
	} {
		if !strings.Contains(doc, "`"+string(mode)+"`") {
			t.Errorf("the deployment document does not name the policy mode %q", mode)
		}
	}
	for _, scope := range []string{policy.ScopeEnv, policy.ScopeOverride, policy.ScopeDefine} {
		if !strings.Contains(doc, scope) {
			t.Errorf("the deployment document does not name the scope %q", scope)
		}
	}
	for _, era := range []config.Era{config.EraAuto, config.EraModern, config.EraLegacy} {
		if !strings.Contains(doc, "`"+string(era)+"`") {
			t.Errorf("the deployment document does not name the era %q", era)
		}
	}

	// The two warnings the document exists to carry.
	for _, subject := range []string{config.HeaderName, "define-new"} {
		if !strings.Contains(doc, subject) {
			t.Errorf("the deployment document does not discuss %q", subject)
		}
	}
}

func deploymentDoc(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "deployment.md")
	// G304: a fixed path inside the repository, not one any input can choose.
	data, err := os.ReadFile(path) //nolint:gosec // see above
	if err != nil {
		t.Fatalf("read the deployment document: %v", err)
	}
	return string(data)
}
