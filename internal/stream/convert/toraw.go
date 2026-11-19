package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/norandom/restic-pve/internal/stream/rvs"
)

const (
	// SumsFile lists the plain SHA-256 of every raw file (sha256sum format).
	SumsFile = "SHA256SUMS"
	// RawSuffix is appended to the device name to form its file name.
	RawSuffix = ".raw"
	// InvalidSuffix is appended to every raw file of a failed conversion.
	InvalidSuffix = ".invalid"
)

// safeName accepts device names usable as a file name in the output
// directory: [A-Za-z0-9._-], not starting with '.'.
func safeName(s string) bool {
	if s == "" || s[0] == '.' {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type rawFile struct {
	name string // file name inside the directory
	path string
	f    *os.File
	sum  hash.Hash // plain SHA-256 of the file, fed in device order
}

// rawHandler writes one sparse file per device. It writes patches at their
// positions and data clusters at their offsets. Zero clusters stay holes
// (the file is created empty and extended with Truncate). It computes plain
// SHA-256 in device order. At a patched position it reads the cluster back
// from the file.
type rawHandler struct {
	dir   string
	files map[uint8]*rawFile
	order []*rawFile
	sizes map[uint8]uint64
	buf   []byte
}

func (h *rawHandler) begin(hdr *rvs.Header) error {
	seen := map[string]bool{}
	for _, d := range hdr.Devices {
		if !safeName(d.Name) {
			return integrity("device %d name %q is not a safe file name", d.ID, d.Name)
		}
		if seen[d.Name] {
			return integrity("device name %q appears twice", d.Name)
		}
		seen[d.Name] = true
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return localIO("create output directory", err)
	}
	sums := filepath.Join(h.dir, SumsFile)
	if _, err := os.Lstat(sums); err == nil {
		return localIO("create "+SumsFile, fmt.Errorf("%s already exists", sums))
	} else if !errors.Is(err, os.ErrNotExist) {
		return localIO("create "+SumsFile, err)
	}
	h.files = map[uint8]*rawFile{}
	h.sizes = map[uint8]uint64{}
	h.buf = make([]byte, rvs.ClusterSize)
	for _, d := range hdr.Devices {
		name := d.Name + RawSuffix
		p := filepath.Join(h.dir, name)
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil { // an existing file is never overwritten
			return localIO("create "+name, err)
		}
		rf := &rawFile{name: name, path: p, f: f, sum: sha256.New()}
		h.files[d.ID] = rf
		h.order = append(h.order, rf)
		h.sizes[d.ID] = d.Size
		if err := f.Truncate(int64(d.Size)); err != nil {
			return localIO("size "+name, err)
		}
	}
	return nil
}

func (h *rawHandler) patch(dev uint8, index uint32, data []byte) error {
	rf := h.files[dev]
	if _, err := rf.f.WriteAt(data, int64(index)*rvs.ClusterSize); err != nil {
		return localIO("write "+rf.name, err)
	}
	return nil
}

func (h *rawHandler) data(dev uint8, index uint32, data []byte) error {
	rf := h.files[dev]
	rf.sum.Write(data)
	if allZero(data) {
		return nil // hole
	}
	return h.patch(dev, index, data)
}

func (h *rawHandler) patched(dev uint8, index uint32) error {
	rf := h.files[dev]
	off := uint64(index) * rvs.ClusterSize
	b := h.buf[:min(rvs.ClusterSize, h.sizes[dev]-off)]
	if _, err := rf.f.ReadAt(b, int64(off)); err != nil {
		return localIO("read back "+rf.name, err)
	}
	rf.sum.Write(b)
	return nil
}

// finish syncs and closes every file and writes SHA256SUMS atomically.
func (h *rawHandler) finish() error {
	var sums strings.Builder
	for _, rf := range h.order {
		if err := rf.f.Sync(); err != nil {
			return localIO("sync "+rf.name, err)
		}
		err := rf.f.Close()
		rf.f = nil
		if err != nil {
			return localIO("close "+rf.name, err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(rf.sum.Sum(nil)), rf.name)
	}
	return writeAtomic(h.dir, SumsFile, []byte(sums.String()))
}

// invalidate closes the files and renames each to *.raw.invalid. A failed
// conversion must never leave a file that looks like a restored device.
func (h *rawHandler) invalidate() error {
	var errs []error
	for _, rf := range h.order {
		if rf.f != nil {
			rf.f.Close()
			rf.f = nil
		}
		if err := os.Rename(rf.path, rf.path+InvalidSuffix); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func writeAtomic(dir, name string, b []byte) (err error) {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return localIO("write "+name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(b); err != nil {
		return localIO("write "+name, err)
	}
	if err = f.Chmod(0o644); err != nil {
		return localIO("write "+name, err)
	}
	if err = f.Sync(); err != nil {
		return localIO("write "+name, err)
	}
	if err = f.Close(); err != nil {
		return localIO("write "+name, err)
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return localIO("write "+name, err)
	}
	if d, derr := os.Open(dir); derr == nil { // best effort: persist the renames
		d.Sync()
		d.Close()
	}
	return nil
}

// ToRaw converts an RVS1 stream into dir/<device-name>.raw (sparse) per
// device plus dir/SHA256SUMS (plain SHA-256 of each file). dir is created
// if missing; existing raw files or SHA256SUMS are never overwritten. On any
// failure (digest or manifest mismatch, malformed stream, I/O) every raw
// file created is renamed to *.raw.invalid and no SHA256SUMS is written.
func ToRaw(in io.Reader, dir string, o Options) (*Result, error) {
	h := &rawHandler{dir: dir}
	res, err := walk(in, h, o)
	if err == nil {
		err = h.finish()
	}
	if err != nil {
		if ierr := h.invalidate(); ierr != nil {
			err = fmt.Errorf("%w (marking the output invalid failed: %v)", err, ierr)
		}
		return nil, err
	}
	return res, nil
}
