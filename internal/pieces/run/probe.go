package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Probe asks the Proxmox host for its change record of the guest (request changes <vmid>
// probe), shows it in the block view, sets the guest's record state to none (Proxmox's
// record now counts from this job, past any base), sends "ack <token> probe" and writes the
// result line. It reads no disk data and runs no restic command.
func Probe(ctx context.Context, e Env, j *Job) Result {
	ctx, cancel := context.WithDeadline(ctx, j.DeadlineTime())
	defer cancel()
	r := &runner{ctx: ctx, e: e, j: j, ev: &events{w: e.Stderr}, recPath: pieces.RecordFile(e.State, j.Alias),
		res: Result{Type: "result", Kind: "probe", Check: "none", Disks: []Disk{}}}
	unlock, err := lockAlias(e.State, j.Alias)
	if err != nil {
		return r.fail(err)
	}
	defer unlock()
	r.v = newView(e, j, r.ev, blockmap.KindProbe)
	v, res := r.v, &r.res
	if err := r.loadRecord(); err != nil {
		return r.fail(err)
	}
	jb, err := r.startJob(changes.ModeProbe)
	if err != nil {
		return r.fail(err)
	}
	maps := map[uint8]*changes.Map{}
	if err := jb.frames(func(m changes.Map) error { maps[m.Dev] = &m; return nil }, nil); err != nil {
		return r.fail(err)
	}
	h := jb.h

	// what Proxmox reported, per disk
	devs := make([]vma.DevInfo, 0, len(h.Devices))
	for _, d := range h.Devices {
		devs = append(devs, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	v.begin(blockmap.KindProbe, devs, blockmap.CellUnread)
	var parts []string
	for _, d := range h.Devices {
		m := maps[d.ID]
		var dirty, changed uint64
		if m != nil && m.Recorded {
			for i := uint64(0); i < pieces.Pieces(d.Size); i++ {
				if m.Dirty(i) {
					changed++
					dirty += pieces.PieceLen(d.Size, i)
				} else {
					v.mark(d.Name, i, blockmap.CellKept, 0, pieces.PieceLen(d.Size, i))
				}
			}
			parts = append(parts, fmt.Sprintf("%s record reuse, %s changed of %s", d.Name, size(dirty), size(d.Size)))
		} else {
			dirty = d.Size
			parts = append(parts, fmt.Sprintf("%s record %s (the next run reads it in full)", d.Name, d.Bitmap))
		}
		res.Disks = append(res.Disks, Disk{Name: d.Name, Record: d.Bitmap, DirtyBytes: dirty})
		v.update(func(s *blockmap.State) {
			s.Records = append(s.Records, blockmap.Record{Disk: d.Name, State: d.Bitmap, Changed: changed, Pieces: pieces.Pieces(d.Size)})
			res.SkippedBytes = s.Skipped
		})
	}

	// the record state first, then the ack: nothing is stored, the record moved past any base
	r.rec.Probe(h.Token)
	if err := r.commit(jb, changes.Ack{Token: h.Token, Verb: changes.VerbProbe}); err != nil {
		return r.fail(err)
	}
	return r.end(CodeOK, false, "probe: "+strings.Join(parts, "; ")+"; nothing was read or stored")
}
