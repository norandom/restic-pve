package rvs

import (
	"bufio"
	"fmt"
	"io"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

var _ reorder.Sink = (*Writer)(nil)

// Writer writes an RVS1 stream. It implements reorder.Sink: InOrder must be
// called once per cluster in cursor order (devices ascending, then index) and
// Patch before the cursor reaches a position, as the reorder buffer does.
//
// The current data frame is assembled in memory (at most FrameBytes, or the
// largest device size if smaller) so that a patch for a position inside the
// open frame is still written before that data frame. Patched positions are
// passed as nil and become zero placeholders.
//
// A Writer is not safe for concurrent use. It keeps the first error. A stream
// without a trailer is incomplete. An aborted backup cannot be mistaken
// for a valid one.
type Writer struct {
	w      *bufio.Writer
	devs   []vma.DevInfo
	dev    int    // index into devs of the cursor
	idx    uint64 // next cluster index at the cursor
	frame  []byte // payload buffer of the open data frame
	fill   uint64 // bytes of the open data frame filled so far
	closed bool
	err    error
}

// NewWriter validates the device table, then writes the header frame and one
// config frame per blob (verbatim, VMA order). Output is buffered; Flush or
// Close writes it out. The uuid and ctime of h are not written (they belong in
// the trailer).
func NewWriter(w io.Writer, h vma.Header) (*Writer, error) {
	if err := validateDevices(h.Devices); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSequence, err)
	}
	for _, c := range h.Configs {
		if len(c.Name) == 0 || len(c.Name) > vma.MaxBlobSize || len(c.Data) > vma.MaxBlobSize {
			return nil, fmt.Errorf("%w: config %q (%d bytes) exceeds VMA blob limits", ErrSequence, c.Name, len(c.Data))
		}
	}
	rw := &Writer{w: bufio.NewWriterSize(w, 1<<20), devs: append([]vma.DevInfo(nil), h.Devices...)}
	var largest uint64
	for _, d := range rw.devs {
		largest = max(largest, d.Size)
	}
	rw.frame = make([]byte, min(FrameBytes, largest))
	rw.skipEmpty()
	if err := rw.emit(FrameHeader{Type: TypeHeader}, encodeHeaderPayload(rw.devs)); err != nil {
		return nil, err
	}
	for _, c := range h.Configs {
		if err := rw.emit(FrameHeader{Type: TypeConfig}, encodeConfigPayload(c)); err != nil {
			return nil, err
		}
	}
	return rw, nil
}

func (w *Writer) skipEmpty() {
	for w.dev < len(w.devs) && w.idx >= vma.Clusters(w.devs[w.dev].Size) {
		w.dev++
		w.idx = 0
	}
}

func (w *Writer) emit(h FrameHeader, payload []byte) error {
	h.Length = uint64(len(payload))
	hdr := h.Encode()
	if _, err := w.w.Write(hdr[:]); err != nil {
		return w.fail(fmt.Errorf("rvs: write: %w", err))
	}
	if _, err := w.w.Write(payload); err != nil {
		return w.fail(fmt.Errorf("rvs: write: %w", err))
	}
	return nil
}

func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return err
}

func (w *Writer) usable() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return fmt.Errorf("%w: writer closed", ErrSequence)
	}
	return nil
}

func contract(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrSequence}, a...)...)
}

// InOrder adds the next cluster at the cursor. data is nil for a zero cluster
// or a patched position, else at least the cluster's real length (normally
// ClusterSize; the final cluster is truncated to the device size).
func (w *Writer) InOrder(dev uint8, index uint32, data []byte) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.dev >= len(w.devs) {
		return w.fail(contract("InOrder(%d,%d) after the last cluster", dev, index))
	}
	d := w.devs[w.dev]
	if dev != d.ID || uint64(index) != w.idx {
		return w.fail(contract("InOrder(%d,%d), cursor at (%d,%d)", dev, index, d.ID, w.idx))
	}
	off := uint64(index) * ClusterSize
	n := min(ClusterSize, d.Size-off)
	if data != nil && uint64(len(data)) < n {
		return w.fail(contract("InOrder(%d,%d) data %d bytes, need %d", dev, index, len(data), n))
	}
	if off%FrameBytes == 0 {
		w.fill = 0
	}
	dst := w.frame[w.fill : w.fill+n]
	if data == nil {
		clear(dst)
	} else {
		copy(dst, data[:n])
	}
	w.fill += n
	w.idx++
	frameOff := off / FrameBytes * FrameBytes
	if w.fill == dataFrameLength(d.Size, frameOff) {
		if err := w.emit(FrameHeader{Type: TypeData, Dev: uint32(d.ID), Offset: frameOff}, w.frame[:w.fill]); err != nil {
			return err
		}
		w.fill = 0
	}
	w.skipEmpty()
	return nil
}

// Patch writes an early cluster ahead as a patch frame. The position must not
// have been reached by InOrder yet; InOrder later passes nil for it.
func (w *Writer) Patch(dev uint8, index uint32, data []byte) error {
	if err := w.usable(); err != nil {
		return err
	}
	pos := -1
	for i, d := range w.devs {
		if d.ID == dev {
			pos = i
			break
		}
	}
	if pos < 0 {
		return w.fail(contract("Patch(%d,%d): unknown device", dev, index))
	}
	d := w.devs[pos]
	if uint64(index) >= vma.Clusters(d.Size) {
		return w.fail(contract("Patch(%d,%d) beyond the device (%d bytes)", dev, index, d.Size))
	}
	if pos < w.dev || (pos == w.dev && uint64(index) < w.idx) {
		return w.fail(contract("Patch(%d,%d) behind the cursor", dev, index))
	}
	off := uint64(index) * ClusterSize
	n := min(ClusterSize, d.Size-off)
	if uint64(len(data)) < n {
		return w.fail(contract("Patch(%d,%d) data %d bytes, need %d", dev, index, len(data), n))
	}
	return w.emit(FrameHeader{Type: TypePatch, Dev: uint32(dev), Offset: off}, data[:n])
}

// Flush writes buffered frames to the underlying writer.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if err := w.w.Flush(); err != nil {
		return w.fail(fmt.Errorf("rvs: write: %w", err))
	}
	return nil
}

// Close writes the trailer (format forced to "RVS1", canonical JSON) and
// flushes. Every cluster must have been passed to InOrder, and t.Devices must
// list the device table (id, name, size) with hex sha256ct digests.
func (w *Writer) Close(t Trailer) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.dev < len(w.devs) {
		return w.fail(contract("Close with the cursor at device %d cluster %d", w.devs[w.dev].ID, w.idx))
	}
	if err := checkTrailerDevices(&t, w.devs); err != nil {
		return w.fail(contract("%v", err))
	}
	b, err := MarshalTrailer(t)
	if err != nil {
		return w.fail(fmt.Errorf("rvs: trailer: %w", err))
	}
	if len(b) > MaxTrailerBytes {
		return w.fail(contract("trailer %d bytes exceeds %d", len(b), MaxTrailerBytes))
	}
	if err := w.emit(FrameHeader{Type: TypeTrailer}, b); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	w.closed = true
	return nil
}
