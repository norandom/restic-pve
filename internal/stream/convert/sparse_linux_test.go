//go:build linux

package convert

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// TestToRawLeavesHoles checks that zero regions are not allocated (sparse files).
func TestToRawLeavesHoles(t *testing.T) {
	data := vmatest.PatternData(2<<20, 41)
	devs := []vmatest.Device{{ID: 1, Name: "drive-scsi0", Size: 16 << 20, Data: data}} // 14 MiB zero tail
	s, _ := encode(t, devs, nil, reorder.Caps{RAMBytes: 1 << 20})
	dir := t.TempDir()
	if _, err := ToRaw(bytes.NewReader(s), dir, Options{}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "drive-scsi0.raw")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 16<<20 {
		t.Fatalf("size %d", fi.Size())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no allocation information")
	}
	if alloc := st.Blocks * 512; alloc > 4<<20 {
		t.Fatalf("%d bytes allocated for 2 MiB of data in a 16 MiB device: zero regions are not holes", alloc)
	}
}
