package facts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// manyConfig is a config with n local sources r00..r(n-1) on rclone.
func manyConfig(n int) Config {
	c := Config{Base: "rclone:drive:backups"}
	for i := range n {
		c.Files = append(c.Files, fmt.Sprintf("r%02d", i))
	}
	return c
}

// slowStorage answers every repository after delay(repo) with one snapshot named after it,
// and counts how many restic children run at once.
type slowStorage struct {
	delay   func(repo string) time.Duration
	answer  func(repo string) Result // nil: one snapshot
	now     atomic.Int32
	most    atomic.Int32
	mu      sync.Mutex
	started []string
	env     [][]string
}

func (s *slowStorage) run(c Call) Result {
	if c.Name == "rclone" {
		return Result{Stdout: []byte(aboutJSON)}
	}
	repo := c.Args[len(c.Args)-1]
	for i, a := range c.Args {
		if a == "-r" {
			repo = c.Args[i+1]
		}
	}
	n := s.now.Add(1)
	defer s.now.Add(-1)
	for m := s.most.Load(); n > m && !s.most.CompareAndSwap(m, n); m = s.most.Load() {
	}
	s.mu.Lock()
	s.started = append(s.started, repo)
	s.env = append(s.env, append([]string{}, c.Env...))
	s.mu.Unlock()
	d := time.Duration(0)
	if s.delay != nil {
		d = s.delay(repo)
	}
	ctx := c.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return Result{Err: ErrStopped}
	}
	if s.answer != nil {
		return s.answer(repo)
	}
	name := repo[strings.LastIndexByte(repo, '/')+1:]
	return Result{Stdout: []byte(`[{"time": "2026-11-18T05:41:00Z", "tags": ["instance=` + name + `"]}]`)}
}

// TestParallelTime: 16 repositories of 100 ms each take about six rounds of three, not 16
// rounds; never more than three restic children run at once.
func TestParallelTime(t *testing.T) {
	s := &slowStorage{delay: func(string) time.Duration { return 100 * time.Millisecond }}
	start := time.Now()
	st := ReadStorage(context.Background(), manyConfig(16), "pw", nil, s.run, now, nil)
	took := time.Since(start)
	if took < 550*time.Millisecond || took > 1200*time.Millisecond {
		t.Errorf("16 x 100 ms took %v; want about 6 x 100 ms, not 1.6 s", took)
	}
	if m := s.most.Load(); m != Parallel {
		t.Errorf("at most %d children ran at once, want %d", m, Parallel)
	}
	if len(st.Repos) != 16 || len(st.RepoErr) != 0 || st.About == nil {
		t.Errorf("result %d repos %v about %v", len(st.Repos), st.RepoErr, st.About)
	}
	// the slowest group sets the time: one repository of 400 ms among quick ones
	s = &slowStorage{delay: func(r string) time.Duration {
		if strings.HasSuffix(r, "/r01") {
			return 400 * time.Millisecond
		}
		return 50 * time.Millisecond
	}}
	start = time.Now()
	ReadStorage(context.Background(), manyConfig(16), "pw", nil, s.run, now, nil)
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Errorf("one slow repository held up the others: %v", took)
	}
}

// TestParallelOrderAndProblems: whatever the order of the answers, the result is the same as
// one read after the other; one failing and one timing-out repository leave the rest known.
func TestParallelOrderAndProblems(t *testing.T) {
	c := manyConfig(10)
	answer := func(repo string) Result {
		switch {
		case strings.HasSuffix(repo, "/r03"):
			return Result{Code: 1, Stderr: []byte("Fatal: unable to open repository\n")}
		case strings.HasSuffix(repo, "/r07"):
			return Result{Err: ErrTimeout}
		case strings.HasSuffix(repo, "/r05"), strings.HasSuffix(repo, "/r08"):
			return Result{Code: ResticWrongPassword}
		}
		name := repo[strings.LastIndexByte(repo, '/')+1:]
		return Result{Stdout: []byte(`[{"time": "2026-11-18T05:41:00Z", "tags": ["instance=` + name + `"]}]`)}
	}
	var first Storage
	for round := range 8 {
		s := &slowStorage{answer: answer, delay: func(r string) time.Duration {
			i, _ := strconv.Atoi(r[len(r)-2:])
			return time.Duration((i*7+round*3)%10) * 5 * time.Millisecond
		}}
		st := ReadStorage(context.Background(), c, "pw", nil, s.run, now, nil)
		if round == 0 {
			first = st
			continue
		}
		if !reflect.DeepEqual(st, first) {
			t.Fatalf("round %d differs:\n%+v\n%+v", round, st, first)
		}
	}
	if len(first.Repos) != 6 || first.RepoErr["r03"] != "restic said: Fatal: unable to open repository" ||
		first.RepoErr["r07"] != "restic did not answer in time" || first.WrongPassword != "rclone:drive:backups/r05" {
		t.Errorf("problems %+v wrong %q", first.RepoErr, first.WrongPassword)
	}
	// restic missing: every repository says so, not only the ones that ran
	s := &slowStorage{answer: func(string) Result { return Exec(Call{Name: "restic-not-here", Timeout: time.Second}) }}
	st := ReadStorage(context.Background(), c, "pw", nil, s.run, now, nil)
	for _, src := range c.Files {
		if !strings.Contains(st.RepoErr[src], "not installed") {
			t.Errorf("%s: %q", src, st.RepoErr[src])
		}
	}
}

// TestParallelProgress: the progress counts 0..n once each; rows not answered yet show the
// cache, or "not read yet" without one; the cache is written once, after the whole read.
func TestParallelProgress(t *testing.T) {
	m := newMachine(t)
	m.storage()
	type step struct {
		done, total int
		known       int
		notRead     int
	}
	var steps []step
	first := true
	e := m.env()
	e.Progress = func(f Facts, done, total int) {
		s := step{done: done, total: total}
		for _, in := range f.Instances {
			if in.Known {
				s.known++
			}
			if in.Problem == NotRead {
				s.notRead++
			}
		}
		if _, err := os.Stat(filepath.Join(m.state, CacheFile)); err == nil && first {
			t.Errorf("the cache was written before the read ended (step %d)", done)
		}
		steps = append(steps, s)
	}
	if _, err := Read(e); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 || steps[0] != (step{0, 3, 0, 4}) || steps[3].done != 3 || steps[3].known != 5 || steps[3].notRead != 0 {
		t.Errorf("first read without a cache: %+v", steps)
	}
	if _, ok := LoadCache(m.state); !ok {
		t.Fatal("no cache after a complete read")
	}
	// with a cache: every row is known from the first step on
	steps, first = nil, false
	e.Refresh = true
	Read(e)
	for _, s := range steps {
		if s.known != 5 || s.notRead != 0 {
			t.Errorf("with a cache: %+v", steps)
			break
		}
	}
}

// TestParallelStop: leaving the screen during a read stops it at once, kills the running
// children (real processes, through Exec) and keeps the complete cache as it was.
func TestParallelStop(t *testing.T) {
	m := newMachine(t)
	m.storage()
	if _, err := Read(m.env()); err != nil { // a complete cache
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(m.state, CacheFile))
	bin, pids := t.TempDir(), t.TempDir()
	standIn(t, bin, "restic", "echo $$ > "+pids+"/$$\nsleep 30\n")
	standIn(t, bin, "rclone", "echo $$ > "+pids+"/$$\nsleep 30\n")
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	ctx, cancel := context.WithCancel(context.Background())
	e := m.env()
	e.Refresh, e.Ctx = true, ctx
	e.Run = Exec
	e.Environ = []string{"PATH=" + bin + ":/bin:/usr/bin"}
	done := make(chan Facts)
	go func() { f, _ := Read(e); done <- f }()
	for i := 0; i < 400; i++ { // three restic and the rclone about
		if es, _ := os.ReadDir(pids); len(es) == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if es, _ := os.ReadDir(pids); len(es) != 4 {
		t.Fatalf("%d children started, want 3 restic and 1 rclone", len(es))
	}
	start := time.Now()
	cancel()
	var f Facts
	select {
	case f = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the read did not end after the stop")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the stop took %v", took)
	}
	if Running() != 0 {
		t.Errorf("%d children still registered", Running())
	}
	es, _ := os.ReadDir(pids)
	for _, p := range es {
		pid, _ := strconv.Atoi(p.Name())
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("child %d outlived the stop", pid)
		}
	}
	after, _ := os.ReadFile(filepath.Join(m.state, CacheFile))
	if string(after) != string(before) {
		t.Error("a stopped read replaced the complete cache")
	}
	for _, in := range f.Instances {
		if in.Known {
			t.Errorf("%s known after a stopped read", in.Name)
		}
	}
}

// TestParallelBusyHost: while a backup holds its lock and a cache exists, opening reads
// nothing, also with a progress receiver; and the password reaches restic only through
// the environment of each parallel child, never rclone and never an argv.
func TestParallelBusyHost(t *testing.T) {
	m := newMachine(t)
	m.storage()
	Read(m.env())
	write(t, filepath.Join(m.state, JobsDir, "guests.json"), `{"schema": 1, "name": "guests", "started": "2026-11-19T09:00:00Z", "ended": null, "exit": null, "run_id": null}`)
	hold(t, filepath.Join(m.state, JobsDir, "guests.lock"))
	m.calls = nil
	e := m.env()
	e.Now = now.Add(12 * time.Hour)
	called := 0
	e.Progress = func(Facts, int, int) { called++ }
	f, _ := Read(e)
	if len(m.calls) != 0 || !f.Deferred || called != 0 {
		t.Errorf("busy host: %d calls, deferred %v, progress %d", len(m.calls), f.Deferred, called)
	}

	s := &slowStorage{delay: func(string) time.Duration { return 10 * time.Millisecond }}
	pw := "parallel-SECRET-password-123"
	ReadStorage(context.Background(), manyConfig(9), pw, []string{"PATH=/usr/bin", "DISCORD_WEBHOOK_URL=x"}, s.run, now, nil)
	for i, env := range s.env {
		n := 0
		for _, kv := range env {
			if kv == PasswordEnv+"="+pw {
				n++
			}
			if strings.HasPrefix(kv, WebhookEnv) {
				t.Errorf("child %d got the webhook", i)
			}
		}
		if n != 1 {
			t.Errorf("child %d: password %d times in its environment", i, n)
		}
	}
	for _, r := range s.started {
		if strings.Contains(r, pw) {
			t.Error("password in an argv")
		}
	}
}

// TestLocalStarting: the running guest job has no live view of its run yet: getting ready.
func TestLocalStarting(t *testing.T) {
	m := newMachine(t)
	m.records()
	f := Local(m.env(), Config{})
	if len(f.Running) != 1 || f.Running[0].Name != "guests" || !f.Running[0].Starting {
		t.Errorf("running %+v", f.Running)
	}
}
