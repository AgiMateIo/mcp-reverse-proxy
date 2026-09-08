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

// The example configuration in the repository root is a configuration the
// gateway would actually start with.
//
// An example is copied, not read, so one that has drifted from the code is
// worse than none: an operator meets the failure at startup with no way to tell
// whether the mistake is theirs. Loading is not enough on its own — the policy
// is where a define-new deployment without a command allowlist is refused, and
// that check lives in the policy package rather than in Load.
func TestExampleConfigurationLoads(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "config.example.yaml")
	f, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if _, err := policy.New(f.Policy); err != nil {
		t.Fatalf("the example's policy would not start: %v", err)
	}
	// Its point is to show every option, so the ones with defaults must be
	// spelled out rather than left to be filled in.
	if f.Limits != config.DefaultLimits {
		t.Errorf("the example sets limits %+v, want it to show the defaults %+v",
			f.Limits, config.DefaultLimits)
	}
	if len(f.Servers) == 0 {
		t.Error("the example declares no servers")
	}
}
