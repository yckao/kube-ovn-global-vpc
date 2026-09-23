package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDomainRegistrationRejectsInvalidAuthorityLabel(t *testing.T) {
	for _, domain := range []string{"", "UpperCase", "contains.dots", strings.Repeat("x", 64)} {
		if err := (Config{Domain: domain}).Validate(); err == nil {
			t.Fatalf("accepted invalid domain %q", domain)
		}
	}
}
func TestLoadRejectsMultipleOrMalformedJSONDocuments(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "domain.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"{}", "null", "garbage"} {
		p := filepath.Join(t.TempDir(), "domain.json")
		if err = os.WriteFile(p, append(append([]byte{}, sample...), []byte(suffix)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = Load(p); err == nil {
			t.Fatal("accepted trailing content")
		}
	}
	p := filepath.Join(t.TempDir(), "domain.json")
	if err = os.WriteFile(p, sample, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(p); err != nil {
		t.Fatalf("example configuration shape invalid: %v", err)
	}
}
