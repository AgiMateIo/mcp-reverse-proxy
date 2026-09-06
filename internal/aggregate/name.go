package aggregate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Separator joins a backend identifier to a name the backend chose. Two
// underscores rather than one because a single one is ordinary inside tool
// names, and a separator that occurs in the names it separates cannot be split
// back apart.
const Separator = "__"

// MaxNameBytes is the longest name a tool may carry. The limit is the
// protocol's, not the gateway's, which is why a prefix that pushes a name past
// it is a configuration problem rather than something to truncate.
const MaxNameBytes = 128

var (
	// ErrNotFound reports a name no backend owns. It covers a prefix matching
	// no backend and a name carrying no prefix at all: both are the client
	// naming something that does not exist, and neither is a reason to ask
	// every backend in turn.
	ErrNotFound = errors.New("no such tool")
	// ErrUnknownBackend reports a well-formed gateway URI whose backend is not
	// in the resolved configuration.
	ErrUnknownBackend = errors.New("no such backend")
	// ErrNameCollision reports two backends whose namespaced names coincide.
	// The gateway refuses the surface rather than picking a winner: a silent
	// choice would route a client's call to a backend it never named.
	ErrNameCollision = errors.New("namespaced names collide")
	// ErrNameTooLong reports a namespaced name past [MaxNameBytes].
	ErrNameTooLong = errors.New("namespaced name exceeds the length limit")
)

// Qualify returns the name a client sees for one backend's name.
func Qualify(backendID, name string) string {
	return backendID + Separator + name
}

// A Router resolves a namespaced name back to the backend that owns it.
//
// It is built from the resolved configuration rather than from a listing, so
// that a call to a backend that is currently down still routes: the client is
// then told that backend is unavailable, which is true, instead of that the
// tool does not exist, which is not.
type Router struct {
	// ids are ordered longest first, so that a backend named "a__b" wins over
	// one named "a" for the name "a__b__c". Identifiers containing the
	// separator are legal configuration, and the longer match is the one that
	// cannot be reached any other way.
	ids []string
}

// NewRouter returns a router over the identifiers of the resolved
// configuration.
func NewRouter(ids []string) *Router {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return &Router{ids: sorted}
}

// Route splits a namespaced name into the backend that owns it and the name
// that backend knows it by.
func (r *Router) Route(qualified string) (backendID, name string, err error) {
	for _, id := range r.ids {
		if rest, ok := strings.CutPrefix(qualified, id+Separator); ok && rest != "" {
			return id, rest, nil
		}
	}
	// Deliberately the same answer for an unknown prefix and for no prefix at
	// all. The alternative — trying the name on every backend — would turn one
	// client's typo into a request against every process the subject owns.
	return "", "", fmt.Errorf("%q: %w", qualified, ErrNotFound)
}

// Has reports whether the router knows a backend.
func (r *Router) Has(backendID string) bool { return slices.Contains(r.ids, backendID) }
