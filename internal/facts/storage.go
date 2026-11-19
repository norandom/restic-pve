package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Snap is what the screens keep of a snapshot: no paths, no ids, no host.
type Snap struct {
	Instance string    `json:"instance"`
	Kind     string    `json:"kind"` // "guest-image" or ""
	Time     time.Time `json:"time"`
	Size     *int64    `json:"size,omitempty"` // summary total_bytes_processed
}

// About is what `rclone about --json` reports; any field may be missing.
type About struct {
	Total   *int64 `json:"total,omitempty"`
	Used    *int64 `json:"used,omitempty"`
	Free    *int64 `json:"free,omitempty"`
	Trashed *int64 `json:"trashed,omitempty"`
	Other   *int64 `json:"other,omitempty"`
}

// Storage is what was read from the storage, now or from the cache.
type Storage struct {
	CheckedAt time.Time         `json:"checked_at"`
	Repos     map[string][]Snap `json:"repos"` // per source name
	About     *About            `json:"about"` // nil: the storage does not report
	// not cached: problems of this read, per source and for about
	RepoErr       map[string]string `json:"-"`
	AboutErr      string            `json:"-"`
	WrongPassword string            `json:"-"` // the repository the password in use does not open
}

// Time-outs of the storage calls.
var (
	SnapshotsTimeout = 2 * time.Minute
	AboutTimeout     = time.Minute
	CatConfigTimeout = time.Minute
)

// CacheAge is how old the storage facts may get before a screen reads the storage again.
const CacheAge = 6 * time.Hour

// rcloneArgs are the options restic gets for an rclone repository: the same serve command as
// backupctl's, rclone found through PATH.
var rcloneArgs = []string{"-o", "rclone.program=rclone",
	"-o", "rclone.args=serve restic --stdio --tpslimit 8 --drive-chunk-size 64M --drive-use-trash=false --low-level-retries 20"}

// ResticArgs is the argv after "restic" for a command on repo; it holds no secret.
func ResticArgs(repo string, cmd ...string) []string {
	a := append([]string{}, cmd...)
	a = append(a, "--no-lock", "-r", repo)
	if strings.HasPrefix(repo, "rclone:") {
		a = append(a, rcloneArgs...)
	}
	return a
}

// Exit codes of restic the screens tell apart.
const (
	resticNoRepo        = 10
	ResticWrongPassword = 12
)

// WrongPasswordText is the message when the password in use does not open a repository.
func WrongPasswordText(repo string) string {
	return "The password in use does not open " + repo + ". Take the copy from your password manager " +
		"into .env (RESTIC_PASSWORD=...); never make a new password for existing backups."
}

// Parallel is how many repositories are read at the same time. Each read is one restic and
// one rclone child; three keep a read of many repositories short without loading the host.
const Parallel = 3

// Stopped is the problem of a repository whose read was stopped because the screen was left.
const Stopped = "the read was stopped"

// Sources are the source names of c in the order of backup.toml: local sources, then the
// Proxmox hosts. Every storage read and every list follows this order.
func Sources(c Config) []string {
	sources := append([]string{}, c.Files...)
	for _, p := range c.Proxmox {
		sources = append(sources, p.Name)
	}
	return sources
}

// Each runs do(i) for i in 0..n-1, at most Parallel at a time, in the order of i, in
// goroutines of its own. Before it starts do(i), skip(i) decides whether to leave i out
// (nil: none is left out); a left-out i is not started. done(i) is called for every i that
// was started or left out, one at a time and only from the goroutine that called Each, in
// the order the answers arrive. Each returns when every started do has returned.
func Each(n int, skip func(i int) bool, do func(i int), done func(i int, skipped bool)) {
	type answer struct {
		i       int
		skipped bool
	}
	answers := make(chan answer)
	slots := make(chan struct{}, Parallel)
	go func() {
		for i := range n {
			slots <- struct{}{} // wait for a free slot, then decide: skip sees the newest answers
			if skip != nil && skip(i) {
				<-slots
				answers <- answer{i, true}
				continue
			}
			go func() {
				do(i)
				<-slots
				answers <- answer{i, false}
			}()
		}
	}()
	for range n {
		a := <-answers
		done(a.i, a.skipped)
	}
}

// ReadStorage lists the snapshots of every repository and asks rclone how full the storage
// is. Up to Parallel repositories are read at a time, and rclone about runs alongside; each
// child at nice 19 and idle I/O (Exec). A slow or failing repository does not hold up or fail
// the others; its problem is in RepoErr. The result does not depend on the order in which the
// answers arrive. progress, when not nil, is called first with done 0, then after each
// repository's answer with the read so far (a copy the receiver may keep); it is called only
// from the goroutine that called ReadStorage. When ctx ends, running children are killed,
// no new one starts, and every repository not read has the problem Stopped.
func ReadStorage(ctx context.Context, c Config, password string, environ []string, run Runner, now time.Time,
	progress func(st Storage, done, total int)) Storage {
	if ctx == nil {
		ctx = context.Background()
	}
	st := Storage{CheckedAt: now, Repos: map[string][]Snap{}, RepoErr: map[string]string{}}
	env := append(ChildEnv(environ), PasswordEnv+"="+password)
	sources := Sources(c)
	aboutDone := make(chan struct{})
	var about *About
	var aboutErr string
	go func() {
		defer close(aboutDone)
		about, aboutErr = readAbout(ctx, c.Base, environ, run)
	}()

	// one answer per repository, written by its own goroutine into its own element, read by
	// this goroutine only after done(i) (the channel in Each orders the two)
	type answer struct {
		snaps   []Snap
		known   bool // snaps holds the answer (nil: no repository there yet)
		problem string
		wrongPw bool
	}
	answers := make([]answer, len(sources))
	// missing is the problem "restic is not installed" once a child found it: set by a worker,
	// read by Each's dispatcher and by this goroutine, hence atomic
	var missing atomic.Pointer[string]
	if progress != nil {
		progress(st.copy(), 0, len(sources))
	}
	n := 0
	Each(len(sources),
		func(int) bool { return ctx.Err() != nil || missing.Load() != nil },
		func(i int) {
			repo := c.Repository(sources[i])
			r := run(Call{Name: "restic", Args: ResticArgs(repo, "snapshots", "--json"), Env: env, Timeout: SnapshotsTimeout, Ctx: ctx})
			a := &answers[i]
			switch {
			case r.Err == nil && r.Code == resticNoRepo:
				a.known = true // no backup made yet
			case r.Err == nil && r.Code == ResticWrongPassword:
				a.problem, a.wrongPw = WrongPasswordText(repo), true
			case errors.Is(r.Err, ErrStopped):
				a.problem = Stopped
			case r.Err != nil || r.Code != 0:
				a.problem = r.Problem("restic")
				if r.Err != nil && strings.Contains(a.problem, "not installed") {
					p := a.problem
					missing.CompareAndSwap(nil, &p)
				}
			default:
				snaps, err := parseSnapshots(r.Stdout)
				if err != nil {
					a.problem = "restic gave a snapshot list that cannot be read: " + err.Error()
				} else {
					a.snaps, a.known = snaps, true
				}
			}
		},
		func(i int, skipped bool) {
			s, a := sources[i], answers[i]
			switch {
			case skipped && missing.Load() != nil:
				st.RepoErr[s] = *missing.Load()
			case skipped:
				st.RepoErr[s] = Stopped
			case a.known:
				st.Repos[s] = a.snaps
			default:
				st.RepoErr[s] = a.problem
			}
			n++
			if progress != nil {
				progress(st.copy(), n, len(sources))
			}
		})
	for i, s := range sources { // the first repository of backup.toml the password does not open
		if answers[i].wrongPw && st.WrongPassword == "" {
			st.WrongPassword = c.Repository(s)
		}
	}
	<-aboutDone
	st.About, st.AboutErr = about, aboutErr
	return st
}

// copy is st with maps of its own, for a progress receiver.
func (st Storage) copy() Storage {
	c := st
	c.Repos, c.RepoErr = maps.Clone(st.Repos), maps.Clone(st.RepoErr)
	return c
}

func parseSnapshots(b []byte) ([]Snap, error) {
	var raw []struct {
		Time    time.Time `json:"time"`
		Tags    []string  `json:"tags"`
		Summary *struct {
			Total *int64 `json:"total_bytes_processed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make([]Snap, 0, len(raw))
	for _, s := range raw {
		sn := Snap{Time: s.Time.UTC()}
		for _, t := range s.Tags {
			if v, ok := strings.CutPrefix(t, "instance="); ok {
				sn.Instance = v
			}
			if v, ok := strings.CutPrefix(t, "kind="); ok {
				sn.Kind = v
			}
		}
		if s.Summary != nil {
			sn.Size = s.Summary.Total
		}
		out = append(out, sn)
	}
	return out, nil
}

// ReadAbout asks `rclone about <remote>: --json`. A base that is not rclone does not report
// (nil, ""); a remote that cannot answer gives nil and the reason. The child gets no
// repository password.
func ReadAbout(base string, environ []string, run Runner) (*About, string) {
	return readAbout(context.Background(), base, environ, run)
}

func readAbout(ctx context.Context, base string, environ []string, run Runner) (*About, string) {
	remote := Remote(base)
	if remote == "" {
		return nil, ""
	}
	r := run(Call{Name: "rclone", Args: []string{"about", remote + ":", "--json"}, Env: ChildEnv(environ), Timeout: AboutTimeout, Ctx: ctx})
	if p := r.Problem("rclone"); p != "" {
		return nil, p
	}
	var a About
	if err := json.Unmarshal(r.Stdout, &a); err != nil {
		return nil, "rclone gave an answer that cannot be read: " + err.Error()
	}
	if a.Total == nil && a.Used == nil && a.Free == nil && a.Trashed == nil && a.Other == nil {
		return nil, ""
	}
	return &a, ""
}

// cache is status-cache.json: times, sizes and instance names only, never a secret.
type cache struct {
	Schema int `json:"schema"`
	Storage
}

// LoadCache reads the cache of the state root; ok is false when there is none.
func LoadCache(state string) (Storage, bool) {
	b, err := os.ReadFile(filepath.Join(state, CacheFile))
	if err != nil {
		return Storage{}, false
	}
	var c cache
	if json.Unmarshal(b, &c) != nil || c.Schema != 1 || c.CheckedAt.IsZero() {
		return Storage{}, false
	}
	return c.Storage, true
}

// SaveCache writes the storage facts through a temp file and a rename, 0644. The caller
// saves only a read in which every repository answered.
func SaveCache(state string, st Storage) error {
	c := cache{Schema: 1, Storage: Storage{CheckedAt: st.CheckedAt, Repos: st.Repos, About: st.About}}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(state, "."+CacheFile+".")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if err2 := tmp.Close(); err == nil {
		err = err2
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(state, CacheFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cannot write the cache %s: %v", filepath.Join(state, CacheFile), err)
	}
	return nil
}
