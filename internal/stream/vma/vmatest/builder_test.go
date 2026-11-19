package vmatest

import (
	"bytes"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/vma"
)

var testUUID = [16]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

func multiDevice() *Builder {
	devs := []Device{
		DataDisk(1, "scsi0", 300*vma.ClusterSize+12345, 1),
		EFIDisk(2, 8*vma.ClusterSize, 2),
		TPMState(3, vma.ClusterSize, 3),
	}
	return &Builder{
		UUID:    testUUID,
		CTime:   1760000000,
		Devices: devs,
		Configs: []vma.ConfigBlob{ExampleConfig()},
		Order:   InterleavedOrder(devs),
	}
}

func mustBuild(t *testing.T, b *Builder) (*Result, *Layout) {
	t.Helper()
	r, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	l, err := Inspect(r.Stream)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return r, l
}

type seenMap map[ClusterRef]int

func seen(l *Layout) seenMap {
	s := seenMap{}
	for _, e := range l.Extents {
		for _, en := range e.Entries {
			s[ClusterRef{en.Dev, en.Index}]++
		}
	}
	return s
}

func checkValid(t *testing.T, b *Builder, r *Result, l *Layout) {
	t.Helper()
	if !l.HeaderMD5OK() {
		t.Error("header md5 mismatch")
	}
	if l.UUID != r.UUID {
		t.Error("result uuid differs from header uuid")
	}
	if int64(l.HeaderSize) != r.HeaderSize || len(r.ExtentOffsets) != len(l.Extents) {
		t.Errorf("result layout: header %d/%d extents %d/%d", r.HeaderSize, l.HeaderSize, len(r.ExtentOffsets), len(l.Extents))
	}
	for i, e := range l.Extents {
		if !e.MD5OK() || e.UUID != l.UUID || e.Reserved != 0 || e.Magic != vma.ExtentMagic {
			t.Errorf("extent %d invalid (md5 ok %v, uuid ok %v, reserved %d)", i, e.MD5OK(), e.UUID == l.UUID, e.Reserved)
		}
		if i < len(r.ExtentOffsets) && r.ExtentOffsets[i] != e.Offset {
			t.Errorf("extent %d offset %d, result says %d", i, e.Offset, r.ExtentOffsets[i])
		}
	}
	s := seen(l)
	for _, d := range b.Devices {
		for i := uint32(0); i < uint32(vma.Clusters(d.Size)); i++ {
			if s[ClusterRef{d.ID, i}] != 1 {
				t.Fatalf("device %d cluster %d seen %d times", d.ID, i, s[ClusterRef{d.ID, i}])
			}
		}
	}
	c := l.Contents()
	for _, d := range b.Devices {
		if !bytes.Equal(c[d.ID], b.Contents(d.ID)) {
			t.Errorf("device %d content differs", d.ID)
		}
	}
}

func TestBuildMultiDevice(t *testing.T) {
	b := multiDevice()
	r, l := mustBuild(t, b)
	checkValid(t, b, r, l)
	if r.UUID != testUUID || l.CTime != 1760000000 {
		t.Fatalf("uuid %x ctime %d", r.UUID, l.CTime)
	}
	names := []string{"drive-scsi0", "drive-efidisk0", "drive-tpmstate0-backup"}
	if len(l.Devices) != 3 {
		t.Fatalf("devices %+v", l.Devices)
	}
	for i, d := range l.Devices {
		if d.Name != names[i] || d.ID != uint8(i+1) || d.Size != b.Devices[i].Size {
			t.Errorf("device %d = %+v", i, d)
		}
	}
	if len(l.Configs) != 1 || l.Configs[0].Name != "qemu-server.conf" || !bytes.Equal(l.Configs[0].Data, ExampleConfig().Data) {
		t.Fatalf("config not verbatim: %+v", l.Configs)
	}
	for _, line := range []string{"#qmdump#map:scsi0:drive-scsi0:", "#qmdump#map:efidisk0:drive-efidisk0:", "#qmdump#map:tpmstate0:drive-tpmstate0-backup:"} {
		if !bytes.Contains(l.Configs[0].Data, []byte(line)) {
			t.Errorf("config lacks %q", line)
		}
	}
	// The small devices are interleaved throughout the large one.
	var devSeq []uint8
	for _, e := range l.Extents {
		for _, en := range e.Entries {
			devSeq = append(devSeq, en.Dev)
		}
	}
	n := len(devSeq)
	lo, hi := n, -1
	for i, d := range devSeq {
		if d == 2 {
			lo, hi = min(lo, i), max(hi, i)
		}
	}
	if devSeq[0] != 1 || devSeq[n-1] != 1 || lo > n/4 || hi < 3*n/4 {
		t.Errorf("arrival order is not interleaved: efidisk clusters at %d..%d of %d", lo, hi, n)
	}
	// Pattern data has zero clusters (mask 0) and partially zero clusters.
	var zero, partial int
	for _, e := range l.Extents {
		for _, en := range e.Entries {
			switch {
			case en.Mask == 0:
				zero++
			case en.Mask != 0xFFFF:
				partial++
			}
		}
	}
	if zero == 0 || partial == 0 {
		t.Errorf("pattern data: %d zero, %d partial clusters", zero, partial)
	}
}

func TestBuildOrders(t *testing.T) {
	devs := []Device{DataDisk(1, "virtio0", 70*vma.ClusterSize, 5), EFIDisk(4, 3*vma.ClusterSize, 6)}
	for name, order := range map[string][]ClusterRef{
		"default":     nil,
		"sequential":  SequentialOrder(devs),
		"shuffled":    ShuffledOrder(devs, 7),
		"interleaved": InterleavedOrder(devs),
		"custom":      append([]ClusterRef{{4, 2}, {1, 69}, {4, 0}}, removeRefs(SequentialOrder(devs), ClusterRef{4, 2}, ClusterRef{1, 69}, ClusterRef{4, 0})...),
	} {
		b := &Builder{Devices: devs, Order: order}
		r, l := mustBuild(t, b)
		checkValid(t, b, r, l)
		if order == nil {
			order = SequentialOrder(devs)
		}
		var got []ClusterRef
		for _, e := range l.Extents {
			for _, en := range e.Entries {
				got = append(got, ClusterRef{en.Dev, en.Index})
			}
		}
		for i := range order {
			if got[i] != order[i] {
				t.Fatalf("%s: entry %d = %v, want %v", name, i, got[i], order[i])
			}
		}
	}
	if s, sh := SequentialOrder(devs), ShuffledOrder(devs, 7); equalRefs(s, sh) || !equalRefs(sh, ShuffledOrder(devs, 7)) {
		t.Error("shuffled order is not a deterministic permutation")
	}
}

func removeRefs(in []ClusterRef, drop ...ClusterRef) []ClusterRef {
	var out []ClusterRef
outer:
	for _, r := range in {
		for _, d := range drop {
			if r == d {
				continue outer
			}
		}
		out = append(out, r)
	}
	return out
}

func equalRefs(a, b []ClusterRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildRejectsIncompleteOrder(t *testing.T) {
	devs := []Device{DataDisk(1, "scsi0", 4*vma.ClusterSize, 1)}
	if _, err := (&Builder{Devices: devs, Order: SequentialOrder(devs)[:3]}).Build(); err == nil {
		t.Fatal("incomplete order built")
	}
	dup := append(SequentialOrder(devs), ClusterRef{1, 0})
	if _, err := (&Builder{Devices: devs, Order: dup}).Build(); err == nil {
		t.Fatal("duplicate order built")
	}
}

func TestBuildDeterministicWithFixedUUID(t *testing.T) {
	a, _ := mustBuild(t, multiDevice())
	b, _ := mustBuild(t, multiDevice())
	if !bytes.Equal(a.Stream, b.Stream) {
		t.Fatal("same builder input gives different streams")
	}
}

func TestBuildVMStateDevice(t *testing.T) {
	devs := []Device{DataDisk(1, "scsi0", 2*vma.ClusterSize, 1), VMStateDevice(2, 3*vma.ClusterSize)}
	b := &Builder{Devices: devs}
	r, l := mustBuild(t, b)
	checkValid(t, b, r, l)
	if l.Devices[1].Name != "vmstate" {
		t.Fatalf("devices %+v", l.Devices)
	}
}

// tampered builds the multi-device stream with one defect. It returns the
// clean and tampered layouts.
func tampered(t *testing.T, tm Tamper) (*Result, *Layout, *Result, *Layout) {
	t.Helper()
	cr, cl := mustBuild(t, multiDevice())
	b := multiDevice()
	b.Tamper = tm
	r, l := mustBuild(t, b)
	return cr, cl, r, l
}

func allExtentsOK(l *Layout, except int) bool {
	for i, e := range l.Extents {
		if i == except {
			continue
		}
		if !e.MD5OK() || e.UUID != l.UUID || e.Reserved != 0 {
			return false
		}
		for _, en := range e.Entries {
			if en.Reserved != 0 {
				return false
			}
		}
	}
	return true
}

func TestTamperHeaderMD5(t *testing.T) {
	_, cl, r, l := tampered(t, Tamper{Kind: TamperHeaderMD5})
	if l.HeaderMD5OK() || !allExtentsOK(l, -1) || len(l.Extents) != len(cl.Extents) {
		t.Fatal("only the header md5 must be wrong")
	}
	if r.TamperOffset != 0 {
		t.Fatalf("offset %d", r.TamperOffset)
	}
}

func TestTamperExtentMD5(t *testing.T) {
	_, _, r, l := tampered(t, Tamper{Kind: TamperExtentMD5, Extent: 2})
	e := l.Extents[2]
	if e.MD5OK() || e.UUID != l.UUID || !l.HeaderMD5OK() || !allExtentsOK(l, 2) {
		t.Fatal("only extent 2's md5 must be wrong")
	}
	if r.TamperOffset != e.Offset {
		t.Fatalf("offset %d, extent at %d", r.TamperOffset, e.Offset)
	}
}

func TestTamperExtentUUID(t *testing.T) {
	_, _, r, l := tampered(t, Tamper{Kind: TamperExtentUUID, Extent: 1})
	e := l.Extents[1]
	if !e.MD5OK() || e.UUID == l.UUID || !allExtentsOK(l, 1) {
		t.Fatal("extent 1 must have a valid md5 and a foreign uuid")
	}
	if r.TamperOffset != e.Offset {
		t.Fatalf("offset %d", r.TamperOffset)
	}
}

func TestTamperReserved(t *testing.T) {
	_, _, r, l := tampered(t, Tamper{Kind: TamperReserved, Extent: 1})
	e := l.Extents[1]
	if !e.MD5OK() || e.Reserved == 0 || !allExtentsOK(l, 1) {
		t.Fatal("extent 1 must have a valid md5 and reserved != 0")
	}
	if r.TamperOffset != e.Offset {
		t.Fatalf("offset %d", r.TamperOffset)
	}
}

func TestTamperBlockInfo(t *testing.T) {
	b := multiDevice()
	present := map[uint8]uint64{}
	for _, d := range b.Devices {
		present[d.ID] = vma.Clusters(d.Size)
	}
	for _, kind := range []TamperKind{TamperBlockInfoDevice, TamperBlockInfoRange, TamperBlockInfoReserved} {
		_, cl, r, l := tampered(t, Tamper{Kind: kind, Extent: 1})
		e := l.Extents[1]
		if !e.MD5OK() || !allExtentsOK(l, 1) {
			t.Fatalf("kind %d: extent md5 must stay valid", kind)
		}
		first, orig := e.Entries[0], cl.Extents[1].Entries[0]
		switch kind {
		case TamperBlockInfoDevice:
			if _, ok := present[first.Dev]; ok || first.Dev == 0 {
				t.Errorf("device tamper left dev %d", first.Dev)
			}
		case TamperBlockInfoRange:
			if first.Dev != orig.Dev || uint64(first.Index) < present[first.Dev] {
				t.Errorf("range tamper: dev %d index %d", first.Dev, first.Index)
			}
		case TamperBlockInfoReserved:
			if first.Reserved == 0 || first.Dev != orig.Dev || first.Index != orig.Index {
				t.Errorf("reserved tamper: %+v", first)
			}
		}
		if first.Mask != orig.Mask {
			t.Errorf("kind %d changed the mask", kind)
		}
		if r.TamperOffset != e.Offset {
			t.Errorf("kind %d offset %d", kind, r.TamperOffset)
		}
	}
}

func TestTamperDuplicateCluster(t *testing.T) {
	_, cl, r, l := tampered(t, Tamper{Kind: TamperDuplicateCluster, Extent: 1})
	if !l.HeaderMD5OK() || !allExtentsOK(l, -1) || len(l.Extents) != len(cl.Extents)+1 {
		t.Fatal("duplicate must add one valid extent")
	}
	dups := 0
	for _, n := range seen(l) {
		if n > 1 {
			dups++
		}
		if n == 0 {
			t.Fatal("duplicate tamper lost a cluster")
		}
	}
	if dups == 0 {
		t.Fatal("no cluster seen twice")
	}
	if r.TamperOffset != l.Extents[2].Offset {
		t.Fatalf("offset %d, duplicate extent at %d", r.TamperOffset, l.Extents[2].Offset)
	}
}

func TestTamperMissingCluster(t *testing.T) {
	_, cl, r, l := tampered(t, Tamper{Kind: TamperMissingCluster, Extent: 1})
	if !l.HeaderMD5OK() || !allExtentsOK(l, -1) || len(l.Extents) != len(cl.Extents)-1 || l.TrailingBytes != 0 {
		t.Fatal("missing must drop one extent and keep the rest valid")
	}
	if len(seen(l)) >= len(seen(cl)) {
		t.Fatal("no cluster missing")
	}
	if r.TamperOffset != int64(len(r.Stream)) {
		t.Fatalf("offset %d, want EOF %d", r.TamperOffset, len(r.Stream))
	}
}

func TestTamperTruncate(t *testing.T) {
	cr, cl, r, l := tampered(t, Tamper{Kind: TamperTruncate})
	if len(r.Stream) >= len(cr.Stream) || l.TrailingBytes == 0 || len(l.Extents) != len(cl.Extents)-1 {
		t.Fatalf("default truncation must cut inside the final extent (len %d/%d, trailing %d)", len(r.Stream), len(cr.Stream), l.TrailingBytes)
	}
	if r.TamperOffset != int64(len(r.Stream)) {
		t.Fatalf("offset %d", r.TamperOffset)
	}
	b := multiDevice()
	b.Tamper = Tamper{Kind: TamperTruncate, At: 5000} // inside the fixed header
	r2, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Stream) != 5000 || r2.TamperOffset != 5000 {
		t.Fatalf("explicit truncation at %d", len(r2.Stream))
	}
}

func TestTamperBadExtentIndex(t *testing.T) {
	b := multiDevice()
	b.Tamper = Tamper{Kind: TamperExtentMD5, Extent: 10000}
	if _, err := b.Build(); err == nil {
		t.Fatal("tamper on a non-existent extent built")
	}
}
