// Package pieces holds the piece layout of a guest snapshot: the names and the order of the
// files inside one restic snapshot, the manifest (schema 2) and the per-disk fingerprint
// sha256pt.
//
// A snapshot of the layout is a directory:
//
//	_manifest.json                    first in name order
//	config/qemu-server.conf
//	config/qemu-server.fw             when present
//	disks/<device>/<group>/<index>    4 MiB pieces, 256 per group, zero-padded
//
// restic stores and dumps the entries of a directory sorted by name, byte by byte, one path
// component at a time (Less). Files lists every file of a manifest in exactly that order, so
// a reader of `restic dump --archive tar` sees the manifest first and then each piece once,
// in order.
package pieces

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strings"
)

const (
	// PieceSize is the size of every piece but a disk's last: the granularity of Proxmox's
	// change record (the dirty bitmap of a provider job is made with 4 MiB granularity).
	PieceSize = 4 << 20
	// GroupSize is the number of pieces in one group directory.
	GroupSize = 256
	// MaxGroups bounds the groups of one disk so that the four-digit group name keeps the
	// name order equal to the number order.
	MaxGroups = 10000
	// MaxPieces is the most pieces one disk can have (MaxGroups x GroupSize), a disk of
	// 10,240,000 MiB (about 9.8 TiB).
	MaxPieces = MaxGroups * GroupSize

	// ManifestName, ConfigDir and DisksDir are the three entries of the snapshot directory.
	ManifestName = "_manifest.json"
	ConfigDir    = "config"
	DisksDir     = "disks"

	// PTPrefix starts the sha256pt outer hash.
	PTPrefix = "RVS2-PT\x00"
)

// Pieces returns the number of pieces of a disk of size bytes.
func Pieces(size uint64) uint64 { return (size + PieceSize - 1) / PieceSize }

// PieceLen returns the real length of piece index of a disk of size bytes: PieceSize, or the
// rest for the last piece.
func PieceLen(size, index uint64) uint64 {
	return min(PieceSize, size-index*PieceSize)
}

// PiecePath returns the path of piece index of device, relative to the snapshot root:
// disks/<device>/<group, 4 digits>/<index, 8 digits>.
func PiecePath(device string, index uint64) string {
	return fmt.Sprintf("%s/%s/%04d/%08d", DisksDir, device, index/GroupSize, index)
}

// ConfigPath returns the path of a config file, relative to the snapshot root.
func ConfigPath(name string) string { return ConfigDir + "/" + name }

// ValidDeviceName reports whether name can be a directory below disks/: 1 to 64 characters
// from A-Z a-z 0-9 . _ -, not starting with a dot. Proxmox device names (drive-scsi0,
// drive-efidisk0, drive-tpmstate0) pass.
func ValidDeviceName(name string) bool { return validName(name, 64) }

// ValidConfigName reports whether name can be a file below config/ (same characters, at most
// 255).
func ValidConfigName(name string) bool { return validName(name, 255) }

func validName(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen || s[0] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Less orders two relative paths the way restic stores and dumps them: component by
// component, each compared byte by byte. (Plain string order differs: "a-x/b" sorts before
// "a/b" because '-' is below '/'.)
func Less(a, b string) bool {
	for {
		ai, bi := strings.IndexByte(a, '/'), strings.IndexByte(b, '/')
		ac, bc := a, b
		if ai >= 0 {
			ac = a[:ai]
		}
		if bi >= 0 {
			bc = b[:bi]
		}
		if ac != bc {
			return ac < bc
		}
		if ai < 0 || bi < 0 {
			return ai < 0 && bi >= 0 // a parent comes before its children
		}
		a, b = a[ai+1:], b[bi+1:]
	}
}

// Files returns every file of a snapshot of m, relative to its root, in restic's name order:
// the manifest, the config files, then each disk's pieces. Directories are not listed.
func Files(m *Manifest) []string {
	out := []string{ManifestName}
	cfg := make([]string, 0, len(m.Config))
	for _, c := range m.Config {
		cfg = append(cfg, ConfigPath(c.Name))
	}
	sort.Slice(cfg, func(i, j int) bool { return Less(cfg[i], cfg[j]) })
	out = append(out, cfg...)
	devs := append([]Device(nil), m.Devices...)
	sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
	for _, d := range devs {
		for i := uint64(0); i < d.Pieces; i++ {
			out = append(out, PiecePath(d.Name, i))
		}
	}
	return out
}

// PieceDigest returns the lowercase hex SHA-256 of a piece's bytes (at its real length).
func PieceDigest(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// PT computes sha256pt of one disk from its piece digests, in piece order:
//
//	sha256pt = SHA-256("RVS2-PT\x00" ‖ u64le(size) ‖ u64le(piece_size) ‖ d_0 ‖ … ‖ d_{n-1})
//
// with d_i the 32-byte SHA-256 of piece i. Each piece's digest can come from its bytes (a
// full read or a changed piece) or from the parent's manifest (an unchanged piece), so a full
// read and a changed-blocks run of the same content give the same value.
type PT struct {
	size, pieces, next uint64
	h                  hash.Hash
}

// NewPT starts the fingerprint of a disk of size bytes.
func NewPT(size uint64) *PT {
	p := &PT{size: size, pieces: Pieces(size), h: sha256.New()}
	var b [8]byte
	p.h.Write([]byte(PTPrefix))
	binary.LittleEndian.PutUint64(b[:], size)
	p.h.Write(b[:])
	binary.LittleEndian.PutUint64(b[:], PieceSize)
	p.h.Write(b[:])
	return p
}

// Add adds the digest of the next piece (64 lowercase hex characters).
func (p *PT) Add(digest string) error {
	if p.next >= p.pieces {
		return fmt.Errorf("pieces: digest for piece %d of %d", p.next, p.pieces)
	}
	if !isHexDigest(digest) {
		return fmt.Errorf("pieces: piece %d digest %q is not 64 lowercase hex characters", p.next, digest)
	}
	var b [32]byte
	hex.Decode(b[:], []byte(digest))
	p.h.Write(b[:])
	p.next++
	return nil
}

// AddData adds the next piece from its bytes, which must be the piece's real length.
func (p *PT) AddData(data []byte) error {
	if p.next < p.pieces && uint64(len(data)) != PieceLen(p.size, p.next) {
		return fmt.Errorf("pieces: piece %d is %d bytes, want %d", p.next, len(data), PieceLen(p.size, p.next))
	}
	return p.Add(PieceDigest(data))
}

// Sum returns sha256pt in lowercase hex once every piece was added.
func (p *PT) Sum() (string, error) {
	if p.next != p.pieces {
		return "", fmt.Errorf("pieces: sha256pt with %d of %d pieces", p.next, p.pieces)
	}
	return hex.EncodeToString(p.h.Sum(nil)), nil
}

// SHA256PT computes sha256pt from a complete digest list.
func SHA256PT(size uint64, digests []string) (string, error) {
	p := NewPT(size)
	for _, d := range digests {
		if err := p.Add(d); err != nil {
			return "", err
		}
	}
	return p.Sum()
}

func isHexDigest(s string) bool { return isHex(s, 64) }

// isHex reports whether s is exactly n lowercase hex characters.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
