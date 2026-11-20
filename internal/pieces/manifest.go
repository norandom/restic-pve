package pieces

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Schema is the manifest schema of the piece layout; Layout its "layout" value.
const (
	Schema = 2
	Layout = "pieces"
	// MaxManifestBytes bounds a manifest: about a million pieces (3.8 TiB of disks) in total.
	// A 125 GiB disk gives about 2.1 MB.
	MaxManifestBytes = 64 << 20
)

// ErrManifest is the class of a manifest that is malformed, truncated or inconsistent (stream
// commands: exit 8).
var ErrManifest = errors.New("pieces: manifest invalid")

// Manifest is _manifest.json, schema 2. It is inside the snapshot it describes.
type Manifest struct {
	Schema      int         `json:"schema"`
	Layout      string      `json:"layout"`
	Producer    string      `json:"producer"`
	VMID        uint32      `json:"vmid"`
	Alias       string      `json:"alias"`
	Created     string      `json:"created"` // RFC 3339
	Kind        string      `json:"kind"`    // changes | full | check
	Parent      *string     `json:"parent"`  // restic snapshot id, or null
	PieceSize   uint64      `json:"piece_size"`
	Devices     []Device    `json:"devices"` // ascending id
	Config      []Config    `json:"config"`
	QmdumpMap   []string    `json:"qmdump_map"`
	Consistency Consistency `json:"consistency"`
	Record      *Record     `json:"record"` // the provider job of a changes run; null for a plain full read
}

// Device is one disk: its size, its pieces' digests and their fingerprint.
type Device struct {
	ID       uint8    `json:"id"`
	Name     string   `json:"name"`
	Size     uint64   `json:"size"`
	Pieces   uint64   `json:"pieces"`
	SHA256PT string   `json:"sha256pt"`
	Digests  []string `json:"digests"` // digests[i] = SHA-256 of piece i at its real length
}

// Config is one config file below config/.
type Config struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Consistency is how the guest was quiesced, as in the stream contract.
type Consistency struct {
	Value  string `json:"value"` // application | crash | stopped
	Detail string `json:"detail"`
}

// Record names the provider job whose change record the run used, per disk.
type Record struct {
	Job   string       `json:"job"` // 128-bit job token, 32 lowercase hex
	Disks []RecordDisk `json:"disks"`
}

// RecordDisk is Proxmox's bitmap mode for one disk and the bytes it reported dirty.
type RecordDisk struct {
	Name       string `json:"name"`
	State      string `json:"state"` // none | new | reuse
	DirtyBytes uint64 `json:"dirty_bytes"`
}

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrManifest}, args...)...)
}

// Validate checks every field and the consistency of the digests with sha256pt and the
// sizes. A manifest that fails is never written and never trusted when read.
func (m *Manifest) Validate() error {
	if m.Schema != Schema {
		return bad("schema %d, want %d", m.Schema, Schema)
	}
	if m.Layout != Layout {
		return bad("layout %q, want %q", m.Layout, Layout)
	}
	if !printable(m.Producer, 128) {
		return bad("producer %q", m.Producer)
	}
	if m.VMID < 100 || m.VMID > 999999999 {
		return bad("vmid %d", m.VMID)
	}
	if !validName(m.Alias, 64) {
		return bad("alias %q", m.Alias)
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		return bad("created %q is not RFC 3339", m.Created)
	}
	switch m.Kind {
	case "changes", "full", "check":
	default:
		return bad("kind %q", m.Kind)
	}
	if m.Parent != nil && !isHexDigest(*m.Parent) {
		return bad("parent %q is not a restic snapshot id", *m.Parent)
	}
	if m.Kind != "full" && m.Parent == nil {
		return bad("kind %s needs a parent", m.Kind)
	}
	if m.PieceSize != PieceSize {
		return bad("piece_size %d, want %d", m.PieceSize, PieceSize)
	}
	if len(m.Devices) == 0 || len(m.Devices) > vma.MaxDevID {
		return bad("%d devices", len(m.Devices))
	}
	names := map[string]uint64{}
	for i, d := range m.Devices {
		if d.ID < vma.MinDevID || d.ID > vma.MaxDevID || i > 0 && d.ID <= m.Devices[i-1].ID {
			return bad("device %d: id %d out of range or not ascending", i, d.ID)
		}
		if !ValidDeviceName(d.Name) {
			return bad("device %d: name %q", d.ID, d.Name)
		}
		if _, dup := names[d.Name]; dup {
			return bad("device name %q twice", d.Name)
		}
		names[d.Name] = d.Size
		if d.Size == 0 || Pieces(d.Size) > MaxPieces {
			return bad("device %s: size %d", d.Name, d.Size)
		}
		if d.Pieces != Pieces(d.Size) || uint64(len(d.Digests)) != d.Pieces {
			return bad("device %s: %d pieces and %d digests for size %d, want %d", d.Name, d.Pieces, len(d.Digests), d.Size, Pieces(d.Size))
		}
		pt, err := SHA256PT(d.Size, d.Digests)
		if err != nil {
			return bad("device %s: %v", d.Name, err)
		}
		if pt != d.SHA256PT {
			return bad("device %s: sha256pt %s does not match its digests (%s)", d.Name, d.SHA256PT, pt)
		}
	}
	if len(m.Config) == 0 || len(m.Config) > 16 {
		return bad("%d config files", len(m.Config))
	}
	seen := map[string]bool{}
	for _, c := range m.Config {
		if !ValidConfigName(c.Name) || seen[c.Name] {
			return bad("config name %q invalid or twice", c.Name)
		}
		seen[c.Name] = true
		if !isHexDigest(c.SHA256) {
			return bad("config %s: sha256 %q", c.Name, c.SHA256)
		}
	}
	if !seen["qemu-server.conf"] {
		return bad("config has no qemu-server.conf")
	}
	if len(m.QmdumpMap) > 256 {
		return bad("%d qmdump_map lines", len(m.QmdumpMap))
	}
	for _, l := range m.QmdumpMap {
		if len(l) < len("#qmdump#map:") || l[:len("#qmdump#map:")] != "#qmdump#map:" || !printable(l, 1024) {
			return bad("qmdump_map line %q", l)
		}
	}
	switch m.Consistency.Value {
	case "application", "crash", "stopped":
	default:
		return bad("consistency %q", m.Consistency.Value)
	}
	if len(m.Consistency.Detail) > 1024 || m.Consistency.Detail != "" && !printable(m.Consistency.Detail, 1024) {
		return bad("consistency detail")
	}
	return m.validateRecord(names)
}

func (m *Manifest) validateRecord(sizes map[string]uint64) error {
	r := m.Record
	if r == nil {
		if m.Kind == "changes" {
			return bad("kind changes without a record")
		}
		return nil
	}
	if !isHex(r.Job, 32) {
		return bad("record job %q is not 32 lowercase hex characters", r.Job)
	}
	if len(r.Disks) != len(m.Devices) {
		return bad("record lists %d disks, manifest %d", len(r.Disks), len(m.Devices))
	}
	seen := map[string]bool{}
	for _, d := range r.Disks {
		size, ok := sizes[d.Name]
		if !ok || seen[d.Name] {
			return bad("record disk %q unknown or twice", d.Name)
		}
		seen[d.Name] = true
		switch d.State {
		case "none", "new", "reuse":
		default:
			return bad("record disk %s: state %q", d.Name, d.State)
		}
		if m.Kind == "changes" && d.State != "reuse" {
			return bad("kind changes with disk %s in state %s", d.Name, d.State)
		}
		if d.DirtyBytes > size {
			return bad("record disk %s: %d dirty bytes of %d", d.Name, d.DirtyBytes, size)
		}
	}
	return nil
}

func printable(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Encode validates m and returns it as canonical JSON (sorted keys, no HTML escaping).
func (m *Manifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	c := *m
	if c.QmdumpMap == nil {
		c.QmdumpMap = []string{}
	}
	return rvs.CanonicalJSON(c)
}

// ReadManifest decodes and validates one manifest of at most MaxManifestBytes. Unknown
// fields, data after the object, a truncated object and every failed check are ErrManifest.
func ReadManifest(r io.Reader) (*Manifest, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pieces: read manifest: %w", err)
	}
	if len(b) > MaxManifestBytes {
		return nil, bad("larger than %d bytes", MaxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, bad("decode: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad("data after the manifest object")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
