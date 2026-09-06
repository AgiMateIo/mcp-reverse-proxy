package aggregate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
)

// A stub is one backend as the tests drive it: a fixed set of entries, an
// optional failure, and a count of what it was asked to do — which is how a
// test shows that a call reached one backend and no other.
type stub struct {
	tools     []string
	prompts   []string
	resources []string
	templates []string
	// fail, when set, is returned by every method.
	fail error
	// missing lists the methods answered with "method not found", the way a
	// tools-only backend answers prompts/list.
	missing map[string]bool
	// hang makes the backend never answer, which is what "does not respond"
	// means: not a refusal, but silence until somebody stops waiting.
	hang  bool
	ttlMs int

	calls atomic.Int64
	// lastName records what the backend was asked for, unprefixed.
	lastName atomic.Value
}

func (s *stub) answer(method string) error {
	if s.missing[method] {
		return fmt.Errorf("%w", &backend.ProtocolError{ServerID: "stub", Code: -32601, Message: "method not found"})
	}
	return s.fail
}

func (s *stub) ListTools(ctx context.Context) (backend.ToolList, error) {
	if s.hang {
		<-ctx.Done()
		return backend.ToolList{}, ctx.Err()
	}
	if err := s.answer("tools/list"); err != nil {
		return backend.ToolList{}, err
	}
	tools := make([]backend.Tool, 0, len(s.tools))
	for _, n := range s.tools {
		tools = append(tools, backend.Tool{Name: n})
	}
	return backend.ToolList{Tools: tools, Cache: backend.Cache{TTLMs: s.ttlMs}}, nil
}

func (s *stub) CallTool(_ context.Context, name string, _ json.RawMessage) (backend.ToolResult, error) {
	s.calls.Add(1)
	s.lastName.Store(name)
	if err := s.answer("tools/call"); err != nil {
		return backend.ToolResult{}, err
	}
	return backend.ToolResult{Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)}, nil
}

func (s *stub) ListPrompts(context.Context) (backend.PromptList, error) {
	if err := s.answer("prompts/list"); err != nil {
		return backend.PromptList{}, err
	}
	prompts := make([]backend.Prompt, 0, len(s.prompts))
	for _, n := range s.prompts {
		prompts = append(prompts, backend.Prompt{Name: n})
	}
	return backend.PromptList{Prompts: prompts}, nil
}

func (s *stub) GetPrompt(_ context.Context, name string, _ map[string]string) (backend.PromptResult, error) {
	s.calls.Add(1)
	s.lastName.Store(name)
	if err := s.answer("prompts/get"); err != nil {
		return backend.PromptResult{}, err
	}
	return backend.PromptResult{Messages: json.RawMessage(`[]`)}, nil
}

func (s *stub) ListResources(context.Context) (backend.ResourceList, error) {
	if err := s.answer("resources/list"); err != nil {
		return backend.ResourceList{}, err
	}
	resources := make([]backend.Resource, 0, len(s.resources))
	for _, u := range s.resources {
		resources = append(resources, backend.Resource{URI: u, Name: u})
	}
	return backend.ResourceList{Resources: resources}, nil
}

func (s *stub) ListResourceTemplates(context.Context) (backend.ResourceTemplateList, error) {
	if err := s.answer("resources/templates/list"); err != nil {
		return backend.ResourceTemplateList{}, err
	}
	templates := make([]backend.ResourceTemplate, 0, len(s.templates))
	for _, u := range s.templates {
		templates = append(templates, backend.ResourceTemplate{Name: u, URITemplate: u})
	}
	return backend.ResourceTemplateList{Templates: templates}, nil
}

func (s *stub) ReadResource(_ context.Context, uri string) (backend.ResourceContents, error) {
	s.calls.Add(1)
	s.lastName.Store(uri)
	if err := s.answer("resources/read"); err != nil {
		return backend.ResourceContents{}, err
	}
	return backend.ResourceContents{Contents: json.RawMessage(`[{"uri":"` + uri + `"}]`)}, nil
}

func gateway(sources map[string]*stub) (*aggregate.Gateway, map[string]*stub) {
	as := make(map[string]aggregate.Source, len(sources))
	for id, s := range sources {
		as[id] = s
	}
	return aggregate.New(as, 0, nil), sources
}

func names(tools []backend.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// Task 9.1: three backends merge into one listing, each entry under its
// namespaced name.
func TestMergedListing(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{
		"gh":    {tools: []string{"search", "issue"}},
		"docs":  {tools: []string{"search"}, prompts: []string{"summarize"}},
		"files": {tools: []string{"read"}, templates: []string{"file:///{path}"}},
	})
	s, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	want := []string{"docs__search", "files__read", "gh__issue", "gh__search"}
	if got := names(s.Tools); !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if len(s.Prompts) != 1 || s.Prompts[0].Name != "docs__summarize" {
		t.Errorf("prompts = %+v, want one docs__summarize", s.Prompts)
	}
	if len(s.Templates) != 1 || s.Templates[0].Name != "files__file:///{path}" {
		t.Errorf("templates = %+v, want the name namespaced", s.Templates)
	}
}

// Task 9.2 and 9.3: colliding and overlong namespaced names fail the surface
// rather than being resolved by a silent winner.
func TestAssembleRejects(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("t", aggregate.MaxNameBytes)
	tests := []struct {
		name     string
		listings []aggregate.Listing
		want     error
		// names is what the error must identify.
		names []string
	}{
		{
			name: "two backends producing the same tool name",
			listings: []aggregate.Listing{
				{Backend: "a", Tools: []backend.Tool{{Name: "b__c"}}},
				{Backend: "a__b", Tools: []backend.Tool{{Name: "c"}}},
			},
			want:  aggregate.ErrNameCollision,
			names: []string{"a", "a__b", "a__b__c"},
		},
		{
			name: "one backend listing a name twice",
			listings: []aggregate.Listing{
				{Backend: "gh", Tools: []backend.Tool{{Name: "search"}, {Name: "search"}}},
			},
			want:  aggregate.ErrNameCollision,
			names: []string{"gh", "gh__search"},
		},
		{
			name: "a prefix pushing a name past the limit",
			listings: []aggregate.Listing{
				{Backend: "gh", Tools: []backend.Tool{{Name: long}}},
			},
			want:  aggregate.ErrNameTooLong,
			names: []string{"gh", long},
		},
		{
			name: "a colliding prompt name",
			listings: []aggregate.Listing{
				{Backend: "a", Prompts: []backend.Prompt{{Name: "b__c"}}},
				{Backend: "a__b", Prompts: []backend.Prompt{{Name: "c"}}},
			},
			want:  aggregate.ErrNameCollision,
			names: []string{"a__b__c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := aggregate.Assemble(tt.listings)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
			for _, n := range tt.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("error does not name %q: %v", n, err)
				}
			}
		})
	}
}

// A tool and a prompt are separate namespaces: the same name in each is not a
// collision.
func TestKindsDoNotCollideWithEachOther(t *testing.T) {
	t.Parallel()
	_, err := aggregate.Assemble([]aggregate.Listing{{
		Backend: "gh",
		Tools:   []backend.Tool{{Name: "search"}},
		Prompts: []backend.Prompt{{Name: "search"}},
	}})
	if err != nil {
		t.Errorf("Assemble: %v", err)
	}
}

// Task 9.4 and 9.5: a call goes to the one backend named by the prefix, under
// the unprefixed name, and a name that routes nowhere reaches nobody.
func TestRouting(t *testing.T) {
	t.Parallel()
	g, sources := gateway(map[string]*stub{
		"gh":   {tools: []string{"search"}},
		"docs": {tools: []string{"search"}},
	})

	if _, err := g.CallTool(t.Context(), "gh__search", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := sources["gh"].lastName.Load(); got != "search" {
		t.Errorf("the backend was called with %v, want the unprefixed name", got)
	}
	if n := sources["docs"].calls.Load(); n != 0 {
		t.Errorf("the other backend was called %d times, want none", n)
	}

	for _, name := range []string{"nosuch__search", "search", "__search", "gh__"} {
		if _, err := g.CallTool(t.Context(), name, nil); !errors.Is(err, aggregate.ErrNotFound) {
			t.Errorf("CallTool(%q) = %v, want it to wrap %v", name, err, aggregate.ErrNotFound)
		}
	}
	// One reached gh, and nothing else reached any backend: an unroutable name
	// is not broadcast in the hope that somebody owns it.
	if n := sources["gh"].calls.Load() + sources["docs"].calls.Load(); n != 1 {
		t.Errorf("backends were called %d times, want exactly the one routable call", n)
	}
}

// Task 9.6: the wrapping is the spec's, and a listed resource reads back under
// the backend's own URI.
func TestResourceURIRoundTrip(t *testing.T) {
	t.Parallel()
	const original = "file:///readme.md"
	g, sources := gateway(map[string]*stub{"docs": {resources: []string{original}}})

	s, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(s.Resources) != 1 {
		t.Fatalf("resources = %+v, want one", s.Resources)
	}
	const want = "mcp-proxy://docs/file%3A%2F%2F%2Freadme.md"
	if got := s.Resources[0].URI; got != want {
		t.Fatalf("URI = %q, want %q", got, want)
	}
	if _, err := g.ReadResource(t.Context(), s.Resources[0].URI); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got := sources["docs"].lastName.Load(); got != original {
		t.Errorf("the backend was asked for %v, want its own URI %q", got, original)
	}
}

// A URI template keeps its RFC 6570 expressions: encoding them would leave the
// client with a literal where a variable belongs, and an expansion of the
// wrapped template still unwraps to what the backend would have expanded.
func TestURITemplateKeepsItsExpressions(t *testing.T) {
	t.Parallel()
	got := aggregate.WrapURITemplate("docs", "file:///{path}")
	const want = "mcp-proxy://docs/file%3A%2F%2F%2F{path}"
	if got != want {
		t.Fatalf("WrapURITemplate = %q, want %q", got, want)
	}
	id, uri, err := aggregate.UnwrapURI(strings.Replace(got, "{path}", "readme.md", 1))
	if err != nil || id != "docs" || uri != "file:///readme.md" {
		t.Errorf("unwrapping an expansion gave (%q, %q, %v)", id, uri, err)
	}
}

// Task 9.7: a URI that is not the gateway's, and one naming a backend that is
// not configured, are both the client's mistake.
func TestBadGatewayURI(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{"docs": {}})
	tests := []struct {
		name string
		uri  string
		want error
	}{
		{"another scheme", "file:///readme.md", aggregate.ErrBadURI},
		{"no backend", "mcp-proxy:///file%3A%2F%2Freadme.md", aggregate.ErrBadURI},
		{"no resource", "mcp-proxy://docs/", aggregate.ErrBadURI},
		{"unparseable", "mcp-proxy://docs/%zz", aggregate.ErrBadURI},
		{"unknown backend", "mcp-proxy://nosuch/file%3A%2F%2Freadme.md", aggregate.ErrUnknownBackend},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := g.ReadResource(t.Context(), tt.uri); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

// Task 9.8: an unavailable backend costs the listing its own entries and
// nothing else, and shortens the lifetime of the answer.
func TestListingSurvivesAnUnavailableBackend(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{
		"gh":   {tools: []string{"search"}, ttlMs: 60_000},
		"down": {fail: errors.New("no process")},
	})
	s, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if got := names(s.Tools); !reflect.DeepEqual(got, []string{"gh__search"}) {
		t.Errorf("tools = %v, want only the available backend's", got)
	}
	if !reflect.DeepEqual(s.Unavailable, []string{"down"}) {
		t.Errorf("unavailable = %v, want [down]", s.Unavailable)
	}
	if s.TTLMs != aggregate.DegradedTTLMs {
		t.Errorf("ttlMs = %d, want the degraded %d", s.TTLMs, aggregate.DegradedTTLMs)
	}
}

// A backend that has no prompts says so with "method not found". That is an
// answer, so it neither drops the backend from the listing nor shortens it.
func TestMethodNotFoundIsNotUnavailability(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{"gh": {
		tools:   []string{"search"},
		ttlMs:   60_000,
		missing: map[string]bool{"prompts/list": true, "resources/list": true, "resources/templates/list": true},
	}})
	s, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(s.Unavailable) != 0 {
		t.Errorf("unavailable = %v, want none", s.Unavailable)
	}
	if s.TTLMs != 60_000 {
		t.Errorf("ttlMs = %d, want the backend's own %d", s.TTLMs, 60_000)
	}
	if got := names(s.Tools); !reflect.DeepEqual(got, []string{"gh__search"}) {
		t.Errorf("tools = %v", got)
	}
}

// Task 9.9: calling into a backend that is down says so, and says which one.
func TestCallingAnUnavailableBackend(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{"gh": {fail: errors.New("process exited")}})
	_, err := g.CallTool(t.Context(), "gh__search", nil)
	if !errors.Is(err, aggregate.ErrBackendUnavailable) {
		t.Fatalf("err = %v, want it to wrap %v", err, aggregate.ErrBackendUnavailable)
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("the error does not identify the backend: %v", err)
	}
}

// A backend's own JSON-RPC error is an answer, not unavailability: a client
// told to retry would retry forever.
func TestBackendErrorIsNotUnavailability(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{"gh": {
		fail: fmt.Errorf("%w", &backend.ProtocolError{ServerID: "gh", Code: -32602, Message: "bad arguments"}),
	}})
	_, err := g.CallTool(t.Context(), "gh__search", nil)
	if errors.Is(err, aggregate.ErrBackendUnavailable) {
		t.Errorf("a backend's own error was reported as unavailability: %v", err)
	}
}

// Task 9.10: with nothing answering, a listing is empty rather than failed.
func TestEveryBackendUnavailable(t *testing.T) {
	t.Parallel()
	g, _ := gateway(map[string]*stub{
		"gh":   {fail: errors.New("no process")},
		"docs": {fail: errors.New("no process")},
	})
	s, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface = %v, want empty lists rather than an error", err)
	}
	if len(s.Tools) != 0 || len(s.Prompts) != 0 || len(s.Resources) != 0 || len(s.Templates) != 0 {
		t.Errorf("surface is not empty: %+v", s)
	}
	if !reflect.DeepEqual(s.Unavailable, []string{"docs", "gh"}) {
		t.Errorf("unavailable = %v, want both backends", s.Unavailable)
	}
}

// Task 9.11: two listings of the same backends are identical, so that a client
// comparing them sees a change only when there was one.
func TestListingOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	sources := map[string]*stub{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		sources[id] = &stub{
			tools:     []string{"z", "m", "a"},
			prompts:   []string{"q", "b"},
			resources: []string{"file:///z", "file:///a"},
		}
	}
	g, _ := gateway(sources)
	first, err := g.Surface(t.Context())
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	for range 5 {
		next, err := g.Surface(t.Context())
		if err != nil {
			t.Fatalf("Surface: %v", err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("two listings differ:\n%+v\n%+v", first, next)
		}
	}
}

// A backend that never answers is left out on the listing's own deadline: the
// spec's unavailable backend is silence as much as it is a refusal, and without
// a per-backend bound the slowest one would decide how long every client waits.
func TestAHungBackendDoesNotHoldTheListing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const timeout = 100 * time.Millisecond
		g := aggregate.New(map[string]aggregate.Source{
			"gh":     &stub{tools: []string{"search"}, ttlMs: 60_000},
			"silent": &stub{hang: true},
		}, timeout, nil)

		start := time.Now()
		s, err := g.Surface(t.Context())
		if err != nil {
			t.Fatalf("Surface: %v", err)
		}
		if waited := time.Since(start); waited != timeout {
			t.Errorf("the listing took %v, want the per-backend deadline %v", waited, timeout)
		}
		if got := names(s.Tools); !reflect.DeepEqual(got, []string{"gh__search"}) {
			t.Errorf("tools = %v, want only the backend that answered", got)
		}
		if !reflect.DeepEqual(s.Unavailable, []string{"silent"}) {
			t.Errorf("unavailable = %v, want [silent]", s.Unavailable)
		}
	})
}
