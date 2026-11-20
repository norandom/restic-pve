package testfix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResticMissingIsASkip(t *testing.T) {
	t.Setenv(EnvRestic, filepath.Join(t.TempDir(), "no-such-restic"))
	_, missing, err := findRestic()
	if err != nil || !strings.Contains(missing, "not found") || !strings.Contains(missing, EnvRestic) {
		t.Fatalf("missing=%q err=%v", missing, err)
	}
}

func TestResticOtherBinaryFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho restic 0.19.1 compiled\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvRestic, p)
	_, missing, err := findRestic()
	if missing != "" || err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("missing=%q err=%v, want a checksum failure", missing, err)
	}
}

func TestPerlExplicitBrokenFails(t *testing.T) {
	t.Setenv(EnvPerl, filepath.Join(t.TempDir(), "no-such-perl"))
	_, missing, err := findPerl()
	if missing != "" || err == nil {
		t.Fatalf("missing=%q err=%v, want a failure for an explicit perl that does not run", missing, err)
	}
}

// TestPinnedRestic runs only where the pinned binary is present.
func TestPinnedRestic(t *testing.T) {
	r := NewRepo(t)
	out, err := r.Run("snapshots", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("new repository lists %s", out)
	}
}
