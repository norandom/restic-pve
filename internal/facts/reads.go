package facts

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
)

// GuestRun is the last stored run of a guest that reads only changed blocks, as backupctl
// records it from the result line of `restic-pve pieces` (records.json of the guest profile,
// key guest_runs, per alias).
type GuestRun struct {
	Ended        time.Time `json:"ended"`
	Kind         string    `json:"kind"` // changes | full | check
	ReadBytes    int64     `json:"read_bytes"`
	SkippedBytes int64     `json:"skipped_bytes"`
}

// GuestReads is what this machine knows of a guest that reads only changed blocks.
type GuestReads struct {
	Last   *GuestRun           // nil: no run recorded yet
	Record *pieces.RecordState // <state>/pieces/<alias>.record.json; nil: no such file
}

// WorkSpace is what the pieces runs keep on this machine below <state>/pieces: each guest's
// tree (placeholders take no space) and staging folder.
type WorkSpace struct {
	Dir    string
	Total  int64       // allocated bytes below Dir
	Guests []GuestWork // the guests of backup.toml that have a folder there, in its order
	Free   *int64      // free on that file system; nil: not known
}

// GuestWork is the allocated bytes of one guest's folder.
type GuestWork struct {
	Alias string
	Bytes int64
}

// guestReads collects, per alias, the guests with a record state file or a recorded run.
func guestReads(state string, c Config, recG *Records) map[string]GuestReads {
	out := map[string]GuestReads{}
	for _, p := range c.Proxmox {
		for _, g := range p.Guests {
			var r GuestReads
			if recG != nil {
				if run, ok := recG.GuestRuns[g.Alias]; ok {
					r.Last = &run
				}
			}
			path := pieces.RecordFile(state, g.Alias)
			if _, err := os.Stat(path); err == nil {
				r.Record = pieces.LoadRecord(path, uint32(g.VMID))
			}
			if r.Last != nil || r.Record != nil {
				out[g.Alias] = r
			}
		}
	}
	return out
}

// workSpace measures <state>/pieces; nil when it is not a folder (no guest reads changes).
func workSpace(state string, c Config) *WorkSpace {
	dir := filepath.Join(state, "pieces")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	w := &WorkSpace{Dir: dir, Total: allocated(dir)}
	for _, p := range c.Proxmox {
		for _, g := range p.Guests {
			if fi, err := os.Stat(filepath.Join(dir, g.Alias)); err == nil && fi.IsDir() {
				w.Guests = append(w.Guests, GuestWork{g.Alias, allocated(filepath.Join(dir, g.Alias))})
			}
		}
	}
	if free, err := pieces.FreeSpace(dir); err == nil {
		v := int64(free)
		w.Free = &v
	}
	return w
}

// allocated is the space the files below dir take on disk (a sparse placeholder: none).
func allocated(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			} else {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
