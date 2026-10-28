package keys

import (
	"strings"
	"testing"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

func TestRoundTrip(t *testing.T) {
	for _, s := range config.Services {
		id, plaintext := Generate(s)
		if !strings.HasPrefix(plaintext, s.KeyPrefix()+"_"+id+"_") || len(id) != 12 {
			t.Fatalf("key = %q", plaintext)
		}
		p, ok := Parse(plaintext)
		if !ok || p.Service != s || p.KeyID != id {
			t.Fatalf("parse(%q) = %+v %v", plaintext, p, ok)
		}
		if !Verify(plaintext, Hash(plaintext)) || Verify(plaintext+"x", Hash(plaintext)) {
			t.Fatal("verify")
		}
	}
}

func TestKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		_, k := Generate(config.Barista)
		if seen[k] {
			t.Fatal("duplicate key")
		}
		seen[k] = true
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, k := range []string{"", "nope", "abc_aaaaaaaaaaaa_" + strings.Repeat("x", 43), "bar_AAAAAAAAAAAA_" + strings.Repeat("x", 43), "bar_aaaaaaaaaaaa_short"} {
		if _, ok := Parse(k); ok {
			t.Errorf("parsed %q", k)
		}
	}
}

func TestLookupID(t *testing.T) {
	if LookupID("bar_abc123def456_…") != "abc123def456" || LookupID("abc123def456") != "abc123def456" {
		t.Fatal("LookupID")
	}
}
