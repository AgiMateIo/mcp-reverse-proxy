package aggregate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
)

// DegradedTTLMs is the cache lifetime of a listing assembled while a backend
// was unavailable. It is short on purpose: the answer is true but incomplete,
// and the client should come back for the rest rather than hold a partial
// surface for the ordinary minute.
const DegradedTTLMs = 5_000

// A Listing is what one backend contributed to the surface.
//
// Err carries the backend's failure rather than aborting the assembly: one
// backend being down must not decide the whole listing, so its entries are left
// out and its identifier recorded.
type Listing struct {
	Backend   string
	Tools     []backend.Tool
	Prompts   []backend.Prompt
	Resources []backend.Resource
	Templates []backend.ResourceTemplate
	// TTLMs is the lifetime this backend offered, zero if it offered none.
	TTLMs int
	Err   error
}

// A Surface is what one client sees: the entries of every backend under
// gateway names and gateway URIs.
type Surface struct {
	Tools     []backend.Tool
	Prompts   []backend.Prompt
	Resources []backend.Resource
	Templates []backend.ResourceTemplate
	// TTLMs is the lifetime of the assembled listing.
	TTLMs int
	// Unavailable names the backends that did not answer, in configuration
	// order.
	Unavailable []string
}

// Assemble folds the backends' listings into one surface.
//
// The order of the result is fixed by backend identifier and then by name, not
// by whichever backend answered first: a client comparing two listings to
// notice a change would otherwise see one on every call.
func Assemble(listings []Listing) (Surface, error) {
	ordered := slices.Clone(listings)
	slices.SortFunc(ordered, func(a, b Listing) int { return strings.Compare(a.Backend, b.Backend) })

	var s Surface
	ttl := 0
	// owners maps a resulting name to the backend that produced it, per kind:
	// a tool and a prompt of the same name are two namespaces, not a clash.
	owners := map[string]map[string]string{}
	claim := func(kind, name, backendID string) error {
		if len(name) > MaxNameBytes {
			return fmt.Errorf("backend %q: %s %q: %w of %d bytes (it is %d)",
				backendID, kind, name, ErrNameTooLong, MaxNameBytes, len(name))
		}
		if owners[kind] == nil {
			owners[kind] = map[string]string{}
		}
		if first, taken := owners[kind][name]; taken {
			return fmt.Errorf("backends %q and %q both produce the %s name %q: %w",
				first, backendID, kind, name, ErrNameCollision)
		}
		owners[kind][name] = backendID
		return nil
	}

	for _, l := range ordered {
		if l.Err != nil {
			s.Unavailable = append(s.Unavailable, l.Backend)
			continue
		}
		if l.TTLMs > 0 && (ttl == 0 || l.TTLMs < ttl) {
			// The shortest offer wins: the assembled listing is only as fresh
			// as its most impatient contributor.
			ttl = l.TTLMs
		}
		for _, t := range sortedByName(l.Tools, func(t backend.Tool) string { return t.Name }) {
			t.Name = Qualify(l.Backend, t.Name)
			if err := claim("tool", t.Name, l.Backend); err != nil {
				return Surface{}, err
			}
			s.Tools = append(s.Tools, t)
		}
		for _, p := range sortedByName(l.Prompts, func(p backend.Prompt) string { return p.Name }) {
			p.Name = Qualify(l.Backend, p.Name)
			if err := claim("prompt", p.Name, l.Backend); err != nil {
				return Surface{}, err
			}
			s.Prompts = append(s.Prompts, p)
		}
		for _, t := range sortedByName(l.Templates, func(t backend.ResourceTemplate) string { return t.Name }) {
			t.Name = Qualify(l.Backend, t.Name)
			if err := claim("resource template", t.Name, l.Backend); err != nil {
				return Surface{}, err
			}
			t.URITemplate = WrapURITemplate(l.Backend, t.URITemplate)
			s.Templates = append(s.Templates, t)
		}
		for _, r := range sortedByName(l.Resources, func(r backend.Resource) string { return r.URI }) {
			// Resources are not namespaced by name: their identity is the URI,
			// and two backends offering the same URI stay distinct because the
			// wrapping carries the backend.
			r.URI = WrapURI(l.Backend, r.URI)
			r.Name = Qualify(l.Backend, r.Name)
			s.Resources = append(s.Resources, r)
		}
	}
	s.TTLMs = ttl
	if len(s.Unavailable) > 0 {
		s.TTLMs = DegradedTTLMs
	}
	return s, nil
}

// sortedByName copies a backend's entries into a fixed order. The backend's own
// order is not to be trusted for stability: nothing in the protocol promises it
// is the same from one call to the next.
func sortedByName[T any](entries []T, name func(T) string) []T {
	out := slices.Clone(entries)
	slices.SortStableFunc(out, func(a, b T) int { return strings.Compare(name(a), name(b)) })
	return out
}
