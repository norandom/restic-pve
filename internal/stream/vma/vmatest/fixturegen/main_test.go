package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

// readAll decodes a stream with the production reader and returns the device
// contents by name.
func readAll(t *testing.T, path string) (map[string][]byte, *vma.Header, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := vma.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	h := r.Header()
	byID := map[uint8][]byte{}
	names := map[uint8]string{}
	for _, d := range h.Devices {
		byID[d.ID] = make([]byte, d.Size)
		names[d.ID] = d.Name
	}
	for {
		c, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, h, err
		}
		if !c.Zero {
			copy(byID[c.Dev][uint64(c.Index)*vma.ClusterSize:], c.Data)
		}
	}
	out := map[string][]byte{}
	for id, b := range byID {
		out[names[id]] = b
	}
	return out, h, nil
}

func TestGenerateStreamsMatchExpected(t *testing.T) {
	dir := t.TempDir()
	exp, err := Generate(dir, 3, 2)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, name := range []string{"example.vma", "example-shuffled.vma", "example-big.vma"} {
		contents, h, err := readAll(t, filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: reader rejected the stream: %v", name, err)
		}
		e := exp[name]
		if len(e.Devices) != 2 || len(contents) != 2 {
			t.Fatalf("%s: %d expected devices, %d decoded", name, len(e.Devices), len(contents))
		}
		for _, d := range e.Devices {
			sum := sha256.Sum256(contents[d.Name])
			if got := hex.EncodeToString(sum[:]); got != d.SHA256 || uint64(len(contents[d.Name])) != d.Size {
				t.Errorf("%s %s: sha256 %s size %d, expected %s size %d", name, d.Name, got,
					len(contents[d.Name]), d.SHA256, d.Size)
			}
		}
		if len(h.Configs) != 1 || !bytes.Equal(h.Configs[0].Data, Config().Data) {
			t.Errorf("%s: config blob not carried verbatim", name)
		}
		st, _ := os.Stat(filepath.Join(dir, name))
		if st.Size() != e.Bytes {
			t.Errorf("%s: %d bytes on disk, expected.json says %d", name, st.Size(), e.Bytes)
		}
	}
	if _, _, err := readAll(t, filepath.Join(dir, "example-tampered.vma")); err == nil {
		t.Error("tampered stream accepted by the reader")
	}
	if _, err := os.Stat(filepath.Join(dir, "expected.json")); err != nil {
		t.Error("expected.json missing")
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if _, err := Generate(a, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(b, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"example.vma", "example-tampered.vma", "example-shuffled.vma", "example-big.vma", "expected.json"} {
		x, _ := os.ReadFile(filepath.Join(a, name))
		y, _ := os.ReadFile(filepath.Join(b, name))
		if !bytes.Equal(x, y) {
			t.Errorf("%s differs between two runs", name)
		}
	}
}

func TestClusterDataPattern(t *testing.T) {
	d := StreamDevice{ID: 1, Name: "drive-scsi0", Size: 5*vma.ClusterSize + 100, Seed: 7}
	if !bytes.Equal(ClusterData(d, 1), make([]byte, vma.ClusterSize)) {
		t.Error("cluster 1 must be all zeros")
	}
	c3 := ClusterData(d, 3)
	if !bytes.Equal(c3[:vma.BlockSize], make([]byte, vma.BlockSize)) || bytes.Equal(c3[vma.BlockSize:2*vma.BlockSize], make([]byte, vma.BlockSize)) {
		t.Error("cluster 3 must have its even 4 KiB blocks zeroed")
	}
	if got := len(ClusterData(d, 5)); got != 100 {
		t.Errorf("last cluster length %d, want 100", got)
	}
	if bytes.Equal(ClusterData(d, 0), ClusterData(StreamDevice{ID: 1, Size: d.Size, Seed: 8}, 0)) {
		t.Error("different seeds must give different content")
	}
}
