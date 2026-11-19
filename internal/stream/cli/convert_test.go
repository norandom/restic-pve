package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// guestStream is a backup of testGuest: the RVS1 stream, its manifest path
// and the builder (device contents).
type guestStream struct {
	rvs      []byte
	manifest string
	b        *vmatest.Builder
}

func backupGuest(t *testing.T, extra ...string) guestStream {
	t.Helper()
	b := testGuest()
	r := runBackup(t, nil, append([]string{"--input", writeVMA(t, b)}, extra...)...)
	assertResult(t, r, exitOK, false)
	return guestStream{rvs: r.stdout, manifest: r.manifest, b: b}
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type cliRun struct {
	code   exitCode
	stdout []byte
	stderr string
}

func runWith(t *testing.T, stdin []byte, args ...string) cliRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, bytes.NewReader(stdin), &out, &errb)
	return cliRun{code: code, stdout: out.Bytes(), stderr: errb.String()}
}

func (r cliRun) expect(t *testing.T, code exitCode) resultLine {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", r.code, code, r.stderr)
	}
	res := lastResult(t, r.stderr)
	if res.Code != int(code) || res.Transient {
		t.Fatalf("result %+v, want code %d, not transient", res, code)
	}
	if (code == exitOK) != (res.Status == "ok") {
		t.Fatalf("result status %q for code %d", res.Status, code)
	}
	return res
}

// flipData flips one byte inside the first data frame of device dev.
func flipData(t *testing.T, s []byte, dev uint32) []byte {
	t.Helper()
	out := append([]byte(nil), s...)
	for pos := 0; pos < len(out); {
		h, err := rvs.DecodeFrameHeader(out[pos : pos+rvs.FrameHeaderSize])
		if err != nil {
			t.Fatal(err)
		}
		if h.Type == rvs.TypeData && h.Dev == dev {
			out[pos+rvs.FrameHeaderSize+100] ^= 0x01
			return out
		}
		pos += rvs.FrameHeaderSize + int(h.Length)
	}
	t.Fatalf("no data frame for device %d", dev)
	return nil
}

func wrongManifest(t *testing.T, path string) string {
	t.Helper()
	m, err := verify.ReadManifestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Devices[1].SHA256CT = strings.Repeat("0", 64)
	p := filepath.Join(t.TempDir(), "wrong.manifest.json")
	if err := verify.WriteManifestFile(p, m); err != nil {
		t.Fatal(err)
	}
	return p
}

// decodeVMA parses a VMA stream with the 2.2 reader and rebuilds each device.
func decodeVMA(b []byte) (*vma.Header, map[uint8][]byte, error) {
	r, err := vma.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	h := r.Header()
	devs := map[uint8][]byte{}
	for _, d := range h.Devices {
		devs[d.ID] = make([]byte, vma.Clusters(d.Size)*vma.ClusterSize)
	}
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return h, nil, err
		}
		if !c.Zero {
			copy(devs[c.Dev][uint64(c.Index)*vma.ClusterSize:], c.Data)
		}
	}
	for _, d := range h.Devices {
		devs[d.ID] = devs[d.ID][:d.Size]
	}
	return h, devs, nil
}

func checkVMA(t *testing.T, out []byte, b *vmatest.Builder) {
	t.Helper()
	h, devs, err := decodeVMA(out)
	if err != nil {
		t.Fatalf("2.2 reader rejects the to-vma output: %v", err)
	}
	for _, d := range b.Devices {
		if !bytes.Equal(devs[d.ID], b.Contents(d.ID)) {
			t.Fatalf("device %d differs after backup → to-vma", d.ID)
		}
	}
	if len(h.Configs) != 1 || !bytes.Equal(h.Configs[0].Data, b.Configs[0].Data) {
		t.Fatal("config blob not verbatim")
	}
}

func assertTruncated(t *testing.T, out []byte) {
	t.Helper()
	if len(out) < vma.HeaderFixedSize {
		t.Fatalf("to-vma wrote only %d bytes; expected the header and the extents before the final one", len(out))
	}
	_, _, err := decodeVMA(out)
	var fe *vma.FormatError
	if !errors.As(err, &fe) || (fe.Kind != vma.ErrMissingCluster && fe.Kind != vma.ErrTruncated) {
		t.Fatalf("to-vma output after a failure must be a truncated VMA, reader says %v", err)
	}
}

// --- verify ---

func TestVerifyCommand(t *testing.T) {
	g := backupGuest(t)
	in := writeTemp(t, "guest.rvs", g.rvs)
	t.Run("clean stdin", func(t *testing.T) {
		r := runWith(t, g.rvs, "verify")
		r.expect(t, exitOK)
		if len(r.stdout) != 0 {
			t.Fatal("verify wrote to stdout")
		}
	})
	t.Run("clean with manifest", func(t *testing.T) {
		runWith(t, g.rvs, "verify", "--manifest", g.manifest).expect(t, exitOK)
	})
	t.Run("clean --in", func(t *testing.T) {
		runWith(t, nil, "verify", "--in", in, "--manifest", g.manifest).expect(t, exitOK)
	})
	t.Run("flipped data byte", func(t *testing.T) {
		res := runWith(t, flipData(t, g.rvs, 2), "verify", "--manifest", g.manifest).expect(t, exitIntegrity)
		if !strings.Contains(res.Message, "sha256ct") {
			t.Fatalf("message should name the digest: %q", res.Message)
		}
	})
	t.Run("wrong manifest", func(t *testing.T) {
		runWith(t, g.rvs, "verify", "--manifest", wrongManifest(t, g.manifest)).expect(t, exitManifestMismatch)
	})
	t.Run("malformed manifest", func(t *testing.T) {
		runWith(t, g.rvs, "verify", "--manifest", writeTemp(t, "m.json", []byte("{"))).expect(t, exitManifestMismatch)
	})
	t.Run("missing manifest file", func(t *testing.T) {
		runWith(t, g.rvs, "verify", "--manifest", filepath.Join(t.TempDir(), "none.json")).expect(t, exitLocalIO)
	})
	t.Run("truncated stream", func(t *testing.T) {
		runWith(t, g.rvs[:len(g.rvs)-10], "verify").expect(t, exitIntegrity)
	})
	t.Run("missing --in file", func(t *testing.T) {
		runWith(t, nil, "verify", "--in", filepath.Join(t.TempDir(), "none.rvs")).expect(t, exitLocalIO)
	})
}

// --- to-vma ---

func TestToVMACommand(t *testing.T) {
	g := backupGuest(t)
	in := writeTemp(t, "guest.rvs", g.rvs)
	t.Run("stdin to stdout", func(t *testing.T) {
		r := runWith(t, g.rvs, "to-vma", "--manifest", g.manifest)
		r.expect(t, exitOK)
		checkVMA(t, r.stdout, g.b)
	})
	t.Run("--in FILE to --out FILE", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "guest.vma")
		r := runWith(t, nil, "to-vma", "--in", in, "--out", out, "--manifest", g.manifest)
		r.expect(t, exitOK)
		if len(r.stdout) != 0 {
			t.Fatal("stdout used although --out names a file")
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		checkVMA(t, b, g.b)
	})
	t.Run("corrupted input", func(t *testing.T) {
		r := runWith(t, flipData(t, g.rvs, 2), "to-vma")
		r.expect(t, exitIntegrity)
		assertTruncated(t, r.stdout)
	})
	t.Run("wrong manifest", func(t *testing.T) {
		r := runWith(t, g.rvs, "to-vma", "--manifest", wrongManifest(t, g.manifest))
		r.expect(t, exitManifestMismatch)
		assertTruncated(t, r.stdout)
	})
	t.Run("corrupted --in to file", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "guest.vma")
		bad := writeTemp(t, "bad.rvs", flipData(t, g.rvs, 2))
		runWith(t, nil, "to-vma", "--in", bad, "--out", out).expect(t, exitIntegrity)
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		assertTruncated(t, b)
	})
	t.Run("existing --out", func(t *testing.T) {
		out := writeTemp(t, "taken.vma", []byte("keep"))
		runWith(t, g.rvs, "to-vma", "--out", out).expect(t, exitLocalIO)
		if b, _ := os.ReadFile(out); string(b) != "keep" {
			t.Fatal("existing output file overwritten")
		}
	})
	t.Run("stdout error", func(t *testing.T) {
		var errb bytes.Buffer
		code := run([]string{"to-vma"}, bytes.NewReader(g.rvs), &failWriter{}, &errb)
		if code != exitLocalIO {
			t.Fatalf("exit %d, want 7; stderr:\n%s", code, errb.String())
		}
	})
}

// --- to-raw ---

func TestToRawCommand(t *testing.T) {
	g := backupGuest(t)
	t.Run("stdin", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "raw")
		r := runWith(t, g.rvs, "to-raw", "--dir", dir, "--manifest", g.manifest)
		r.expect(t, exitOK)
		if len(r.stdout) != 0 {
			t.Fatal("to-raw wrote to stdout")
		}
		sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		var want strings.Builder
		for _, d := range g.b.Devices { // ascending id
			c := g.b.Contents(d.ID)
			got, err := os.ReadFile(filepath.Join(dir, d.Name+".raw"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, c) {
				t.Fatalf("%s.raw differs from the device", d.Name)
			}
			s := sha256.Sum256(c)
			want.WriteString(hex.EncodeToString(s[:]) + "  " + d.Name + ".raw\n")
		}
		if string(sums) != want.String() {
			t.Fatalf("SHA256SUMS:\n%s\nwant:\n%s", sums, want.String())
		}
		if p, err := exec.LookPath("sha256sum"); err == nil {
			cmd := exec.Command(p, "-c", "SHA256SUMS")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sha256sum -c: %v\n%s", err, out)
			}
		}
	})
	t.Run("--in FILE", func(t *testing.T) {
		dir := t.TempDir()
		runWith(t, nil, "to-raw", "--in", writeTemp(t, "g.rvs", g.rvs), "--dir", dir).expect(t, exitOK)
	})
	t.Run("corrupted stream", func(t *testing.T) {
		dir := t.TempDir()
		runWith(t, flipData(t, g.rvs, 2), "to-raw", "--dir", dir).expect(t, exitIntegrity)
		ents, _ := os.ReadDir(dir)
		if len(ents) != len(g.b.Devices) {
			t.Fatalf("%d entries, want %d *.raw.invalid", len(ents), len(g.b.Devices))
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".raw.invalid") {
				t.Fatalf("unexpected file %q", e.Name())
			}
		}
	})
	t.Run("wrong manifest", func(t *testing.T) {
		dir := t.TempDir()
		runWith(t, g.rvs, "to-raw", "--dir", dir, "--manifest", wrongManifest(t, g.manifest)).expect(t, exitManifestMismatch)
		if _, err := os.Stat(filepath.Join(dir, "SHA256SUMS")); !os.IsNotExist(err) {
			t.Fatal("SHA256SUMS written for a failed restore")
		}
	})
	t.Run("output dir error", func(t *testing.T) {
		file := writeTemp(t, "plain", nil)
		runWith(t, g.rvs, "to-raw", "--dir", file).expect(t, exitLocalIO)
	})
}
