package aggregate

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Scheme is the scheme of a gateway resource URI.
const Scheme = "mcp-proxy"

// ErrBadURI reports a URI that is not a gateway resource URI.
var ErrBadURI = errors.New("not a gateway resource URI")

// WrapURI turns one backend's resource URI into the gateway's.
//
// A resource URI cannot be prefixed the way a name is: it has a structure of
// its own, starting with a scheme. So the original is carried whole inside the
// gateway's URI, percent-encoded. The mapping is reversible without a table,
// which is what lets it survive a restart of the gateway.
func WrapURI(backendID, uri string) string {
	return Scheme + "://" + backendID + "/" + escape(uri)
}

// UnwrapURI recovers the backend and its own URI from a gateway URI.
func UnwrapURI(gateway string) (backendID, uri string, err error) {
	u, err := url.Parse(gateway)
	if err != nil {
		return "", "", fmt.Errorf("%q: %w", gateway, ErrBadURI)
	}
	if u.Scheme != Scheme || u.Host == "" {
		return "", "", fmt.Errorf("%q: %w: expected %s://<backend>/<encoded uri>", gateway, ErrBadURI, Scheme)
	}
	// EscapedPath is the raw text: url.Parse has already decoded Path, and
	// decoding it a second time would turn a %2520 in the backend's own URI
	// into a space.
	encoded := strings.TrimPrefix(u.EscapedPath(), "/")
	if encoded == "" {
		return "", "", fmt.Errorf("%q: %w: no resource URI", gateway, ErrBadURI)
	}
	inner, err := url.PathUnescape(encoded)
	if err != nil {
		return "", "", fmt.Errorf("%q: %w: %w", gateway, ErrBadURI, err)
	}
	return u.Host, inner, nil
}

// WrapURITemplate wraps a resource template's URI template.
//
// The expressions are left standing: a template is expanded by the client under
// RFC 6570, and percent-encoding a `{var}` would leave it with a literal string
// where a variable belongs. Only the literal text around them is encoded, so
// the result of an expansion is a gateway URI that unwraps to what the backend
// would have expanded the original to.
func WrapURITemplate(backendID, template string) string {
	var b strings.Builder
	b.WriteString(Scheme + "://" + backendID + "/")
	for rest := template; rest != ""; {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(escape(rest))
			break
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			// An unterminated expression is not a template the gateway can
			// reason about; carrying it through encoded keeps it visible.
			b.WriteString(escape(rest))
			break
		}
		b.WriteString(escape(rest[:open]))
		b.WriteString(rest[open : open+end+1])
		rest = rest[open+end+1:]
	}
	return b.String()
}

// escape percent-encodes everything outside the unreserved set of RFC 3986.
//
// url.PathEscape is not enough: it leaves `:` and `@` standing, so a
// `file:///readme.md` would keep its colon and the gateway URI would carry two
// schemes.
func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		if c := s[i]; unreserved(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func unreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-' || c == '.' || c == '_' || c == '~':
		return true
	}
	return false
}
