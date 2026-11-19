package convert

import (
	"bufio"
	"fmt"
	"io"

	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// errWriter records the first output write error. This separates writer
// failures (local I/O) from contract errors.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// vmaHandler writes every cluster once to a vma.Writer: patch clusters as
// soon as they arrive (VMA order is free), data clusters except at patched
// positions.
type vmaHandler struct {
	out *errWriter
	w   *vma.Writer
}

func (h *vmaHandler) fail(op string, err error) error {
	if h.out.err != nil {
		return localIO("write output", h.out.err)
	}
	return fmt.Errorf("convert: %s: %w", op, err)
}

func (h *vmaHandler) begin(hdr *rvs.Header) error {
	w, err := vma.NewWriter(h.out, vma.WriterOptions{Devices: hdr.Devices, Configs: hdr.Configs})
	if err != nil {
		if h.out.err != nil {
			return localIO("write output", h.out.err)
		}
		return integrity("stream cannot be written as VMA: %v", err)
	}
	h.w = w
	return nil
}

func (h *vmaHandler) patch(dev uint8, index uint32, data []byte) error {
	if err := h.w.WriteCluster(dev, index, data); err != nil {
		return h.fail("write cluster", err)
	}
	return nil
}

func (h *vmaHandler) data(dev uint8, index uint32, data []byte) error {
	return h.patch(dev, index, data)
}

func (h *vmaHandler) patched(uint8, uint32) error { return nil }

// ToVMA converts an RVS1 stream into an uncompressed VMA stream on out: a
// fresh uuid, the current time, the config blobs verbatim and every cluster
// of every device once. The final extent is written (Commit) only after
// every digest matches the trailer and the optional manifest. On any failure
// the writer aborts. out holds a truncated VMA stream that no reader
// (and no qmrestore) accepts.
func ToVMA(in io.Reader, out io.Writer, o Options) (*Result, error) {
	bw := bufio.NewWriterSize(out, 1<<20)
	ow := &errWriter{w: bw}
	h := &vmaHandler{out: ow}
	res, err := walk(in, h, o)
	if err != nil {
		if h.w != nil {
			h.w.Abort()
		}
		bw.Flush() // the partial stream (without its final extent) reaches the consumer
		return nil, err
	}
	if err := h.w.Commit(); err != nil {
		h.w.Abort()
		bw.Flush()
		return nil, h.fail("commit", err)
	}
	if err := bw.Flush(); err != nil {
		return nil, localIO("write output", err)
	}
	return res, nil
}
