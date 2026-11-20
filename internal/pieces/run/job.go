// Package run is `restic-pve pieces --job FILE` on the backup machine: one guest run in the
// piece layout. This release runs the probe: Proxmox's change record is looked at through a
// provider job, nothing is read from the disks and nothing is stored.
package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Modes of a job file.
const (
	ModeAuto      = "auto"       // decide per run (later release)
	ModeProbe     = "probe"      // look at the record; store nothing
	ModeFullCheck = "full-check" // a full read compared with the base (later release)
)

// Job is the job file backupctl writes (schema 1).
type Job struct {
	Schema           int             `json:"schema"`
	Mode             string          `json:"mode"`
	VMID             uint32          `json:"vmid"`
	Alias            string          `json:"alias"`
	Source           string          `json:"source"`
	SSHConfig        string          `json:"ssh_config"`
	SSHHost          string          `json:"ssh_host"`
	ResticHost       string          `json:"restic_host"`
	Tags             []string        `json:"tags"`
	Info             json.RawMessage `json:"info"`
	ViewDir          string          `json:"view_dir"`
	ProgressInterval int             `json:"progress_interval_s"`
	Deadline         string          `json:"deadline"`
	FullCheckEvery   int             `json:"full_check_every"`

	deadline time.Time
}

// ErrJob is the class of a job file that cannot be used (usage error, exit 64).
var ErrJob = errors.New("pieces: job file")

const maxJob = 1 << 20

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	tagRe  = regexp.MustCompile(`^[a-z][a-z_]{0,31}=[A-Za-z0-9_.:/@+-]{1,128}$`)
)

func plainArg(s string) bool {
	if s == "" || len(s) > 4096 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrJob, fmt.Sprintf(format, a...))
}

// LoadJob reads and checks a job file: at most 1 MiB, every field known and in its bounds.
func LoadJob(path string) (*Job, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, bad("%v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxJob+1))
	if err != nil || len(b) > maxJob {
		return nil, bad("%s: unreadable or larger than %d bytes", path, maxJob)
	}
	var j Job
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return nil, bad("%s: %v", path, err)
	}
	if dec.More() {
		return nil, bad("%s: data after the job", path)
	}
	return &j, j.check()
}

func (j *Job) check() error {
	switch {
	case j.Schema != 1:
		return bad("schema %d, this release reads schema 1", j.Schema)
	case j.Mode != ModeAuto && j.Mode != ModeProbe && j.Mode != ModeFullCheck:
		return bad("mode %q (auto, probe or full-check)", j.Mode)
	case j.VMID < 100 || j.VMID > 999999999:
		return bad("vmid %d", j.VMID)
	case !nameRe.MatchString(j.Alias):
		return bad("alias %q: 1-64 of A-Z a-z 0-9 _ . -", j.Alias)
	case !nameRe.MatchString(j.Source):
		return bad("source %q: 1-64 of A-Z a-z 0-9 _ . -", j.Source)
	case !plainArg(j.SSHConfig) || !plainArg(j.SSHHost):
		return bad("ssh_config and ssh_host must be plain words not starting with '-'")
	case !nameRe.MatchString(j.ResticHost):
		return bad("restic_host %q", j.ResticHost)
	case len(j.Tags) > 16:
		return bad("%d tags", len(j.Tags))
	case !filepath.IsAbs(j.ViewDir) || filepath.Clean(j.ViewDir) != j.ViewDir:
		return bad("view_dir %q must be a clean absolute path", j.ViewDir)
	case j.ProgressInterval < 1 || j.ProgressInterval > 3600:
		return bad("progress_interval_s %d (1-3600)", j.ProgressInterval)
	case j.FullCheckEvery < 1 || j.FullCheckEvery > 100:
		return bad("full_check_every %d (1-100)", j.FullCheckEvery)
	case len(j.Info) > 64<<10 || len(j.Info) > 0 && j.Info[0] != '{':
		return bad("info must be the JSON object of request info")
	}
	for _, t := range j.Tags {
		if !tagRe.MatchString(t) {
			return bad("tag %q", t)
		}
	}
	d, err := time.Parse(time.RFC3339, j.Deadline)
	if err != nil {
		return bad("deadline %q is not RFC 3339", j.Deadline)
	}
	j.deadline = d
	return nil
}

// Deadline is the time the run must end by.
func (j *Job) DeadlineTime() time.Time { return j.deadline }
