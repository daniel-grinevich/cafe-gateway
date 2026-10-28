// Package keys defines the API key format.
//
// A key looks like bar_<key_id>_<secret> (barista) or grd_<key_id>_<secret> (grinder). The prefix says which service
// it works on, key_id is a public lookup id stored in clear, and only the sha256 of the whole key is stored, so a
// leaked database can't be replayed.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

var keyRE = regexp.MustCompile(`^(bar|grd)_([a-z0-9]{12})_([A-Za-z0-9_-]{32,64})$`)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

type Parsed struct {
	Service config.Service
	KeyID   string
}

func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Verify compares in constant time.
func Verify(plaintext, keyHash string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(plaintext)), []byte(keyHash)) == 1
}

// Generate returns (key_id, plaintext). The plaintext is shown once and never stored.
func Generate(service config.Service) (string, string) {
	id := make([]byte, 12)
	for i := range id {
		id[i] = idAlphabet[randIndex(len(idAlphabet))]
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never returns an error
	keyID := string(id)
	return keyID, service.KeyPrefix() + "_" + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret)
}

// randIndex returns a uniform index in [0, n) for n <= 256, using rejection sampling.
func randIndex(n int) int {
	limit := 256 - 256%n
	b := make([]byte, 1)
	for {
		_, _ = rand.Read(b)
		if int(b[0]) < limit {
			return int(b[0]) % n
		}
	}
}

func Parse(plaintext string) (Parsed, bool) {
	m := keyRE.FindStringSubmatch(plaintext)
	if m == nil {
		return Parsed{}, false
	}
	service := config.Barista
	if m[1] == "grd" {
		service = config.Grinder
	}
	return Parsed{Service: service, KeyID: m[2]}, true
}

// LookupID accepts a bare key_id or a pasted display prefix like "bar_abc123def456_…".
func LookupID(s string) string {
	parts := strings.Split(strings.TrimSpace(s), "_")
	if len(parts) >= 2 && (parts[0] == "bar" || parts[0] == "grd") {
		return parts[1]
	}
	return strings.TrimSpace(s)
}

// DisplayPrefix is what listings show instead of the key.
func DisplayPrefix(service config.Service, keyID string) string {
	return service.KeyPrefix() + "_" + keyID + "_…"
}
