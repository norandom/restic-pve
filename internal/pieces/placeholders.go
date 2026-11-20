package pieces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrParent is the class of a parent snapshot that cannot be built on: the run reads in full
// without a parent instead.
var ErrParent = errors.New("pieces: parent not usable")

func badParent(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrParent, fmt.Sprintf(format, a...))
}

// Entry is one file or directory of a parent snapshot below the tree root.
type Entry struct {
	Rel   string // relative to the root, "" for the root itself
	Dir   bool
	Size  uint64
	Mode  os.FileMode
	MTime time.Time // with nanoseconds, as restic stored it
	ATime time.Time
}

// Listing is the tree of a parent snapshot as `restic ls --json --recursive <id> <root>` gave
// it. No placeholder state lives between runs: each run lists the parent and rebuilds.
type Listing struct {
	Snapshot Snapshot
	Root     string
	Entries  []Entry // in listing order; a directory before its contents
	files    map[string]int
}

// File returns the entry of the file rel.
func (l *Listing) File(rel string) (Entry, bool) {
	i, ok := l.files[rel]
	if !ok {
		return Entry{}, false
	}
	return l.Entries[i], true
}

func newListing(s Snapshot, root string, entries []Entry) *Listing {
	l := &Listing{Snapshot: s, Root: root, Entries: entries, files: map[string]int{}}
	for i, e := range entries {
		if !e.Dir {
			l.files[e.Rel] = i
		}
	}
	return l
}

// Files returns the number of files in the listing.
func (l *Listing) Files() int { return len(l.files) }

// List lists the tree root of snapshot id. Only plain directories and files below root with
// clean names are allowed; anything else (a symlink, a path outside root, a name twice, no
// root at all: restic lists a missing path without an error) makes the parent unusable.
func List(ctx context.Context, r *Restic, id, root string) (*Listing, error) {
	l := &Listing{Root: root, files: map[string]int{}}
	seen := map[string]bool{}
	var bad error
	err := r.Ls(ctx, id, root, func(s Snapshot) error { l.Snapshot = s; return nil }, func(n Node) error {
		if bad != nil {
			return nil
		}
		rel, ok := relPath(root, n.Path)
		switch {
		case !ok:
			bad = badParent("snapshot %s: %q is outside %s", short(id), n.Path, root)
		case seen[rel]:
			bad = badParent("snapshot %s: %q twice", short(id), n.Path)
		case n.Type != "dir" && n.Type != "file", rel == "" && n.Type != "dir":
			bad = badParent("snapshot %s: %q is a %s", short(id), n.Path, n.Type)
		case rel != "" && !seen[parentRel(rel)]:
			bad = badParent("snapshot %s: %q listed before its folder", short(id), n.Path)
		}
		if bad != nil {
			return nil
		}
		seen[rel] = true
		e := Entry{Rel: rel, Dir: n.Type == "dir", Size: n.Size, Mode: n.Mode.Perm(), MTime: n.MTime, ATime: n.ATime}
		if !e.Dir {
			l.files[rel] = len(l.Entries)
		}
		l.Entries = append(l.Entries, e)
		return nil
	})
	if err != nil {
		var re *ResticError
		if errors.As(err, &re) {
			return nil, fmt.Errorf("%w: %v", ErrParent, err)
		}
		return nil, err
	}
	if bad != nil {
		return nil, bad
	}
	if !seen[""] {
		return nil, badParent("snapshot %s: path %s is not in the snapshot", short(id), root)
	}
	return l, nil
}

func short(id string) string { return id[:min(8, len(id))] }

// relPath returns p relative to root when p is root or below it, with clean components.
func relPath(root, p string) (string, bool) {
	if p == root {
		return "", true
	}
	rel, ok := strings.CutPrefix(p, root+"/")
	if !ok || rel == "" {
		return "", false
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return "", false
		}
	}
	return rel, true
}

func parentRel(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// PiecesOnly keeps the pieces of a listing: the disks folder and everything below it. The
// manifest and the config files are written new by every run's final batch.
func PiecesOnly(rel string) bool { return rel == DisksDir || strings.HasPrefix(rel, DisksDir+"/") }

// Rebuild deletes the tree at root and builds it again from the listing: one sparse file per
// listed file that keep accepts (its size, no data blocks, its mode, mtime and atime with
// nanoseconds) and the folders above them. restic then finds each placeholder unchanged
// against the parent (size and mtime equal; the new inode and ctime are ignored with
// --ignore-inode) and takes its content from the parent without opening it. Returns the
// number of files built.
func (l *Listing) Rebuild(root string, keep func(rel string) bool) (int, error) {
	if err := os.RemoveAll(root); err != nil {
		return 0, fmt.Errorf("pieces: remove %s: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, fmt.Errorf("pieces: %w", err)
	}
	var dirs []Entry
	n := 0
	for _, e := range l.Entries {
		if e.Rel == "" || !keep(e.Rel) {
			continue
		}
		p := filepath.Join(root, e.Rel)
		if e.Dir {
			if err := os.Mkdir(p, 0o755); err != nil && !os.IsExist(err) {
				return n, fmt.Errorf("pieces: %w", err)
			}
			dirs = append(dirs, e)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return n, fmt.Errorf("pieces: %w", err)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, e.Mode|0o200)
		if err != nil {
			return n, fmt.Errorf("pieces: placeholder: %w", err)
		}
		err = f.Truncate(int64(e.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chtimes(p, e.ATime, e.MTime)
		}
		if err != nil {
			return n, fmt.Errorf("pieces: placeholder %s: %w", e.Rel, err)
		}
		n++
	}
	// folders last, deepest first: creating their contents changed their mtime
	sort.SliceStable(dirs, func(i, j int) bool { return len(dirs[i].Rel) > len(dirs[j].Rel) })
	for _, e := range dirs {
		if err := os.Chtimes(filepath.Join(root, e.Rel), e.ATime, e.MTime); err != nil {
			return n, fmt.Errorf("pieces: %w", err)
		}
	}
	return n, nil
}

// ParentWant is what a parent must be to be built on.
type ParentWant struct {
	Root  string   // the snapshot path, /state/pieces/<alias>/vm-<vmid>
	Host  string   // restic host of the unit
	Tags  []string // the unit's guest tags
	VMID  uint32
	Alias string
	Base  string // the record state's base (or the full read a full check just accepted)
}

// Parent is a usable parent: its listing and its manifest agree.
type Parent struct {
	ID       string
	Listing  *Listing
	Manifest *Manifest
	devices  map[string]*Device
}

func newParent(id string, l *Listing, m *Manifest) *Parent {
	p := &Parent{ID: id, Listing: l, Manifest: m, devices: map[string]*Device{}}
	for i := range m.Devices {
		p.devices[m.Devices[i].Name] = &m.Devices[i]
	}
	return p
}

// Digest returns the parent's digest of piece index of device, if it has that piece.
func (p *Parent) Digest(device string, index uint64) (string, bool) {
	d, ok := p.devices[device]
	if !ok || index >= d.Pieces {
		return "", false
	}
	return d.Digests[index], true
}

// Device returns the parent's device by name.
func (p *Parent) Device(name string) (*Device, bool) {
	d, ok := p.devices[name]
	return d, ok
}

// MaxMTime is the newest mtime of the parent's pieces.
func (p *Parent) MaxMTime() time.Time {
	var t time.Time
	for _, e := range p.Listing.Entries {
		if !e.Dir && PiecesOnly(e.Rel) && e.MTime.After(t) {
			t = e.MTime
		}
	}
	return t
}

// LoadParent lists snapshot id and checks every rule of a usable parent: it is the record's
// base, its paths are exactly [root], its host is the unit's restic host, it carries the
// unit's tags, it holds a valid _manifest.json of this guest, and the listing holds exactly
// the manifest's files, each piece at its size. Anything else is ErrParent with the reason.
func LoadParent(ctx context.Context, r *Restic, id string, want ParentWant) (*Parent, error) {
	if want.Base != "" && id != want.Base {
		return nil, badParent("snapshot %s is not the record's base %s", short(id), short(want.Base))
	}
	l, err := List(ctx, r, id, want.Root)
	if err != nil {
		return nil, err
	}
	s := l.Snapshot
	switch {
	case s.ID != id:
		return nil, badParent("restic listed snapshot %s for %s", short(s.ID), short(id))
	case len(s.Paths) != 1 || s.Paths[0] != want.Root:
		return nil, badParent("snapshot %s has the paths %q, want [%s]", short(id), s.Paths, want.Root)
	case s.Hostname != want.Host:
		return nil, badParent("snapshot %s has the host %q, want %q", short(id), s.Hostname, want.Host)
	case !s.HasTags(want.Tags):
		return nil, badParent("snapshot %s lacks a tag of %q (it has %q)", short(id), want.Tags, s.Tags)
	}
	me, ok := l.File(ManifestName)
	if !ok {
		return nil, badParent("snapshot %s has no %s", short(id), ManifestName)
	}
	if me.Size > MaxManifestBytes {
		return nil, badParent("snapshot %s: %s is %d bytes", short(id), ManifestName, me.Size)
	}
	b, err := r.DumpFile(ctx, id, want.Root+"/"+ManifestName, MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParent, err)
	}
	m, err := ReadManifest(strings.NewReader(string(b)))
	if err != nil {
		return nil, badParent("snapshot %s: %v", short(id), err)
	}
	if m.VMID != want.VMID || m.Alias != want.Alias {
		return nil, badParent("snapshot %s: the manifest is of VM %d %s, want VM %d %s", short(id), m.VMID, m.Alias, want.VMID, want.Alias)
	}
	p := newParent(id, l, m)
	files := Files(m)
	if len(files) != l.Files() {
		return nil, badParent("snapshot %s holds %d files, its manifest %d: the tree does not match the manifest", short(id), l.Files(), len(files))
	}
	for _, f := range files {
		e, ok := l.File(f)
		if !ok {
			return nil, badParent("snapshot %s has no %s: the tree does not match the manifest", short(id), f)
		}
		if dev, idx, ok := parsePiece(f); ok {
			d := p.devices[dev]
			if want := PieceLen(d.Size, idx); e.Size != want {
				return nil, badParent("snapshot %s: %s has the size %d, want %d", short(id), f, e.Size, want)
			}
		}
	}
	return p, nil
}

// parsePiece returns the device and index of a piece path disks/<device>/<group>/<index>.
func parsePiece(rel string) (string, uint64, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != DisksDir || len(parts[2]) != 4 || len(parts[3]) != 8 {
		return "", 0, false
	}
	var idx uint64
	for _, c := range parts[3] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		idx = idx*10 + uint64(c-'0')
	}
	if PiecePath(parts[1], idx) != rel {
		return "", 0, false
	}
	return parts[1], idx, true
}
