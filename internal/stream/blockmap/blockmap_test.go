package blockmap

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

func TestStateRoundTrip(t *testing.T) {
	s := &State{VMID: 100, Name: "web", Mode: "full", Status: Running,
		Started: time.UnixMilli(1763380000123), Updated: time.UnixMilli(1763380012456),
		Disk: 34359738368, Read: 1234567, Data: 7654, Handed: 4096, Written: 9999, Writing: true,
		Reading: []int{17, 18}, Message: "", Map: []byte("??..oo##")}
	b := s.Encode()
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Started.Equal(s.Started) || !got.Updated.Equal(s.Updated) {
		t.Fatalf("times %v %v", got.Started, got.Updated)
	}
	got.Started, got.Updated = s.Started, s.Updated
	if string(got.Encode()) != string(b) {
		t.Fatalf("round trip:\n%s\n%s", b, got.Encode())
	}
	// a failure message stays on one line; a map of MaxCells cells stays a few kilobytes
	s.Status, s.Message, s.Map = Failed, "stream cut:\nunexpected EOF\n", bytes.Repeat([]byte{CellData}, MaxCells)
	got, err = Decode(s.Encode())
	if err != nil || got.Message != "stream cut: unexpected EOF" || len(got.Map) != MaxCells {
		t.Fatalf("%v %q", err, got.Message)
	}
	if n := len(s.Encode()); n > MaxCells+512 {
		t.Errorf("%d bytes", n)
	}
	for _, bad := range []string{"", "vmastream-view 2\n", string(b[:len(b)/2]),
		strings.Replace(string(b), "map ??..oo##", "map ??..x", 1), strings.Replace(string(b), "read 1234567", "read -1", 1)} {
		if _, err := Decode([]byte(bad)); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// clock is a fake time source that moves on by step on every call.
type clock struct {
	t    time.Time
	step time.Duration
}

func (c *clock) now() time.Time { c.t = c.t.Add(c.step); return c.t }

func TestTrackerFramesPatchesAndReading(t *testing.T) {
	// one device of 8 cells x 2 clusters; frames of 4 clusters
	devs := []vma.DevInfo{{ID: 1, Name: "drive-scsi0", Size: 16 * vma.ClusterSize}}
	var snaps []*State
	c := &clock{t: time.Unix(1000, 0), step: time.Millisecond}
	tr := NewTracker(State{VMID: 100}, devs, 4*vma.ClusterSize, func(s *State) { snaps = append(snaps, s) }, c.now)
	if tr.cells != 16 {
		t.Fatalf("%d cells", tr.cells) // fewer clusters than MaxCells: one cell per cluster
	}
	data := make([]byte, vma.ClusterSize)
	arrive := func(i uint32, hasData bool) {
		cl := vma.Cluster{Dev: 1, Index: i, Zero: !hasData}
		if hasData {
			cl.Data = data
		}
		tr.Arrived(cl)
	}
	arrive(0, true)
	arrive(1, false)
	arrive(2, true)
	tr.InOrder(1, 0, true)
	tr.InOrder(1, 1, false)
	tr.InOrder(1, 2, true)
	if s := tr.Snapshot(Running, ""); string(s.Map[:4]) != "o.o?" || s.Handed != 0 || s.Read != 3*vma.ClusterSize || s.Data != 2*vma.ClusterSize {
		t.Fatalf("before the frame is written: %s handed %d", s.Map[:4], s.Handed)
	}
	arrive(3, true)
	tr.InOrder(1, 3, true) // the frame of clusters 0..3 is written now
	if s := tr.Snapshot(Running, ""); string(s.Map[:5]) != "#.##?" || s.Handed != 3*vma.ClusterSize {
		t.Fatalf("after the frame: %s handed %d", s.Map[:5], s.Handed)
	}
	// an early cluster far ahead, written as a patch: in the backup before its turn
	arrive(14, true)
	tr.Patch(1, 14)
	s := tr.Snapshot(Running, "")
	if s.Map[14] != CellHanded || len(s.Reading) != 0 {
		t.Fatalf("patch: %s reading %v", s.Map, s.Reading)
	}
	if !slices.Equal(tr.Snapshot(Running, "").Reading, s.Reading) {
		t.Error("without new arrivals the read position stays")
	}
	if len(snaps) == 0 {
		t.Error("no snapshot published")
	}
	// a cell with several clusters: being read while it is not complete
	devs = []vma.DevInfo{{ID: 1, Size: MaxCells * 3 * vma.ClusterSize}}
	tr = NewTracker(State{}, devs, rvs.FrameBytes, func(*State) {}, c.now)
	tr.Arrived(vma.Cluster{Dev: 1, Index: 30, Data: data})
	tr.Arrived(vma.Cluster{Dev: 1, Index: 31, Zero: true})
	if s := tr.Snapshot(Running, ""); !slices.Equal(s.Reading, []int{10}) || s.Map[10] != CellData {
		t.Fatalf("reading %v map %c", s.Reading, s.Map[10])
	}
	tr.Arrived(vma.Cluster{Dev: 1, Index: 32, Zero: true})
	if s := tr.Snapshot(Running, ""); len(s.Reading) != 0 {
		t.Fatalf("a complete cell is not being read: %v", s.Reading)
	}
	if s := tr.Final(Done, ""); s.Reading != nil || s.Status != Done {
		t.Fatal("final")
	}
}

// layoutDisk is a disk of n clusters with data where hasData says so, zeros elsewhere.
func layoutDisk(n int, hasData func(i int) bool) vmatest.Device {
	b := make([]byte, n*vma.ClusterSize)
	pattern := vmatest.PatternData(vma.ClusterSize, 7)
	for i := range n {
		if hasData(i) {
			copy(b[i*vma.ClusterSize:], pattern)
			b[i*vma.ClusterSize] = 1 // never all zero
		}
	}
	return vmatest.Device{ID: 1, Name: "drive-scsi0", Size: uint64(n * vma.ClusterSize), Data: b}
}

// runStream streams a built VMA through reader, reorder buffer and RVS1 writer as the backup
// does, with a tracker on it, and returns the snapshots it published.
func runStream(t *testing.T, b *vmatest.Builder, ramBytes int64, frameBytes uint64) []*State {
	t.Helper()
	res, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	r, err := vma.NewReader(bytes.NewReader(res.Stream))
	if err != nil {
		t.Fatal(err)
	}
	h := *r.Header()
	var snaps []*State
	c := &clock{t: time.Unix(1000, 0), step: Interval} // every arrival publishes
	tr := NewTracker(State{VMID: 100}, h.Devices, frameBytes, func(s *State) { snaps = append(snaps, s) }, c.now)
	w, err := rvs.NewWriter(io.Discard, h)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := reorder.New(reorder.Caps{RAMBytes: ramBytes}, reorder.Layout{Devices: h.Devices}, sink{w, tr}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		cl, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tr.Arrived(cl)
		if err := buf.Put(cl); err != nil {
			t.Fatal(err)
		}
	}
	if err := buf.Finish(); err != nil {
		t.Fatal(err)
	}
	return append(snaps, tr.Final(Done, ""))
}

type sink struct {
	w  *rvs.Writer
	tr *Tracker
}

func (s sink) InOrder(dev uint8, index uint32, data []byte) error {
	if err := s.w.InOrder(dev, index, data); err != nil {
		return err
	}
	s.tr.InOrder(dev, index, data != nil)
	return nil
}

func (s sink) Patch(dev uint8, index uint32, data []byte) error {
	if err := s.w.Patch(dev, index, data); err != nil {
		return err
	}
	s.tr.Patch(dev, index)
	return nil
}

// TestTrackerOnAStream builds the map from real streams: in order, with clusters arriving
// early, shuffled (with patches), and checks every snapshot against the disk.
func TestTrackerOnAStream(t *testing.T) {
	const n = 256
	hasData := func(i int) bool { return i < 40 || i >= 100 && i < 104 || i == 180 || i >= 250 }
	dev := layoutDisk(n, hasData)
	early := vmatest.SequentialOrder([]vmatest.Device{dev})
	// copy-before-write: clusters 200..203 and 180 arrive near the start
	for k, i := range []uint32{200, 201, 202, 203, 180} {
		x := vmatest.ClusterRef{Dev: 1, Index: i}
		p := slices.Index(early, x)
		early = slices.Insert(slices.Delete(early, p, p+1), 5+k, x)
	}
	for _, tc := range []struct {
		name  string
		order []vmatest.ClusterRef
		ram   int64
	}{
		{"in order", nil, 1 << 20},
		{"early clusters", early, 1 << 20},
		{"shuffled, with patches", vmatest.ShuffledOrder([]vmatest.Device{dev}, 3), 8 * vma.ClusterSize},
	} {
		b := &vmatest.Builder{UUID: [16]byte{1}, CTime: 1, Devices: []vmatest.Device{dev}, Order: tc.order}
		snaps := runStream(t, b, tc.ram, 16*vma.ClusterSize)
		if len(snaps) < n/2 {
			t.Fatalf("%s: %d snapshots", tc.name, len(snaps))
		}
		for k, s := range snaps {
			if !(s.Handed <= s.Data && s.Data <= s.Read && s.Read <= s.Disk) {
				t.Fatalf("%s %d: handed %d data %d read %d disk %d", tc.name, k, s.Handed, s.Data, s.Read, s.Disk)
			}
			for i, c := range s.Map {
				// a cell is empty or in the backup only when the disk says so
				if c == CellEmpty && hasData(i) || (c == CellHanded || c == CellData) && !hasData(i) {
					t.Fatalf("%s %d: cell %d is %c", tc.name, k, i, c)
				}
			}
		}
		last := snaps[len(snaps)-1]
		var want strings.Builder
		for i := range n {
			want.WriteByte(map[bool]byte{true: CellHanded, false: CellEmpty}[hasData(i)])
		}
		if string(last.Map) != want.String() || last.Read != last.Disk || last.Handed != last.Data {
			t.Fatalf("%s: final map %s", tc.name, last.Map)
		}
		if tc.name == "early clusters" {
			// right after the early clusters arrived, cell 180 holds data that waits in the
			// reorder buffer: read, not in the backup yet
			if s := snaps[12]; s.Map[180] != CellData || s.Map[200] != CellEmpty {
				t.Errorf("early: %c %c", s.Map[180], s.Map[200])
			}
		}
	}
}

func TestPublisherWritesLiveAndLast(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "view")
	p := Start(Options{Dir: dir, VMID: 100, Interval: 10 * time.Millisecond})
	var out bytes.Buffer
	w := p.Wrap(&out)
	s := &State{VMID: 100, Status: Running, Started: time.Now(), Map: []byte("o?")}
	p.Set(s)
	w.Write([]byte("abc"))
	var live *State
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		b, err := os.ReadFile(filepath.Join(dir, LiveFile))
		if err == nil {
			if live, err = Decode(b); err != nil {
				t.Fatalf("a reader saw a broken file: %v", err)
			}
			if live.Written == 3 {
				break
			}
		}
	}
	if live == nil || live.Written != 3 || out.String() != "abc" {
		t.Fatalf("live %+v", live)
	}
	if _, err := os.Stat(filepath.Join(dir, LastFile(100))); !os.IsNotExist(err) {
		t.Fatal("a last map before the end")
	}
	final := *s
	final.Status, final.Map = Done, []byte("#.")
	if !p.Close(&final, 5*time.Second) {
		t.Fatal("close timed out")
	}
	for _, name := range []string{LiveFile, LastFile(100)} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Decode(b); err != nil || got.Status != Done || string(got.Map) != "#." || got.Writing {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Errorf("files left: %v", ents)
	}
	// a failed run keeps the last map of the last good run
	p = Start(Options{Dir: dir, VMID: 100, Interval: time.Hour})
	final.Status, final.Map = Failed, []byte("o?")
	p.Close(&final, 5*time.Second)
	if b, _ := os.ReadFile(filepath.Join(dir, LastFile(100))); !strings.Contains(string(b), "map #.") {
		t.Error("a failed run replaced the last map")
	}
}

// TestPublisherFailures: an unwritable directory or a full disk switch the publisher off
// after one log line; a slow disk never holds Set, Wrap or Close longer than its wait.
func TestPublisherFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	full := func(string, []byte, os.FileMode) error { return syscall.ENOSPC }
	for name, o := range map[string]Options{
		"unwritable": {Dir: filepath.Join(file, "view")},
		"full disk":  {Dir: t.TempDir(), WriteFile: full},
	} {
		var logs atomic.Int32
		var line atomic.Value
		o.Interval, o.Logf = time.Millisecond, func(s string) { logs.Add(1); line.Store(s) }
		p := Start(o)
		p.Set(&State{Status: Running})
		for end := time.Now().Add(5 * time.Second); !p.Off() && time.Now().Before(end); {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if !p.Off() || logs.Load() != 1 {
			t.Fatalf("%s: off %v, %d log lines", name, p.Off(), logs.Load())
		}
		t.Logf("%s: %s", name, line.Load())
		if !p.Close(&State{Status: Done}, time.Second) {
			t.Errorf("%s: close after off", name)
		}
		if logs.Load() != 1 {
			t.Errorf("%s: logged again", name)
		}
	}
	block := make(chan struct{})
	slow := func(string, []byte, os.FileMode) error { <-block; return nil }
	p := Start(Options{Dir: t.TempDir(), WriteFile: slow, Interval: time.Millisecond})
	defer close(block)
	start := time.Now()
	w := p.Wrap(io.Discard)
	for range 100000 {
		p.Set(&State{Status: Running})
		w.Write([]byte{1})
	}
	if p.Close(&State{Status: Done}, 50*time.Millisecond) {
		t.Error("the final write cannot have ended")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a slow disk held the stream for %v", d)
	}
}
