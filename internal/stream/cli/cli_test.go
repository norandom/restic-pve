package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// resultLine is the documented shape of the final stderr JSON line.
type resultLine struct {
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	Code          int     `json:"code"`
	Transient     bool    `json:"transient"`
	Message       string  `json:"message"`
	Hint          string  `json:"hint,omitempty"`
	BufferRAMPeak *int64  `json:"buffer_ram_peak,omitempty"`
	PatchCount    *uint64 `json:"patch_count,omitempty"`
}

func invoke(t *testing.T, args ...string) (exitCode, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// lastResult asserts that stderr ends with exactly one well-formed result line
// (strict decoding: no unknown or missing keys) and returns it.
func lastResult(t *testing.T, stderr string) resultLine {
	t.Helper()
	if !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr does not end with a newline: %q", stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	last := lines[len(lines)-1]
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(last), &raw); err != nil {
		t.Fatalf("last stderr line is not JSON: %q (%v)", last, err)
	}
	for _, k := range []string{"type", "status", "code", "transient", "message"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("result line lacks key %q: %s", k, last)
		}
	}
	wantKeys := 5
	if _, ok := raw["buffer_ram_peak"]; ok {
		wantKeys = 7
		if _, ok := raw["patch_count"]; !ok {
			t.Fatal("backup result lacks patch_count")
		}
	}
	if _, ok := raw["hint"]; ok {
		wantKeys++
	}
	if len(raw) != wantKeys {
		t.Fatalf("result line has unexpected keys: %s", last)
	}
	dec := json.NewDecoder(strings.NewReader(last))
	dec.DisallowUnknownFields()
	var r resultLine
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("result line does not match schema: %s (%v)", last, err)
	}
	if r.Type != "result" {
		t.Fatalf("type = %q, want result", r.Type)
	}
	for _, l := range lines[:len(lines)-1] {
		if strings.Contains(l, `"type":"result"`) {
			t.Fatalf("more than one result line in stderr: %q", stderr)
		}
	}
	return r
}

func TestExitCodeTable(t *testing.T) {
	want := map[exitCode]int{
		exitOK:               0,
		exitUsage:            2,
		exitIntegrity:        3,
		exitForbiddenContent: 4,
		exitFleecingInactive: 5,
		exitSourceFailed:     6,
		exitLocalIO:          7,
		exitManifestMismatch: 8,
		exitInternal:         9,
		exitGuestFilesystem:  10,
	}
	for c, v := range want {
		if int(c) != v {
			t.Errorf("exit code %v = %d, want %d", c, int(c), v)
		}
	}
}

func TestWriteResultOK(t *testing.T) {
	var buf bytes.Buffer
	writeResult(&buf, exitOK, false, "done")
	r := lastResult(t, buf.String())
	if r.Status != "ok" || r.Code != 0 || r.Transient || r.Message != "done" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestWriteResultErrorIsSingleLine(t *testing.T) {
	var buf bytes.Buffer
	writeResult(&buf, exitSourceFailed, true, "ssh failed\nsecond line")
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("result spans %d lines: %q", n, buf.String())
	}
	r := lastResult(t, buf.String())
	if r.Status != "error" || r.Code != 6 || !r.Transient || r.Message != "ssh failed\nsecond line" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestNoArgsIsUsage(t *testing.T) {
	code, stdout, stderr := invoke(t)
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("usage not printed: %q", stderr)
	}
	r := lastResult(t, stderr)
	if r.Status != "error" || r.Code != 2 {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestUnknownSubcommandIsUsage(t *testing.T) {
	code, _, stderr := invoke(t, "frobnicate")
	if code != exitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "frobnicate") {
		t.Fatalf("usage/unknown name not printed: %q", stderr)
	}
	for _, sub := range []string{"backup", "verify", "to-vma", "to-raw"} {
		if !strings.Contains(stderr, sub) {
			t.Errorf("usage does not list %q", sub)
		}
	}
	r := lastResult(t, stderr)
	if r.Status != "error" || r.Code != 2 || r.Transient {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, a := range []string{"help", "-h", "--help"} {
		code, _, stderr := invoke(t, a)
		if code != exitOK {
			t.Errorf("%s: exit = %d, want 0", a, code)
		}
		if !strings.Contains(stderr, "Usage:") {
			t.Errorf("%s: usage not printed", a)
		}
	}
}

func TestSubcommandUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"unknown flag":         {"verify", "--bogus"},
		"positional argument":  {"verify", "extra"},
		"vmid not decimal":     {"backup", "--ssh-config", "c", "--ssh-host", "h", "--vmid", "10a", "--manifest-out", "m"},
		"vmid signed":          {"backup", "--ssh-config", "c", "--ssh-host", "h", "--vmid", "+100", "--manifest-out", "m"},
		"vmid zero":            {"backup", "--ssh-config", "c", "--ssh-host", "h", "--vmid", "0", "--manifest-out", "m"},
		"vmid missing":         {"backup", "--ssh-config", "c", "--ssh-host", "h", "--manifest-out", "m"},
		"ssh host missing":     {"backup", "--ssh-config", "c", "--vmid", "100", "--manifest-out", "m"},
		"manifest-out missing": {"backup", "--input", "x", "--vmid", "100"},
		"bad interval":         {"backup", "--input", "x", "--vmid", "100", "--manifest-out", "m", "--progress-interval", "0s"},
		"removed disk-gib":     {"backup", "--disk-gib", "1"},
		"removed spill-dir":    {"backup", "--spill-dir", "s"},
		"removed fleecing":     {"backup", "--input", "x", "--vmid", "100", "--manifest-out", "m", "--fleecing", "off"},
		"to-raw dir missing":   {"to-raw"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout must stay empty, got %q", stdout)
			}
			r := lastResult(t, stderr)
			if r.Status != "error" || r.Code != 2 {
				t.Fatalf("unexpected result: %+v", r)
			}
		})
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	code, _, stderr := invoke(t, "backup", "-h")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "-vmid") {
		t.Fatalf("flag help not printed: %q", stderr)
	}
}
