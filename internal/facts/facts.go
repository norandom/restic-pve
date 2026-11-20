package facts

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Env is what a read sees of the machine.
type Env struct {
	Getenv  func(string) string
	Environ []string // the whole environment, for the children's allow list
	Run     Runner
	Now     time.Time
	Refresh bool // the admin pressed r: read the storage whatever the cache says
	// CacheOnly never reads the storage: an open screen reads its local files again when they
	// change, and shows the cached storage facts whatever their age.
	CacheOnly bool
	Version   string
	// Ctx stops a storage read early (the screen was left); nil: it runs to its end.
	Ctx context.Context
	// Progress, when not nil, is told while the storage is read: first with done 0, then after
	// each repository's answer, with the facts so far (the answered repositories, the cache
	// or "not read yet" for the others). It is called from the goroutine that called Read.
	Progress func(f Facts, done, total int)
}

// Facts is everything the status, keys and space screens show.
type Facts struct {
	Config      Config
	Instances   []Instance
	RestoreTest *RestoreTest // newest of the local and the guest profile
	Drill       *time.Time   // nil in stage 1: no drill record exists yet
	Running     []Job
	LastJobs    []Job // ended, newest first
	Next        []NextRun
	Space       SpaceFacts
	Alerts      bool
	Proof       *time.Time // the password copy was proven for the password in use, then
	Version     string
	CheckedAt   time.Time // when the storage was read; zero: never
	FromCache   bool      // the storage facts come from the cache
	Deferred    bool      // a backup runs: the storage read waits, the cache is shown
	Errors      []FactError
	Reads       map[string]GuestReads // per alias: guests that read only changed blocks
}

// Instance is one local source instance or one guest.
type Instance struct {
	Source, Name  string
	Kind          string // "files" or "guest"
	Newest        *time.Time
	NewestChecked *time.Time
	Check         *Check // the last check of its repository; nil: none recorded yet
	Interval      time.Duration
	Size          *int64 // guests: total_bytes_processed of the newest image
	Known         bool   // the storage answered for its repository
	Problem       string // why it is not known
}

// NextRun is the next scheduled run of a profile; zero At: not known.
type NextRun struct {
	Profile string
	At      time.Time
}

// SpaceFacts are the numbers of the space view.
type SpaceFacts struct {
	Remote    string // "" when the base is not rclone
	About     *About // nil: the storage does not report (or was not reachable)
	AboutErr  string
	RepoSizes map[string]RepoSize // per source
	Runs      []Run               // the recorded runs of both profiles, oldest first
	GuestSize map[string]*int64   // per guest alias: its newest image
	Work      *WorkSpace          // the pieces runs' working space; nil: none on this machine
}

// FactError is something that could not be read, with what to do.
type FactError struct {
	What, Text string
}

// NotRead is the problem of an instance whose repository was not read (yet).
const NotRead = "the storage was not read"

// Read collects the facts. It reads the storage only when the cache is older than six hours
// or older than a records file, or when e.Refresh is set; while a backup runs, an existing
// cache is shown instead and the read waits (Deferred). The error is a MissingError or a
// ConfigError: the command cannot work at all.
func Read(e Env) (Facts, error) {
	if err := Require(e.Getenv, StateEnv, ConfigEnv); err != nil {
		return Facts{}, err
	}
	state := e.Getenv(StateEnv)
	c, err := LoadConfig(e.Getenv(ConfigEnv), e.Getenv(BaseEnv))
	if err != nil {
		return Facts{}, err
	}
	f := Local(e, c)
	password := e.Getenv(PasswordEnv)
	cached, hasCache := LoadCache(state)
	newestRecords := mtime(filepath.Join(state, RecordsFile))
	if g := mtime(filepath.Join(state, GuestsDir, RecordsFile)); g.After(newestRecords) {
		newestRecords = g
	}
	fresh := hasCache && e.Now.Sub(cached.CheckedAt) < CacheAge && !cached.CheckedAt.Before(newestRecords) &&
		covers(cached, c)
	var st *Storage
	switch {
	case fresh && !e.Refresh, hasCache && e.CacheOnly:
		st, f.FromCache = &cached, true
	case e.CacheOnly:
	case hasCache && !e.Refresh && len(f.Running) > 0:
		st, f.FromCache, f.Deferred = &cached, true, true
	case password == "":
		f.Errors = append(f.Errors, FactError{"storage", (&MissingError{PasswordEnv}).Error()})
	default:
		var progress func(Storage, int, int)
		if e.Progress != nil {
			recL, recG := records(state, &f)
			progress = func(part Storage, done, total int) {
				p := f
				view := part
				if hasCache { // the repositories not answered yet show what the cache knows
					view.Repos = maps.Clone(cached.Repos)
					maps.Copy(view.Repos, part.Repos)
					view.About, view.CheckedAt = cached.About, cached.CheckedAt
				}
				view.RepoErr = map[string]string{}
				for _, s := range Sources(c) {
					_, answered := part.Repos[s]
					if problem, bad := part.RepoErr[s]; bad {
						view.RepoErr[s], answered = problem, true
						delete(view.Repos, s)
					}
					if _, have := view.Repos[s]; !answered && !have {
						view.RepoErr[s] = NotRead
					}
				}
				p.CheckedAt, p.FromCache = time.Time{}, hasCache
				if hasCache {
					p.CheckedAt = cached.CheckedAt
				}
				p.Space.About, p.Space.AboutErr = view.About, ""
				p.Instances, p.Space.GuestSize = instances(c, &view, recL, recG, e.Now)
				e.Progress(p, done, total)
			}
		}
		read := ReadStorage(e.Ctx, c, password, e.Environ, e.Run, e.Now, progress)
		stopped := e.Ctx != nil && e.Ctx.Err() != nil
		// only a complete read replaces the cache: every repository answered, none stopped
		if len(read.RepoErr) == 0 && !stopped {
			if err := SaveCache(state, read); err != nil {
				f.Errors = append(f.Errors, FactError{"cache", err.Error()})
			}
		}
		if read.WrongPassword != "" {
			f.Errors = append(f.Errors, FactError{"password", WrongPasswordText(read.WrongPassword)})
		}
		if len(read.RepoErr) > 0 && hasCache {
			f.CheckedAt = cached.CheckedAt // the last time the storage answered
		}
		st = &read
	}
	if st != nil {
		if f.CheckedAt.IsZero() {
			f.CheckedAt = st.CheckedAt
		}
		f.Space.About, f.Space.AboutErr = st.About, st.AboutErr
	}
	recL, recG := records(state, &f)
	f.Instances, f.Space.GuestSize = instances(c, st, recL, recG, e.Now)
	return f, nil
}

// Local collects the facts that come from files of this machine only: schedule, records,
// jobs, locks, alerts and the proof. It never calls a program.
func Local(e Env, c Config) Facts {
	state := e.Getenv(StateEnv)
	f := Facts{Config: c, Version: e.Version, Alerts: e.Getenv(WebhookEnv) != ""}
	f.Space.Remote = c.Remote()
	recL, recG := records(state, &f)
	for _, r := range []*Records{recL, recG} {
		if r == nil {
			continue
		}
		if r.RestoreTest != nil && (f.RestoreTest == nil || r.RestoreTest.TS.After(f.RestoreTest.TS)) {
			f.RestoreTest = r.RestoreTest
		}
		for s, sz := range r.RepoSizes {
			if f.Space.RepoSizes == nil {
				f.Space.RepoSizes = map[string]RepoSize{}
			}
			f.Space.RepoSizes[s] = sz
		}
		f.Space.Runs = append(f.Space.Runs, r.Runs...)
	}
	sort.Slice(f.Space.Runs, func(a, b int) bool { return f.Space.Runs[a].Ended.Before(f.Space.Runs[b].Ended) })
	for _, j := range ReadJobs(filepath.Join(state, JobsDir)) {
		if j.Running {
			j.Starting = Starting(state, j)
			f.Running = append(f.Running, j)
		} else {
			f.LastJobs = append(f.LastJobs, j)
		}
	}
	if len(f.Running) == 0 { // a run without a job record (started before the records existed)
		for _, p := range []struct{ name, dir string }{{ProfileLocal, state}, {ProfileGuests, filepath.Join(state, GuestsDir)}} {
			if Locked(filepath.Join(p.dir, RunLock)) {
				j := Job{Name: p.name, Running: true}
				if s := ReadStatus(p.dir); s != nil && s.Started != nil {
					j.Started = *s.Started
				}
				f.Running = append(f.Running, j)
			}
		}
	}
	profiles := []string{}
	if len(c.Files) > 0 {
		profiles = append(profiles, ProfileLocal)
	}
	if len(c.Proxmox) > 0 {
		profiles = append(profiles, ProfileGuests)
	}
	for _, p := range profiles {
		n := NextRun{Profile: p}
		if cr, ok := c.Schedule[p]; ok {
			n.At = cr.Next(e.Now)
		}
		f.Next = append(f.Next, n)
	}
	f.Proof = ProofDate(state, e.Getenv(PasswordEnv))
	f.Reads, f.Space.Work = guestReads(state, c, recG), workSpace(state, c)
	return f
}

// records reads both records files; a file that cannot be read becomes a FactError once.
func records(state string, f *Facts) (local, guests *Records) {
	var errs []FactError
	local, err := ReadRecords(state)
	if err != nil {
		errs = append(errs, FactError{"records", filepath.Join(state, RecordsFile) + ": " + err.Error() +
			"; its facts show as not recorded, and the next run writes it again"})
	}
	guests, err = ReadRecords(filepath.Join(state, GuestsDir))
	if err != nil {
		errs = append(errs, FactError{"records", filepath.Join(state, GuestsDir, RecordsFile) + ": " + err.Error() +
			"; its facts show as not recorded, and the next guest run writes it again"})
	}
	for _, e := range errs {
		dup := false
		for _, have := range f.Errors {
			dup = dup || have == e
		}
		if !dup {
			f.Errors = append(f.Errors, e)
		}
	}
	return local, guests
}

// covers is true when the cache holds every repository of the config.
func covers(st Storage, c Config) bool {
	for _, s := range c.Files {
		if _, ok := st.Repos[s]; !ok {
			return false
		}
	}
	for _, p := range c.Proxmox {
		if _, ok := st.Repos[p.Name]; !ok {
			return false
		}
	}
	return true
}

func mtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// instances turns the snapshots into one entry per local source instance and per guest.
// st nil: the storage was not read, every instance is unknown.
func instances(c Config, st *Storage, recL, recG *Records, now time.Time) ([]Instance, map[string]*int64) {
	check := func(r *Records, source string) *Check {
		if r == nil {
			return nil
		}
		if ch, ok := r.Checks[source]; ok {
			return &ch
		}
		return nil
	}
	known := func(source string) (bool, string) {
		if st == nil {
			return false, NotRead
		}
		if p, bad := st.RepoErr[source]; bad {
			return false, p
		}
		_, ok := st.Repos[source]
		return ok, ""
	}
	var out []Instance
	for _, s := range c.Files {
		ok, problem := known(s)
		by := map[string][]Snap{}
		if ok {
			for _, sn := range st.Repos[s] {
				inst := sn.Instance
				if inst == "" {
					inst = s
				}
				by[inst] = append(by[inst], sn)
			}
		}
		names := make([]string, 0, len(by))
		for n := range by {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			names = []string{s}
		}
		for _, n := range names {
			in := Instance{Source: s, Name: n, Kind: "files", Check: check(recL, s), Known: ok, Problem: problem,
				Interval: c.Interval(ProfileLocal, now)}
			if n != s {
				in.Name = s + "/" + n
			}
			in.Newest, in.NewestChecked, _ = newest(by[n], in.Check)
			out = append(out, in)
		}
	}
	sizes := map[string]*int64{}
	for _, p := range c.Proxmox {
		ok, problem := known(p.Name)
		for _, g := range p.Guests {
			var snaps []Snap
			if ok {
				for _, sn := range st.Repos[p.Name] {
					if sn.Kind == "guest-image" && sn.Instance == g.Alias {
						snaps = append(snaps, sn)
					}
				}
			}
			in := Instance{Source: p.Name, Name: g.Alias, Kind: "guest", Check: check(recG, p.Name), Known: ok,
				Problem: problem, Interval: c.Interval(ProfileGuests, now)}
			in.Newest, in.NewestChecked, in.Size = newest(snaps, in.Check)
			sizes[g.Alias] = in.Size
			out = append(out, in)
		}
	}
	return out, sizes
}

// newest returns the newest snapshot, the newest one not newer than a passed check, and the
// size of the newest.
func newest(snaps []Snap, ch *Check) (n, checked *time.Time, size *int64) {
	for _, s := range snaps {
		t := s.Time
		if n == nil || t.After(*n) {
			n, size = &t, s.Size
		}
		if ch != nil && ch.OK && !t.After(ch.TS) && (checked == nil || t.After(*checked)) {
			checked = &t
		}
	}
	return n, checked, size
}

// Fingerprint is the one-way value of the password-copy marker, as backupctl's setup writes
// it: sha256 of "restic-backup escrow:" and the password, in hex.
func Fingerprint(password []byte) string {
	h := sha256.New()
	h.Write([]byte("restic-backup escrow:"))
	h.Write(password)
	return hex.EncodeToString(h.Sum(nil))
}

// ProofDate is the time of the last passed proof for the password in use: the marker's
// mtime when it holds the fingerprint of that password; nil otherwise.
func ProofDate(state, password string) *time.Time {
	if password == "" {
		return nil
	}
	path := filepath.Join(state, MarkerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	want := Fingerprint([]byte(password))
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(b))), []byte(want)) != 1 {
		return nil
	}
	t := mtime(path)
	return &t
}
