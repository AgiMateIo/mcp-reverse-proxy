package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The composed program: it starts, publishes its metadata, refuses an
// unauthenticated request, and stops when its context ends.
//
// This is deliberately shallow. What each layer does is tested where that layer
// lives; what no other test can show is that they are wired together at all —
// that a binary an operator starts with a configuration file ends up serving
// the endpoint rather than failing at some seam between packages.
func TestGatewayStartsAndServes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	write(t, configPath, `{
	  "servers": [{"id": "gh", "command": "/bin/cat"}],
	  "limits": {"maxProcesses": 4, "idleTtl": "1m"},
	  "policy": {"mode": "env-only"}
	}`)
	keyPath := filepath.Join(dir, "issuer.pem")
	write(t, keyPath, publicKeyPEM(t))

	addr, stop := start(t, []string{
		"-config", configPath,
		"-addr", "127.0.0.1:0",
		"-resource", "https://gateway.example/mcp",
		"-issuer", "https://issuer.example/",
		"-key", "https://issuer.example/=" + keyPath,
	})

	// The metadata document is public: a client reads it precisely because it
	// does not have a token yet.
	var metadata struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	res := get(t, "http://"+addr+"/.well-known/oauth-protected-resource") //nolint:bodyclose // decode below closes it
	if res.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", res.StatusCode)
	}
	decode(t, res, &metadata)
	if metadata.Resource != "https://gateway.example/mcp" {
		t.Errorf("resource = %q", metadata.Resource)
	}
	if len(metadata.AuthorizationServers) != 1 {
		t.Errorf("authorization_servers = %v", metadata.AuthorizationServers)
	}
	// env-only is the configured mode, so the scope for it is on offer and the
	// two above it are not: advertising a scope this deployment would refuse
	// sends a client on a step-up trip that ends in the same refusal.
	if !has(metadata.ScopesSupported, "mcp:access") || !has(metadata.ScopesSupported, "mcp:config:env") {
		t.Errorf("scopes_supported = %v", metadata.ScopesSupported)
	}
	if has(metadata.ScopesSupported, "mcp:config:define") {
		t.Errorf("scopes_supported offers a scope this mode never honors: %v", metadata.ScopesSupported)
	}

	// The endpoint itself is guarded, and says where to go for a token.
	res = get(t, "http://"+addr+"/mcp")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("endpoint status = %d, want 401", res.StatusCode)
	}
	_ = res.Body.Close()
	if challenge := res.Header.Get("WWW-Authenticate"); !strings.Contains(challenge, "resource_metadata=") {
		t.Errorf("WWW-Authenticate = %q, want it to name the metadata document", challenge)
	}

	// And it stops on its own when told to, rather than having to be killed.
	stop(t)
}

// The failures an operator meets before anything is listening.
func TestStartupRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "issuer.pem")
	write(t, keyPath, publicKeyPEM(t))
	good := filepath.Join(dir, "config.json")
	write(t, good, `{"servers":[{"id":"gh","command":"/bin/cat"}]}`)

	base := []string{
		"-config", good, "-addr", "127.0.0.1:0",
		"-resource", "https://gateway.example/mcp",
		"-issuer", "https://issuer.example/",
		"-key", "https://issuer.example/=" + keyPath,
	}
	replace := func(flag, value string) []string {
		out := append([]string(nil), base...)
		for i := range out {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}

	definesWithoutAllowlist := filepath.Join(dir, "define-new.json")
	write(t, definesWithoutAllowlist, `{"servers":[],"policy":{"mode":"define-new"}}`)
	notAKey := filepath.Join(dir, "not-a-key.pem")
	write(t, notAKey, "this is not PEM\n")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no configuration file named", []string{"-addr", "127.0.0.1:0"}, "-config"},
		{"the configuration file is missing", replace("-config", filepath.Join(dir, "absent.json")), "absent.json"},
		{"define-new without a command allowlist", replace("-config", definesWithoutAllowlist), "allowlist"},
		{"the key is not a key", replace("-key", "https://issuer.example/="+notAKey), "PEM"},
		{"the resource is not an absolute URI", replace("-resource", "/mcp"), "absolute URI"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := run(t.Context(), tt.args, io.Discard)
			if err == nil {
				t.Fatal("the gateway started, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error does not mention %q: %v", tt.want, err)
			}
		})
	}
}

// start runs the gateway until the returned stop is called, and reports the
// address it actually bound.
func start(t *testing.T, args []string) (string, func(*testing.T)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()

	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Go(func() {
		err := run(ctx, args, writer)
		_ = writer.CloseWithError(err)
		errs <- err
	})

	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("the gateway never reported an address: %v (%v)", err, <-errs)
	}
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on ")
	if !ok {
		cancel()
		wg.Wait()
		t.Fatalf("first line was %q, want the address", line)
	}

	stopped := false
	stop := func(t *testing.T) {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("the gateway stopped with an error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the gateway did not stop")
		}
		wg.Wait()
	}
	t.Cleanup(func() { stop(t) })
	return addr, stop
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return res
}

func decode(t *testing.T, res *http.Response, into any) {
	t.Helper()
	defer func() { _ = res.Body.Close() }() //nolint:bodyclose // this is the close
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// publicKeyPEM is a throwaway signing key's public half, in the form the
// gateway reads.
func publicKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
