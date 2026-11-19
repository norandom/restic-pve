package cli

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// viewEnv turns the block view off when set to "off", "0", "false" or "no", whatever the flags
// say.
const viewEnv = "VMASTREAM_VIEW"

// viewCloseWait is the longest the end of a backup waits for the last state file to be
// written; a slow file system never holds the backup longer.
const viewCloseWait = time.Second

type viewLine struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// blockView is the block view of one backup: the publisher writes the state file, the tracker
// (made when the stream header arrived) builds the map.
type blockView struct {
	pub  *blockmap.Publisher
	tr   *blockmap.Tracker
	base blockmap.State
}

func viewDisabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// startView starts the block view of a backup, or returns nil when it is off (no --view-dir,
// or VMASTREAM_VIEW=off).
func startView(o *backupOptions, ev *eventLog, started time.Time) *blockView {
	if o.ViewDir == "" || viewDisabled(os.Getenv(viewEnv)) {
		return nil
	}
	var once sync.Once
	logf := func(msg string) {
		once.Do(func() { ev.emit(viewLine{Type: "view", Level: "debug", Message: msg}) })
	}
	v := &blockView{base: blockmap.State{VMID: o.VMID, Name: o.ViewName, Mode: "full", Status: blockmap.Running,
		Started: started}}
	v.pub = blockmap.Start(blockmap.Options{Dir: o.ViewDir, VMID: o.VMID, Logf: logf, WriteFile: o.viewWriteFile})
	first := v.base
	first.Updated = started
	v.pub.Set(&first) // no map yet: the stream has not started
	return v
}

// begin makes the tracker for the devices of the stream; nil when the view is off.
func (v *blockView) begin(devs []vma.DevInfo) *blockmap.Tracker {
	if v == nil {
		return nil
	}
	v.tr = blockmap.NewTracker(v.base, devs, rvs.FrameBytes, v.pub.Set, nil)
	return v.tr
}

// finish writes the final state: done after a complete stream, failed with the error
// otherwise.
func (v *blockView) finish(code exitCode, err error) {
	status, msg := blockmap.Done, ""
	if code != exitOK || err != nil {
		status = blockmap.Failed
		if err != nil {
			msg = err.Error()
		}
	}
	var s *blockmap.State
	if v.tr != nil {
		s = v.tr.Final(status, msg)
	} else {
		c := v.base
		c.Status, c.Message, c.Updated = status, msg, time.Now()
		s = &c
	}
	v.pub.Close(s, viewCloseWait)
}
