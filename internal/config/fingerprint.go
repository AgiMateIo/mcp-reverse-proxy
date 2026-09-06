package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
)

// A Fingerprinter reduces a resolved server configuration to an opaque
// identifier, which the pool uses together with the subject as a process key.
//
// The digest is keyed with a random key generated at construction, so it is
// irreversible in practice and not only in principle: an environment variable
// holding a short or guessable secret cannot be recovered from a fingerprint
// that reaches a log line or a metric label. The key never leaves the process,
// and neither does the pool, so fingerprints are deliberately not stable across
// restarts.
//
// A gateway therefore holds exactly one Fingerprinter for its lifetime.
// Constructing a second one — per request, say — would give the same
// configuration two different fingerprints, and the pool would miss and spawn a
// duplicate process for every request.
type Fingerprinter struct {
	key []byte
}

// NewFingerprinter returns a Fingerprinter with a fresh random key.
func NewFingerprinter() (*Fingerprinter, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate fingerprint key: %w", err)
	}
	return &Fingerprinter{key: key}, nil
}

// Server returns the fingerprint of one resolved server configuration.
//
// Every field that changes what process would be started takes part, including
// environment values: two subjects whose configurations differ only by a token
// must not share a process.
func (f *Fingerprinter) Server(s Server) string {
	mac := hmac.New(sha256.New, f.key)
	// Fields are length-prefixed so that no regrouping of the same bytes —
	// args ["ab","c"] against ["a","bc"] — collides.
	writeField(mac, s.ID)
	writeField(mac, s.Command)
	writeCount(mac, len(s.Args))
	for _, a := range s.Args {
		writeField(mac, a)
	}
	keys := s.Env.Keys()
	writeCount(mac, len(keys))
	for _, k := range keys {
		writeField(mac, k)
		writeField(mac, s.Env[k].Reveal())
	}
	writeField(mac, string(s.Era))
	return hex.EncodeToString(mac.Sum(nil))
}

func writeField(h hash.Hash, s string) {
	writeCount(h, len(s))
	// hash.Hash never reports a write error.
	_, _ = h.Write([]byte(s))
}

func writeCount(h hash.Hash, n int) {
	var buf [8]byte
	// G115: every caller passes a len(), which is never negative, so the
	// conversion cannot wrap.
	binary.BigEndian.PutUint64(buf[:], uint64(n)) //nolint:gosec // see above
	_, _ = h.Write(buf[:])
}
