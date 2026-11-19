package facts

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/stream/blockmap"
)

// ViewDir is the folder of the guest view below the state root: the stream writes its live
// file there while it reads a guest.
const ViewDir = GuestsDir + "/view"

// TooLong is how long a backup may get ready before the screens say it takes longer than
// usual (checks on the Proxmox host take about a minute).
const TooLong = 10 * time.Minute

// startSlack allows for the job record's start time, which has whole seconds only.
const startSlack = 2 * time.Second

// JobKind is what a job record is: "guests" for the guest backup, "local" for a backup of
// local sources (the job is named "local" or after its first source), "" for another job
// (setup, a restore).
func JobKind(name string) string {
	switch {
	case name == ProfileGuests:
		return ProfileGuests
	case name == "setup" || strings.HasPrefix(name, "restore"):
		return ""
	}
	return ProfileLocal
}

// Starting is true for a running backup job that has not begun reading yet: it holds its
// lock and has no end, while there is no live guest view newer than its start (guests) or no
// running entry of this run in status.json (local sources). It reads files only.
func Starting(state string, j Job) bool {
	if !j.Running || j.Ended != nil {
		return false
	}
	since := j.Started.Add(-startSlack)
	switch JobKind(j.Name) {
	case ProfileGuests:
		b, err := os.ReadFile(filepath.Join(state, ViewDir, blockmap.LiveFile))
		if err != nil {
			return true
		}
		live, err := blockmap.Decode(b)
		return err == nil && live.Started.Before(since) // a file that cannot be read is not "getting ready"
	case ProfileLocal:
		s := ReadStatus(state)
		return s == nil || s.RunStatus != "running" || s.Started == nil || s.Started.Before(since)
	}
	return false
}
