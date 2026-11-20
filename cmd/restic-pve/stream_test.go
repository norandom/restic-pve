package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/role"
	"github.com/norandom/restic-pve/internal/stream/cli"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// guestVMA writes a neutral two-disk guest stream (VMID 100) and returns its path.
func guestVMA(t *testing.T, tamper bool) string {
	t.Helper()
	devs := []vmatest.Device{vmatest.EFIDisk(1, 256*1024, 21), vmatest.DataDisk(2, "scsi0", 12<<20+1000, 22)}
	b := &vmatest.Builder{UUID: [16]byte{1, 2, 3}, CTime: 1760000000, Devices: devs,
		Configs: []vma.ConfigBlob{vmatest.ExampleConfig()}, Order: vmatest.InterleavedOrder(devs)}
	if tamper {
		b.Tamper = vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 1}
	}
	res, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "guest.vma")
	if err := os.WriteFile(p, res.Stream, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type outcome struct {
	code           int
	stdout, stderr string
}

// lastLine is the final stderr line: the result line of the stream contract.
func (o outcome) lastLine() string {
	l := strings.Split(strings.TrimSuffix(o.stderr, "\n"), "\n")
	return l[len(l)-1]
}

// sameVMA reports whether two VMA streams are equal apart from what every to-vma run makes new:
// the uuid (header and extents), the creation time and the md5 sums over them.
func sameVMA(t *testing.T, a, b string) bool {
	t.Helper()
	norm := func(s string) []byte {
		v := []byte(s)
		l, err := vmatest.Inspect(v)
		if err != nil {
			t.Fatalf("to-vma output: %v", err)
		}
		clear(v[vma.OffUUID : vma.OffMD5+16]) // uuid, ctime, md5
		for _, e := range l.Extents {
			clear(v[e.Offset+vma.OffExtMD5 : e.Offset+vma.OffExtMD5+16])
			clear(v[e.Offset+vma.OffExtUUID : e.Offset+vma.OffExtUUID+16])
		}
		return v
	}
	return len(a) > 0 && bytes.Equal(norm(a), norm(b))
}

// sameDir requires two folders with the same files and the same bytes.
func sameDir(t *testing.T, a, b string) {
	t.Helper()
	ents, err := os.ReadDir(a)
	if err != nil || len(ents) == 0 {
		t.Fatalf("%s: %v", a, err)
	}
	if other, _ := os.ReadDir(b); len(other) != len(ents) {
		t.Fatalf("%s and %s hold %d and %d files", a, b, len(ents), len(other))
	}
	for _, e := range ents {
		x, _ := os.ReadFile(filepath.Join(a, e.Name()))
		y, err := os.ReadFile(filepath.Join(b, e.Name()))
		if err != nil || !bytes.Equal(x, y) {
			t.Errorf("%s differs: %v", e.Name(), err)
		}
	}
}

// both runs `restic-pve stream <args>` through the command table and the stream program's own
// entry, and requires the same exit code, stdout bytes and result line. The stdout of to-vma is
// compared without the fresh uuid and time of each run.
func both(t *testing.T, stdin []byte, args ...string) outcome {
	t.Helper()
	var out, errb bytes.Buffer
	e := sys(role.Backup, &out, &errb)
	e.stdin = bytes.NewReader(stdin)
	got := outcome{run(append([]string{"stream"}, args...), e), out.String(), errb.String()}
	out.Reset()
	errb.Reset()
	refArgs := append([]string(nil), args...)
	for i := 0; i+1 < len(refArgs); i++ { // to-raw: the reference writes to its own folder, compared below
		if refArgs[i] == "--dir" {
			refArgs[i+1] += ".ref"
			defer sameDir(t, args[i+1], refArgs[i+1])
		}
	}
	ref := outcome{cli.Main(refArgs, bytes.NewReader(stdin), &out, &errb), out.String(), errb.String()}
	same := got.stdout == ref.stdout || len(args) > 0 && args[0] == "to-vma" && sameVMA(t, got.stdout, ref.stdout)
	if got.code != ref.code || !same || got.lastLine() != ref.lastLine() {
		t.Fatalf("%q differs from the stream program: %d %q / %d %q", args, got.code, got.lastLine(), ref.code, ref.lastLine())
	}
	if !strings.HasPrefix(got.lastLine(), `{"type":"result",`) {
		t.Fatalf("%q: no result line: %q", args, got.stderr)
	}
	return got
}

// TestStreamRoundTrip: backup, verify, to-vma and to-raw through `restic-pve stream`, with the
// exit codes and result lines of the stream contract.
func TestStreamRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := guestVMA(t, false)
	m := filepath.Join(dir, "m.json")
	b := both(t, nil, "backup", "--input", in, "--vmid", "100", "--manifest-out", m)
	if b.code != 0 || len(b.stdout) == 0 || !strings.Contains(b.lastLine(), `"status":"ok"`) {
		t.Fatalf("backup: %d %q", b.code, b.stderr)
	}
	rvs := []byte(b.stdout)
	if v := both(t, rvs, "verify", "--manifest", m); v.code != 0 || v.stdout != "" || v.lastLine() != `{"type":"result","status":"ok","code":0,"transient":false,"message":"verify: ok"}` {
		t.Fatalf("verify: %d %q", v.code, v.stderr)
	}
	vm := both(t, rvs, "to-vma", "--manifest", m)
	ref, _ := os.ReadFile(in)
	if vm.code != 0 || len(vm.stdout) != len(ref) {
		t.Fatalf("to-vma: %d %d bytes, the input has %d", vm.code, len(vm.stdout), len(ref))
	}
	if r := both(t, rvs, "to-raw", "--manifest", m, "--dir", filepath.Join(t.TempDir(), "raw")); r.code != 0 {
		t.Fatalf("to-raw: %d %q", r.code, r.stderr)
	}
	// the error classes keep their codes: integrity 3, manifest mismatch 8, usage 2
	if c := both(t, nil, "backup", "--input", guestVMA(t, true), "--vmid", "100", "--manifest-out", filepath.Join(dir, "t.json")); c.code != 3 {
		t.Errorf("tampered stream: %d", c.code)
	}
	if c := both(t, rvs, "verify", "--manifest", filepath.Join(dir, "missing.json")); c.code != 7 {
		t.Errorf("missing manifest: %d", c.code)
	}
	if c := both(t, rvs[:len(rvs)/2], "verify"); c.code != 3 {
		t.Errorf("cut stream: %d", c.code)
	}
	other := filepath.Join(dir, "other.json")
	if c := both(t, nil, "backup", "--input", guestVMA(t, false), "--vmid", "101", "--manifest-out", other); c.code != 0 {
		t.Fatal(c.code)
	}
	mm, _ := os.ReadFile(other)
	_ = os.WriteFile(other, bytes.Replace(mm, []byte(`"size":262144`), []byte(`"size":131072`), 1), 0o644)
	if c := both(t, rvs, "verify", "--manifest", other); c.code != 8 {
		t.Errorf("manifest mismatch: %d %q", c.code, c.stderr)
	}
	for _, args := range [][]string{nil, {"watch"}, {"verify", "--bogus"}} {
		if c := both(t, nil, args...); c.code != 2 {
			t.Errorf("%q: %d", args, c.code)
		}
	}
}

// TestStreamDropsSecrets: the ssh child of `restic-pve stream backup` gets neither the
// repository password nor any other secret of the environment; the rest stays.
func TestStreamDropsSecrets(t *testing.T) {
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env")
	fake := "#!/bin/sh\nenv > \"$FAKE_ENV_OUT\"\ncat \"$FAKE_VMA_IN\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ENV_OUT", envOut)
	t.Setenv("FAKE_VMA_IN", guestVMA(t, false))
	t.Setenv("KEEP_ME", "1")
	secrets := []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND", "RCLONE_CONFIG_PASS",
		"OPENOBSERVE_PASSWORD", "GDRIVE_CLIENT_SECRET", "DISCORD_WEBHOOK_URL", "RUNNER_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "B2_ACCOUNT_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}
	for _, k := range secrets {
		t.Setenv(k, "not-for-the-child")
	}
	var out, errb bytes.Buffer
	code := run([]string{"stream", "backup", "--ssh-config", filepath.Join(dir, "ssh_config"), "--ssh-host", "pve.example",
		"--vmid", "100", "--manifest-out", filepath.Join(dir, "m.json")}, sys(role.Backup, &out, &errb))
	if code != 0 {
		t.Fatalf("backup over the fake ssh: %d %q", code, errb.String())
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range secrets {
		if bytes.Contains(got, []byte(k+"=")) || os.Getenv(k) != "" {
			t.Errorf("%s reached the ssh child or stayed in the environment", k)
		}
	}
	if bytes.Contains(got, []byte("not-for-the-child")) {
		t.Error("a secret value reached the ssh child")
	}
	if !bytes.Contains(got, []byte("KEEP_ME=1")) || !bytes.Contains(got, []byte("PATH=")) {
		t.Errorf("the child lost its ordinary environment: %s", got)
	}
}
