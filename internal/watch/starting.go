package watch

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

// ResultCmd is where the admin sees how a job ended: the status screen, named the same way
// the status screen names its own command (status.SpaceCmd).
const ResultCmd = "./status.sh"

// Jobs are the backup jobs that run now (their lock is held), with Starting set; other jobs
// (setup, restores) are left out. state "" gives none.
func Jobs(state string) []facts.Job {
	if state == "" {
		return nil
	}
	var out []facts.Job
	for _, j := range facts.ReadJobs(filepath.Join(state, facts.JobsDir)) {
		if j.Running && facts.JobKind(j.Name) != "" {
			j.Starting = facts.Starting(state, j)
			out = append(out, j)
		}
	}
	return out
}

// firstStarting is the first job that is getting ready; nil: none.
func firstStarting(jobs []facts.Job) *facts.Job {
	for i := range jobs {
		if jobs[i].Starting {
			return &jobs[i]
		}
	}
	return nil
}

func backupOf(j facts.Job) string {
	if j.Name == facts.ProfileLocal {
		return "the backup of the local sources"
	}
	return "the backup of " + j.Name
}

// since is the time since the job's start in words: seconds in steps of five under a minute
// (the screen redraws every 5 s), then minutes.
func since(j facts.Job, now time.Time) string {
	d := max(now.Sub(j.Started), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds())/5*5)
	case d < 2*time.Minute:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

func short(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// BuildStarting is the notice of a backup job that runs but has not begun reading yet.
func BuildStarting(j facts.Job, now time.Time, w Words) Frame {
	what := upperFirst(backupOf(j))
	if now.Sub(j.Started) > facts.TooLong {
		return w.notice("Getting ready", "Getting ready for longer than usual...",
			fmt.Sprintf("%s has been getting ready for %s. That is longer than usual.", what, since(j, now)),
			"Look at its result: "+ResultCmd+".",
			"This screen changes by itself when reading begins.")
	}
	first := "First it checks the guest on the Proxmox host; the map appears when reading begins, usually within a minute or two."
	if facts.JobKind(j.Name) == facts.ProfileLocal {
		first = "First it prepares the run; the progress appears when reading begins, usually within a minute or two."
	}
	return w.notice("Getting ready", "Getting ready...",
		fmt.Sprintf("%s started %s ago and is getting ready.", what, since(j, now)),
		first,
		"Nothing to do: this screen changes by itself.")
}

// StartingLine is the plain line of a job that is getting ready.
func StartingLine(j facts.Job, now time.Time) string {
	d := max(now.Sub(j.Started), 0)
	l := fmt.Sprintf("starting: %s job began %s ago; reading has not begun yet", j.Name, short(d))
	if d > facts.TooLong {
		l += ", longer than usual; look at its result: " + ResultCmd
	}
	return l
}

// betweenSteps is the notice of a backup job that runs while neither a guest nor a local
// source is being read (between two guests, or completing the snapshot): never "no backup".
func betweenSteps(j facts.Job, now time.Time, w Words, last string) Frame {
	lines := []string{fmt.Sprintf("%s started %s ago and runs; it is between two steps now.", upperFirst(backupOf(j)), since(j, now))}
	if last != "" {
		lines = append(lines, upperFirst(last)+".")
	}
	lines = append(lines, "Nothing to do: this screen shows the next step by itself.")
	return w.notice("Backup running", "Working...", lines...)
}

// JobsLine is LineAll with the running jobs: a job that gets ready says so, and while a
// backup job holds its lock the line never says that no backup runs.
func JobsLine(jobs []facts.Job, snap Snapshot, lc Local, sp *LocalSpeed, now time.Time, start string) string {
	if j := firstStarting(jobs); j != nil {
		return StartingLine(*j, now)
	}
	l := LineAll(snap, lc, sp, now, start)
	if len(jobs) == 0 || !strings.HasPrefix(l, "no ") {
		return l
	}
	j := jobs[0]
	out := fmt.Sprintf("running: %s job began %s ago; between two steps", j.Name, short(max(now.Sub(j.Started), 0)))
	if _, last, ok := strings.Cut(l, "; the last one"); ok {
		out += "; the last one" + last
	}
	return out
}
