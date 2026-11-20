// Package convert reads an RVS1 stream in one pass to check or convert it:
// Verify (fingerprints only), ToVMA (an uncompressed VMA stream for
// qmrestore) and ToRaw (one sparse raw file per device).
//
// Every path recomputes sha256ct per device and compares it with the trailer
// and, when given, the manifest (Requirement 10.3). Success is reported only
// on a match. ToVMA withholds the final VMA extent until then. ToRaw renames
// its files to *.raw.invalid on any failure.
//
// VerifyPieces, ToVMAPieces and ToRawPieces read the tar of a snapshot in the
// piece layout instead (pieces.go) and check each piece and sha256pt.
//
// Error classes: rvs.ErrIntegrity (malformed stream or tar, digest mismatch, doubly
// patched position, non-zero placeholder; CLI exit 3), verify.ErrManifest or
// pieces.ErrManifest (manifest mismatch; exit 8), and ErrLocalIO (input read, output write and
// output directory errors; exit 7). Anything else is an internal error.
package convert

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
)

// ErrLocalIO covers local I/O failures when reading input, writing output
// or creating the output directory or files.
var ErrLocalIO = errors.New("convert: local I/O error")

// IOError is a local I/O failure; errors.Is(err, ErrLocalIO) holds.
type IOError struct {
	Op  string
	Err error
}

func (e *IOError) Error() string { return "convert: " + e.Op + ": " + e.Err.Error() }

// Unwrap places the error in the local I/O class and keeps the cause.
func (e *IOError) Unwrap() []error { return []error{ErrLocalIO, e.Err} }

func localIO(op string, err error) error { return &IOError{Op: op, Err: err} }

// integrity returns an error of the rvs integrity class.
func integrity(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{rvs.ErrIntegrity}, a...)...)
}

// Options are common to every conversion.
type Options struct {
	// Manifest, when set, must match the trailer (devices, sizes, digests).
	Manifest *verify.Manifest
	// Pieces, when set, must match the manifest inside a pieces snapshot (disks, sizes,
	// sha256pt); pieces input only.
	Pieces *pieces.Manifest
}

// Result describes a verified stream.
type Result struct {
	Header  *rvs.Header
	Trailer *rvs.Trailer
	// PatchedClusters counts clusters supplied by patch frames.
	PatchedClusters uint64
	// Bytes is the total size of all devices.
	Bytes uint64
}

// handler receives the clusters of a stream. patch is called when a patch
// frame supplies a cluster (in stream order); data for every other cluster
// and patched when the data frame reaches a patched position, both in device
// order. data slices are only valid during the call and have the cluster's
// real length.
type handler interface {
	begin(h *rvs.Header) error
	patch(dev uint8, index uint32, data []byte) error
	data(dev uint8, index uint32, data []byte) error
	patched(dev uint8, index uint32) error
}

// inputErr keeps integrity errors and classifies everything else from the
// reader as local I/O.
func inputErr(err error) error {
	if errors.Is(err, rvs.ErrIntegrity) {
		return err
	}
	return localIO("read input", err)
}

func posKey(dev uint8, index uint32) uint64 { return uint64(dev)<<32 | uint64(index) }

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// walk reads the whole stream, feeds h, recomputes every sha256ct and
// compares it with the trailer and o.Manifest. Patched positions are
// recorded (with the hash of the patch) and a position patched twice is
// rejected, because the rvs reader does not detect it.
func walk(in io.Reader, h handler, o Options) (*Result, error) {
	r, err := rvs.NewReader(in)
	if err != nil {
		return nil, inputErr(err)
	}
	hdr := r.Header()
	if err := h.begin(hdr); err != nil {
		return nil, err
	}
	res := &Result{Header: hdr}
	sizes := map[uint32]uint64{}
	for _, d := range hdr.Devices {
		sizes[uint32(d.ID)] = d.Size
		res.Bytes += d.Size
	}
	fp := verify.NewFingerprint(reorder.Layout{Devices: hdr.Devices})
	patched := map[uint64][32]byte{}
	buf := make([]byte, rvs.ClusterSize)
	for {
		fh, p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, inputErr(err)
		}
		dev := uint8(fh.Dev) // the reader checked that the device exists (id ≤ 254)
		size := sizes[fh.Dev]
		end := fh.Offset + fh.Length
		for off := fh.Offset; off < end; off += rvs.ClusterSize {
			index := uint32(off / rvs.ClusterSize)
			d := buf[:min(rvs.ClusterSize, size-off)]
			if _, err := io.ReadFull(p, d); err != nil {
				return nil, inputErr(err)
			}
			key := posKey(dev, index)
			if fh.Type == rvs.TypePatch {
				if _, dup := patched[key]; dup {
					return nil, integrity("device %d cluster %d patched twice", dev, index)
				}
				patched[key] = sha256.Sum256(d)
				res.PatchedClusters++
				if err := h.patch(dev, index, d); err != nil {
					return nil, err
				}
				continue
			}
			if sum, ok := patched[key]; ok {
				if !allZero(d) {
					return nil, integrity("device %d cluster %d: patched position holds a non-zero placeholder", dev, index)
				}
				delete(patched, key)
				fp.ClusterHash(dev, index, sum)
				if err := fp.InOrder(dev, index, nil); err != nil {
					return nil, err
				}
				if err := h.patched(dev, index); err != nil {
					return nil, err
				}
				continue
			}
			if err := fp.InOrder(dev, index, d); err != nil {
				return nil, err
			}
			if err := h.data(dev, index, d); err != nil {
				return nil, err
			}
		}
	}
	tr, err := r.Trailer()
	if err != nil {
		return nil, err
	}
	if len(patched) != 0 {
		return nil, fmt.Errorf("convert: %d patched positions never reached by a data frame", len(patched))
	}
	if err := fp.Check(tr); err != nil {
		return nil, err
	}
	if o.Manifest != nil {
		if err := verify.Compare(tr, o.Manifest); err != nil {
			return nil, err
		}
	}
	res.Trailer = tr
	return res, nil
}

// nopHandler only lets walk verify.
type nopHandler struct{}

func (nopHandler) begin(*rvs.Header) error           { return nil }
func (nopHandler) patch(uint8, uint32, []byte) error { return nil }
func (nopHandler) data(uint8, uint32, []byte) error  { return nil }
func (nopHandler) patched(uint8, uint32) error       { return nil }

// Verify reads an RVS1 stream and checks every device's sha256ct against the
// trailer and the optional manifest.
func Verify(in io.Reader, o Options) (*Result, error) {
	return walk(in, nopHandler{}, o)
}
