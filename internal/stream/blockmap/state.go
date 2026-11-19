// Package blockmap keeps the map of a guest's disk while `vmastream backup` streams it, and
// writes it as a small state file that `vmastream watch` draws as the block view.
//
// Every state in the map comes from a real event of the stream: a cluster arrived from the
// Proxmox host (read), it held data or not (empty), its data was written to stdout, the stream
// restic reads (handed on). Nothing is estimated or animated.
//
// The writer never affects the backup: the stream goroutine only counts into its own arrays
// and, at most every Interval, stores a fresh snapshot with one atomic pointer store. A
// separate goroutine writes the file. Any write error switches the writer off for the rest of
// the run.
package blockmap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Magic is the first line of a state file, with the format version.
const Magic = "vmastream-view 1"

// MaxCells is the number of cells of a map; a disk with fewer clusters has one cell per
// cluster.
const MaxCells = 4096

// Interval is the shortest time between two snapshots and between two writes of the file.
const Interval = 500 * time.Millisecond

// LiveFile is the state file of the current (or the last) stream in the view directory.
const LiveFile = "live"

// LastFile is the final map of the last successful stream of a guest.
func LastFile(vmid uint32) string { return fmt.Sprintf("vm-%d.last", vmid) }

// Status of a stream.
const (
	Running = "running"
	Done    = "done"   // the whole stream, trailer included, is written to stdout
	Failed  = "failed" // vmastream ended with an error
)

// Map characters, one per cell.
const (
	CellUnread = '?' // no cluster of the cell arrived yet, or none of those that arrived held data
	CellEmpty  = '.' // every cluster arrived; none held data
	CellData   = 'o' // data arrived; not all of it is written to the stream yet, or the cell is not complete
	CellHanded = '#' // every cluster arrived and all its data is written to the stream
)

// State is one snapshot of a stream. The file holds one "key value" line per field, in this
// order, and ends with the map line:
//
//	vmastream-view 1
//	guest 100                 guest id
//	name web                  guest alias (may be empty)
//	mode full                 kind of backup
//	status running            running | done | failed
//	started 1763380000123     Unix milliseconds
//	updated 1763380012456     Unix milliseconds of this snapshot
//	disk 34359738368          bytes of all disks of the guest
//	read 1234567              bytes of the disks passed (clusters arrived, empty ones included)
//	data 1234567              bytes of clusters that held data
//	handed 1234567            bytes of data written to the stream
//	written 1234567           bytes written to stdout (framing and zero runs included)
//	writing 1                 1: a write to stdout is running or one happened since the last snapshot
//	reading 17 18             cells with clusters arriving now
//	message text              why the stream failed (one line), or empty
//	map ??..oo##...           one character per cell (see the Cell constants)
type State struct {
	VMID    uint32
	Name    string
	Mode    string
	Status  string
	Started time.Time
	Updated time.Time
	Disk    uint64
	Read    uint64
	Data    uint64
	Handed  uint64
	Written uint64
	Writing bool
	Reading []int
	Message string
	Map     []byte
}

// oneLine keeps a text field on one line and short.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// Encode returns the file content.
func (s *State) Encode() []byte {
	var b bytes.Buffer
	b.Grow(len(s.Map) + 400)
	fmt.Fprintf(&b, "%s\nguest %d\nname %s\nmode %s\nstatus %s\nstarted %d\nupdated %d\n", Magic, s.VMID,
		oneLine(s.Name, 64), oneLine(s.Mode, 32), s.Status, s.Started.UnixMilli(), s.Updated.UnixMilli())
	fmt.Fprintf(&b, "disk %d\nread %d\ndata %d\nhanded %d\nwritten %d\n", s.Disk, s.Read, s.Data, s.Handed, s.Written)
	w := 0
	if s.Writing {
		w = 1
	}
	fmt.Fprintf(&b, "writing %d\nreading", w)
	for _, r := range s.Reading {
		fmt.Fprintf(&b, " %d", r)
	}
	fmt.Fprintf(&b, "\nmessage %s\nmap %s\n", oneLine(s.Message, 300), s.Map)
	return b.Bytes()
}

// ErrFormat is returned for a file that is not a state file of this version.
var ErrFormat = errors.New("blockmap: not a state file of version 1")

// Decode parses a state file.
func Decode(b []byte) (*State, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() || sc.Text() != Magic {
		return nil, ErrFormat
	}
	s := &State{}
	seen := map[string]bool{}
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), " ")
		seen[key] = true
		var err error
		u := func() uint64 {
			var v uint64
			if err == nil {
				v, err = strconv.ParseUint(val, 10, 64)
			}
			return v
		}
		ms := func() time.Time {
			var v int64
			if err == nil {
				v, err = strconv.ParseInt(val, 10, 64)
			}
			return time.UnixMilli(v)
		}
		switch key {
		case "guest":
			s.VMID = uint32(min(u(), 1<<32-1))
		case "name":
			s.Name = val
		case "mode":
			s.Mode = val
		case "status":
			s.Status = val
		case "started":
			s.Started = ms()
		case "updated":
			s.Updated = ms()
		case "disk":
			s.Disk = u()
		case "read":
			s.Read = u()
		case "data":
			s.Data = u()
		case "handed":
			s.Handed = u()
		case "written":
			s.Written = u()
		case "writing":
			s.Writing = val == "1"
		case "reading":
			for _, f := range strings.Fields(val) {
				v, e := strconv.Atoi(f)
				if e != nil {
					err = e
					break
				}
				s.Reading = append(s.Reading, v)
			}
		case "message":
			s.Message = val
		case "map":
			s.Map = []byte(val)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrFormat, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	for _, k := range []string{"guest", "status", "started", "updated", "disk", "read", "map"} {
		if !seen[k] {
			return nil, fmt.Errorf("%w: no %s line (a file cut short?)", ErrFormat, k)
		}
	}
	for _, c := range s.Map {
		switch c {
		case CellUnread, CellEmpty, CellData, CellHanded:
		default:
			return nil, fmt.Errorf("%w: map character %q", ErrFormat, c)
		}
	}
	return s, nil
}
