package facts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File names below the state root (local profile) and its guests/ folder (guest profile).
const (
	RecordsFile = "records.json"
	StatusFile  = "status.json"
	RunLock     = "run.lock"
	JobsDir     = "jobs"
	GuestsDir   = "guests"
	MarkerFile  = "password-escrow-confirmed"
	CacheFile   = "status-cache.json"
)

// Records is records.json, schema 1, written by backupctl. Unknown keys are ignored.
type Records struct {
	Schema      int                 `json:"schema"`
	Checks      map[string]Check    `json:"checks"`
	RestoreTest *RestoreTest        `json:"restore_test"`
	RepoSizes   map[string]RepoSize `json:"repo_sizes"`
	Runs        []Run               `json:"runs"`
}

// Check is the last integrity check of a source's repository.
type Check struct {
	TS time.Time `json:"ts"`
	OK bool      `json:"ok"`
}

// RestoreTest is the last automatic restore test of a profile.
type RestoreTest struct {
	TS         time.Time `json:"ts"`
	Status     string    `json:"status"` // success, partial, failed
	Unit       string    `json:"unit"`
	SnapshotID string    `json:"snapshot_id"`
}

// RepoSize is the raw data size of a repository after a run.
type RepoSize struct {
	TS    time.Time `json:"ts"`
	Bytes int64     `json:"bytes"`
}

// Run is one of the last runs that backed up data.
type Run struct {
	RunID     string    `json:"run_id"`
	Ended     time.Time `json:"ended"`
	Status    string    `json:"status"`
	DataAdded int64     `json:"data_added"`
}

// ReadRecords reads dir/records.json. A missing file is nil without an error: nothing
// recorded yet.
func ReadRecords(dir string) (*Records, error) {
	b, err := os.ReadFile(filepath.Join(dir, RecordsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Records
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("not readable: %v", err)
	}
	if r.Schema > 1 {
		return nil, fmt.Errorf("written by a newer backupctl (schema %d): this version of restic-pve is too old for it; update restic-pve", r.Schema)
	}
	return &r, nil
}

// Job is a job record jobs/<name>.json, schema 1, with the state of its lock.
type Job struct {
	Name    string     `json:"name"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended"`
	Exit    *int       `json:"exit"`
	RunID   string     `json:"run_id"`
	Running bool       `json:"-"` // its lock is held now
	// Starting: a backup job that runs but has not begun reading yet (see Starting); set by
	// Local, not by ReadJobs
	Starting bool `json:"-"`
}

// NoResult is true for a job that ended without writing its end (killed, out of memory).
func (j Job) NoResult() bool { return !j.Running && j.Ended == nil }

// ReadJobs reads the job records of dir, newest start first, and probes their locks.
// Records that cannot be read are skipped.
func ReadJobs(dir string) []Job {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var jobs []Job
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var j Job
		if json.Unmarshal(b, &j) != nil || j.Name == "" || j.Started.IsZero() {
			continue
		}
		j.Running = j.Ended == nil && Locked(filepath.Join(dir, j.Name+".lock"))
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Started.After(jobs[b].Started) })
	return jobs
}

// LocalStatus is the part of status.json the watch of a local source needs.
type LocalStatus struct {
	RunID       string           `json:"run_id"`
	Started     *time.Time       `json:"started"`
	Updated     *time.Time       `json:"updated"`
	CurrentUnit string           `json:"current_unit"`
	Step        string           `json:"step"`
	Percent     *float64         `json:"percent"`
	BytesDone   *int64           `json:"bytes_done"`
	TotalBytes  *int64           `json:"total_bytes"`
	RunStatus   string           `json:"run_status"`
	Units       map[string]any   `json:"units"`
	DataAdded   map[string]int64 `json:"data_added"`
	IOPaused    bool             `json:"io_paused"`
	Order       []string         `json:"-"` // the units in the order of the run
}

// ReadStatus reads dir/status.json; nil when it is missing or cannot be read.
func ReadStatus(dir string) *LocalStatus {
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		return nil
	}
	var s LocalStatus
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	s.Order = unitOrder(b)
	return &s
}

// unitOrder is the order of the keys of "units": the order of the run (Go maps have none).
func unitOrder(b []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(top["units"]))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return order
		}
		k, _ := tok.(string)
		order = append(order, k)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return order
		}
	}
	return order
}

// UnitStatus is the word status.json holds for a unit: pending, running, success, ...
func (s *LocalStatus) UnitStatus(unit string) string {
	switch v := s.Units[unit].(type) {
	case string:
		return v
	case map[string]any:
		if w, ok := v["status"].(string); ok {
			return w
		}
	}
	return ""
}

// Source is the source name of a unit id: "<source>" or "<source>/<part>".
func Source(unit string) string {
	s, _, _ := strings.Cut(unit, "/")
	return s
}
