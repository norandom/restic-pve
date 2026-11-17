package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type release struct {
	files map[string][]byte
	priv  ed25519.PrivateKey
	pub   string
}

const oldBinary = "#!/bin/sh\necho v1.2.0\n"

func newRelease(t *testing.T, version, reports string) *release {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{files: map[string][]byte{
		"VERSION":                []byte(version + "\n"),
		"restic-pve-linux-amd64": []byte("#!/bin/sh\necho " + reports + "\n"),
	}, priv: priv, pub: base64.StdEncoding.EncodeToString(pub)}
	r.sign()
	return r
}

// sign writes SHA256SUMS over the current files and signs it.
func (r *release) sign() {
	var b strings.Builder
	for _, name := range []string{"VERSION", "restic-pve-linux-amd64"} {
		sum := sha256.Sum256(r.files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	r.files["SHA256SUMS"] = []byte(b.String())
	r.files["SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.priv, r.files["SHA256SUMS"])) + "\n")
}

type outcome struct {
	code           int
	stdout, stderr string
	target         []byte
	mode           os.FileMode
	leftovers      int
}

func (r *release) run(t *testing.T, mod func(*Config)) outcome {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, ok := r.files[strings.TrimPrefix(req.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "restic-pve")
	if err := os.WriteFile(target, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	c := Config{BaseURL: srv.URL + "/download/", Target: target, CurrentVersion: "v1.2.0", Keys: []string{r.pub},
		Arch: "amd64", HTTPClient: srv.Client(), IsRoot: func() bool { return true }, Stdout: &out, Stderr: &errb}
	if mod != nil {
		mod(&c)
	}
	code := Run(c)
	got, _ := os.ReadFile(target)
	st, _ := os.Stat(target)
	ents, _ := os.ReadDir(dir)
	return outcome{code, out.String(), errb.String(), got, st.Mode().Perm(), len(ents) - 1}
}

func TestUpdateSuccess(t *testing.T) {
	r := newRelease(t, "v1.10.0", "v1.10.0")
	for _, cur := range []string{"v1.2.0", "v1.9.99", "dev", "garbage"} {
		o := r.run(t, func(c *Config) { c.CurrentVersion = cur })
		if o.code != 0 || o.stdout != "restic-pve updated from "+cur+" to v1.10.0\n" {
			t.Fatalf("%s: %+v", cur, o)
		}
		if !bytes.Equal(o.target, r.files["restic-pve-linux-amd64"]) || o.mode != 0o755 || o.leftovers != 0 {
			t.Fatalf("%s: target %q mode %o leftovers %d", cur, o.target, o.mode, o.leftovers)
		}
	}
}

func TestUpdateSameVersion(t *testing.T) {
	o := newRelease(t, "v1.2.0", "v1.2.0").run(t, nil)
	if o.code != 0 || o.stdout != "restic-pve v1.2.0 is up to date\n" || string(o.target) != oldBinary {
		t.Fatalf("%+v", o)
	}
}

// Every refusal exits 1 with one line and leaves the installed binary byte-identical.
func TestUpdateRefusals(t *testing.T) {
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	cases := []struct {
		label, want string
		setup       func(r *release, c *Config)
	}{
		{"not root", "must run as root", func(r *release, c *Config) { c.IsRoot = func() bool { return false } }},
		{"empty key list", "no release keys in this build", func(r *release, c *Config) { c.Keys = nil }},
		{"unknown arch", "architecture", func(r *release, c *Config) { c.Arch = "riscv64" }},
		{"bad signature", "signature", func(r *release, c *Config) { r.files["SHA256SUMS.sig"] = []byte("AAAA\n") }},
		{"unknown key", "does not match any release key", func(r *release, c *Config) {
			r.files["SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(otherPriv, r.files["SHA256SUMS"])))
		}},
		{"tampered sums", "does not match any release key", func(r *release, c *Config) {
			r.files["SHA256SUMS"] = append(r.files["SHA256SUMS"], []byte(strings.Repeat("0", 64)+"  extra\n")...)
		}},
		{"binary hash", "does not match SHA256SUMS", func(r *release, c *Config) {
			r.files["restic-pve-linux-amd64"] = []byte("#!/bin/sh\necho v1.3.0 \n")
		}},
		{"version hash", "VERSION does not match SHA256SUMS", func(r *release, c *Config) { r.files["VERSION"] = []byte("v1.3.1\n") }},
		{"binary disagrees", `reports version "v1.2.9"`, func(r *release, c *Config) {
			r.files["restic-pve-linux-amd64"] = []byte("#!/bin/sh\necho v1.2.9\n")
			r.sign()
		}},
		{"binary fails", "failed to run", func(r *release, c *Config) {
			r.files["restic-pve-linux-amd64"] = []byte("#!/bin/sh\nexit 3\n")
			r.sign()
		}},
		{"bad VERSION", "vMAJOR.MINOR.PATCH", func(r *release, c *Config) { r.files["VERSION"] = []byte("1.3.0\n"); r.sign() }},
		{"older", "older than the installed v1.2.0", func(r *release, c *Config) { r.files["VERSION"] = []byte("v1.1.9\n"); r.sign() }},
		{"oversized", "larger than 65536 bytes", func(r *release, c *Config) {
			r.files["SHA256SUMS"] = bytes.Repeat([]byte("x"), 64<<10+1)
		}},
		{"missing asset", "HTTP 404", func(r *release, c *Config) { delete(r.files, "restic-pve-linux-amd64") }},
		{"missing sums line", "lists no restic-pve-linux-arm64", func(r *release, c *Config) { c.Arch = "arm64" }},
		{"malformed sums", "SHA256SUMS line 1", func(r *release, c *Config) {
			r.files["SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  ../VERSION\n")
			r.files["SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.priv, r.files["SHA256SUMS"])))
		}},
	}
	for _, tc := range cases {
		r := newRelease(t, "v1.3.0", "v1.3.0")
		o := r.run(t, func(c *Config) { tc.setup(r, c) })
		if o.code != 1 || !strings.Contains(o.stderr, tc.want) || strings.Count(o.stderr, "\n") != 1 || o.stdout != "" {
			t.Errorf("%s: %+v", tc.label, o)
		}
		if string(o.target) != oldBinary || o.mode != 0o755 || o.leftovers != 0 {
			t.Errorf("%s: target changed (%q, %o, %d leftovers)", tc.label, o.target, o.mode, o.leftovers)
		}
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for _, bad := range []string{"", h + "  a", h + " a\n", h + "  a/b\n", h + "  a\n" + h + "  a\n", strings.ToUpper(h) + "  a\n", h[:63] + "  a\n"} {
		if _, err := parseSums([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if m, err := parseSums([]byte(h + "  VERSION\n")); err != nil || m["VERSION"] != h {
		t.Fatal(m, err)
	}
}
