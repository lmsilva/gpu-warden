package slurmcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenFromEnv is the one-shot case: no file, token fixed for the life of
// the process.
func TestTokenFromEnv(t *testing.T) {
	c := Config{env: "abc123"}
	got, err := c.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc123" {
		t.Errorf("token = %q, want abc123", got)
	}
}

// TestTokenFileWins: a mounted file takes precedence, because it is the only
// source that can change under a running process.
func TestTokenFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{env: "from-env", TokenFile: path}
	got, err := c.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "from-file" {
		t.Errorf("token = %q, want from-file", got)
	}
}

// TestTokenFileIsRereadEachCall is the whole reason the token is a function.
// A server holding a value read at startup cannot survive its rotation, and
// nothing outside the process can change its environment.
func TestTokenFileIsRereadEachCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{TokenFile: path}
	if got, _ := c.Token(); got != "first" {
		t.Fatalf("token = %q, want first", got)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Token(); got != "second" {
		t.Errorf("a rotated token must be picked up without a restart, got %q", got)
	}
}

// TestTokenTrimsWhitespace: every tool that writes a token adds a trailing
// newline, and an HTTP header carrying one is rejected.
func TestTokenTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  padded\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (Config{TokenFile: path}).Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "padded" {
		t.Errorf("token = %q, want padded", got)
	}
}

// TestMissingTokenSaysSo covers the three ways there is no usable token. Each
// must produce a sentence: an empty header reaches slurmrestd as a 511, which
// reads like a permissions problem rather than a missing credential.
func TestMissingTokenSaysSo(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"nothing set at all", Config{}, "no Slurm token"},
		{"file does not exist", Config{TokenFile: filepath.Join(dir, "nope")}, "reading"},
		{"file is empty", Config{TokenFile: empty}, "is empty"},
	}
	for _, tc := range cases {
		got, err := tc.cfg.Token()
		if err == nil {
			t.Errorf("%s: expected an error, got token %q", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}
