package facts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// ReadStorage lists the snapshots of every repository and asks rclone how full the storage
// is. Programs run one after the other through run, never in parallel.
func ReadStorage(c Config, password string, environ []string, run Runner, now time.Time) Storage {
	st := Storage{CheckedAt: now, Repos: map[string][]Snap{}, RepoErr: map[string]string{}}
	env := append(ChildEnv(environ), PasswordEnv+"="+password)
	sources := append([]string{}, c.Files...)
	for _, p := range c.Proxmox {
		sources = append(sources, p.Name)
	}
	for _, s := range sources {
		repo := c.Repository(s)
		r := run(Call{Name: "restic", Args: ResticArgs(repo, "snapshots", "--json"), Env: env, Timeout: SnapshotsTimeout})
		switch {
		case r.Err == nil && r.Code == resticNoRepo:
			st.Repos[s] = nil // no backup made yet
		case r.Err == nil && r.Code == ResticWrongPassword:
			st.RepoErr[s] = WrongPasswordText(repo)
			if st.WrongPassword == "" {
				st.WrongPassword = repo
			}
		case r.Err != nil || r.Code != 0:
			st.RepoErr[s] = r.Problem("restic")
		default:
			snaps, err := parseSnapshots(r.Stdout)
			if err != nil {
				st.RepoErr[s] = "restic gave a snapshot list that cannot be read: " + err.Error()
				continue
			}
			st.Repos[s] = snaps
		}
		if r.Err != nil && st.RepoErr[s] != "" && strings.Contains(st.RepoErr[s], "not installed") {
			break // the same for every repository
		}
	}
	st.About, st.AboutErr = ReadAbout(c.Base, environ, run)
	return st
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
	remote := Remote(base)
	if remote == "" {
		return nil, ""
	}
	r := run(Call{Name: "rclone", Args: []string{"about", remote + ":", "--json"}, Env: ChildEnv(environ), Timeout: AboutTimeout})
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
