package verify

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// ManifestSchema is the manifest JSON schema version.
const ManifestSchema = 1

// ErrManifest is the class of a manifest that is malformed or does not match
// the image (CLI exit 8: manifest mismatch or missing device).
var ErrManifest = errors.New("verify: manifest mismatch")

// ConfigDigest is the SHA-256 of one verbatim config blob.
type ConfigDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Manifest is schema v1, written by `vmastream backup` and enriched by
// backupctl (alias, description, snapshot id, image bytes) before storage.
type Manifest struct {
	Schema            int                 `json:"schema"`
	VMID              uint32              `json:"vmid"`
	Alias             string              `json:"alias"`
	Description       string              `json:"description"`
	PVEName           string              `json:"pve_name"`
	OSType            string              `json:"ostype"`
	Consistency       string              `json:"consistency"`
	ConsistencyDetail string              `json:"consistency_detail"`
	Devices           []rvs.TrailerDevice `json:"devices"`
	Configs           []ConfigDigest      `json:"configs"`
	VMAUUID           string              `json:"vma_uuid"`
	VMACTime          int64               `json:"vma_ctime"`
	PatchCount        uint64              `json:"patch_count"`
	BufferRAMPeak     int64               `json:"buffer_ram_peak"`
	ImageSnapshotID   string              `json:"image_snapshot_id"`
	ImageBytes        uint64              `json:"image_bytes"`
	Started           string              `json:"started"`
	Finished          string              `json:"finished"`
	Tool              string              `json:"tool"`
}

// ConfigDigests returns the SHA-256 of each config blob, in order.
func ConfigDigests(cfgs []vma.ConfigBlob) []ConfigDigest {
	out := make([]ConfigDigest, 0, len(cfgs))
	for _, c := range cfgs {
		s := sha256.Sum256(c.Data)
		out = append(out, ConfigDigest{Name: c.Name, SHA256: Hex(s)})
	}
	return out
}

// ReadManifest decodes one manifest. Fields it does not know are ignored
// (backupctl may add some); malformed JSON or a schema other than 1 is an
// ErrManifest error.
func ReadManifest(r io.Reader) (*Manifest, error) {
	dec := json.NewDecoder(r)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrManifest, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: data after the manifest object", ErrManifest)
	}
	if m.Schema != ManifestSchema {
		return nil, fmt.Errorf("%w: schema %d, want %d", ErrManifest, m.Schema, ManifestSchema)
	}
	return &m, nil
}

// ReadManifestFile reads a manifest file. Open errors are returned unwrapped
// (local I/O); content errors are ErrManifest.
func ReadManifestFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadManifest(f)
}

// Encode returns the manifest as canonical JSON (sorted keys, empty lists as []).
func (m *Manifest) Encode() ([]byte, error) {
	c := *m
	if c.Devices == nil {
		c.Devices = []rvs.TrailerDevice{}
	}
	if c.Configs == nil {
		c.Configs = []ConfigDigest{}
	}
	return rvs.CanonicalJSON(c)
}

// WriteManifestFile writes m to a temporary file in the target directory,
// fsyncs it, then renames it over path atomically. On failure nothing is left behind.
func WriteManifestFile(path string, m *Manifest) (err error) {
	b, err := m.Encode()
	if err != nil {
		return fmt.Errorf("verify: encode manifest: %w", err)
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	if err = f.Chmod(0o644); err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("verify: write manifest: %w", err)
	}
	if d, derr := os.Open(dir); derr == nil { // best effort: persist the rename
		d.Sync()
		d.Close()
	}
	return nil
}

// MismatchError lists every difference between a trailer and a manifest.
// errors.Is(err, ErrManifest) holds.
type MismatchError struct {
	Problems []string
}

func (e *MismatchError) Error() string {
	return "verify: manifest mismatch: " + strings.Join(e.Problems, "; ")
}

// Unwrap places the mismatch in the manifest class.
func (e *MismatchError) Unwrap() error { return ErrManifest }

func digestEqual(a, b string) bool { return strings.EqualFold(a, b) }

// Compare checks a stream's trailer against a manifest: the same device set
// (by id, with the same names), sizes and sha256ct digests. Counters and
// per-run values are informational and not compared. Any difference is a
// *MismatchError (ErrManifest class).
func Compare(t *rvs.Trailer, m *Manifest) error {
	var probs []string
	if m.Schema != ManifestSchema {
		probs = append(probs, fmt.Sprintf("manifest schema %d", m.Schema))
	}
	want := map[uint8]rvs.TrailerDevice{}
	for _, d := range m.Devices {
		if _, dup := want[d.ID]; dup {
			probs = append(probs, fmt.Sprintf("manifest lists device %d twice", d.ID))
		}
		want[d.ID] = d
	}
	seen := map[uint8]bool{}
	for _, g := range t.Devices {
		seen[g.ID] = true
		w, ok := want[g.ID]
		switch {
		case !ok:
			probs = append(probs, fmt.Sprintf("device %d (%s) not in the manifest", g.ID, g.Name))
			continue
		case w.Name != g.Name:
			probs = append(probs, fmt.Sprintf("device %d is %q in the image, %q in the manifest", g.ID, g.Name, w.Name))
		}
		if w.Size != g.Size {
			probs = append(probs, fmt.Sprintf("device %d (%s) size %d, manifest %d", g.ID, g.Name, g.Size, w.Size))
		}
		if !digestEqual(w.SHA256CT, g.SHA256CT) {
			probs = append(probs, fmt.Sprintf("device %d (%s) sha256ct %s, manifest %s", g.ID, g.Name, g.SHA256CT, w.SHA256CT))
		}
	}
	for _, d := range m.Devices {
		if !seen[d.ID] {
			probs = append(probs, fmt.Sprintf("manifest device %d (%s) missing from the image", d.ID, d.Name))
		}
	}
	if len(probs) > 0 {
		return &MismatchError{Problems: probs}
	}
	return nil
}
