package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveJob answers the plug-in's calls in order with goodReplies' results, refusing what
// Order refuses. It returns the methods seen.
func serveJob(t *testing.T, s *Server, check func(Peer) error) (methods func() []string, done chan error) {
	var mu sync.Mutex
	var seen []string
	done = make(chan error, 1)
	go func() {
		c, err := s.Accept(context.Background(), check)
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		var o Order
		replies := goodReplies()
		for {
			k, err := c.Next()
			if err != nil {
				done <- err
				return
			}
			mu.Lock()
			seen = append(seen, k.Method)
			mu.Unlock()
			if err := o.Allow(k.Method); err != nil {
				c.Refuse(err.Error())
				continue
			}
			var r struct {
				Result any    `json:"result"`
				Log    string `json:"log"`
			}
			if text, ok := replies[k.Method]; ok {
				json.Unmarshal([]byte(text), &r)
			}
			c.Reply(r.Result, r.Log)
		}
	}()
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }, done
}

func TestJobServerServesTheDriver(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "restic-pve")
	s, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for p, want := range map[string]os.FileMode{dir: 0o700 | os.ModeDir, s.Path(): 0o600 | os.ModeSocket} {
		if st, err := os.Lstat(p); err != nil || st.Mode() != want {
			t.Fatalf("%s: %v %v", p, st.Mode(), err)
		}
	}
	methods, done := serveJob(t, s, PeerCheck("/proc", os.Getpid()))
	out := runDriver(t, s.Path())
	if !strings.HasSuffix(out, "job ok, guest ok\n") {
		t.Fatalf("driver:\n%s", out)
	}
	if err := <-done; err == nil {
		t.Fatal("served past the end of the connection")
	}
	want := "job_init backup_init backup_get_mechanism backup_vm_query_incremental backup_vm backup_cleanup backup_handle_log_file job_cleanup"
	if got := strings.Join(methods(), " "); got != want {
		t.Fatalf("calls %s", got)
	}
	if _, err := os.Lstat(s.Path()); err == nil {
		t.Fatal("socket still there after the one connection was accepted")
	}
	s.Close()
	if _, err := os.Lstat(dir); err == nil {
		t.Fatal("job folder left")
	}
}

func TestJobServerRefusesForeignProcesses(t *testing.T) {
	s, err := Listen(filepath.Join(t.TempDir(), "restic-pve"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mu sync.Mutex
	var logged []string
	s.Logf = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, f)
	}
	// "vzdump" is a sleeping child; the test process is not below it
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleeper.Process.Kill()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := s.Accept(ctx, PeerCheck("/proc", sleeper.Process.Pid))
		res <- err
	}()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("unix", s.Path())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if line != `{"ok":false,"message":"not a job started by restic-pve"}`+"\n" {
			t.Fatalf("refusal %q", line)
		}
	}
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("accept after cancel: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 2 || !strings.Contains(logged[0], "refused a connection to the job socket") {
		t.Fatalf("not logged: %v", logged)
	}
}

func TestDescends(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	if !Descends("/proc", child.Process.Pid, os.Getpid()) || !Descends("/proc", os.Getpid(), os.Getpid()) {
		t.Fatal("a child is not below its parent")
	}
	if Descends("/proc", os.Getpid(), child.Process.Pid) || os.Getpid() != 1 && Descends("/proc", 1, os.Getpid()) || Descends("/proc", 999999999, os.Getpid()) {
		t.Fatal("a parent is below its child")
	}
	// comm with spaces and parentheses
	proc := t.TempDir()
	for pid, stat := range map[string]string{"300": "300 (a) b (c) S 200 1 1", "200": "200 (vzdump) S 100 1 1", "100": "100 (sh) S 1 1 1"} {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(stat), 0o644)
	}
	if !Descends(proc, 300, 200) || Descends(proc, 200, 300) {
		t.Fatal("stat parsing")
	}
}

func TestListenCleansAStaleSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "restic-pve")
	os.Mkdir(dir, 0o755)
	stale := filepath.Join(dir, SocketName)
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // a socket file nobody listens on: the job before was killed
	s, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", st.Mode())
	}
	// not a folder: refused
	other := filepath.Join(t.TempDir(), "x")
	os.Symlink(t.TempDir(), other)
	if _, err := Listen(other); err == nil {
		t.Fatal("a symlink was taken as the job folder")
	}
}

func TestOrder(t *testing.T) {
	ok := [][]string{
		{"job_init", "backup_init", "backup_get_mechanism", "backup_vm_query_incremental", "backup_vm", "backup_cleanup", "backup_handle_log_file", "job_cleanup"},
		// a failure after success: backup_cleanup twice; job_cleanup again after an error
		{"job_init", "backup_init", "backup_get_mechanism", "backup_vm_query_incremental", "backup_vm", "backup_cleanup", "backup_cleanup", "backup_handle_log_file", "job_cleanup", "job_cleanup"},
		{"job_init", "backup_init", "backup_cleanup", "backup_handle_log_file", "job_cleanup"},
		{"job_init", "job_cleanup"},
	}
	for _, seq := range ok {
		var o Order
		for _, m := range seq {
			if err := o.Allow(m); err != nil {
				t.Errorf("%v: %v", seq, err)
			}
		}
	}
	bad := [][]string{
		{"backup_init"},
		{"job_init", "backup_vm"},
		{"job_init", "backup_init", "backup_get_mechanism", "backup_vm_query_incremental", "backup_vm", "backup_vm"},
		{"job_init", "backup_init", "backup_get_mechanism", "backup_vm_query_incremental", "backup_vm", "backup_cleanup", "backup_init"},
		{"job_init", "restore_vm_init"},
		{"job_init", "job_init"},
	}
	for _, seq := range bad {
		var o Order
		var err error
		for _, m := range seq {
			if err = o.Allow(m); err != nil {
				break
			}
		}
		if err == nil {
			t.Errorf("%v accepted", seq)
		}
	}
}

// The peer check alone, without Perl: another uid is refused, a process that is not below
// our vzdump is refused, vzdump and its descendants pass.
func TestPeerCheck(t *testing.T) {
	proc := t.TempDir()
	for pid, stat := range map[string]string{"300": "300 (perl) S 200 1 1", "200": "200 (vzdump) S 100 1 1",
		"100": "100 (restic-pve) S 1 1 1", "400": "400 (other) S 1 1 1"} {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(stat), 0o644)
	}
	check := PeerCheck(proc, 200)
	me := os.Geteuid()
	for _, c := range []struct {
		p    Peer
		want string
	}{
		{Peer{PID: 300, UID: me}, ""},
		{Peer{PID: 200, UID: me}, ""},
		{Peer{PID: 300, UID: me + 1}, "peer uid"},
		{Peer{PID: 400, UID: me}, "is not below the vzdump this request started (200)"},
		{Peer{PID: 100, UID: me}, "is not below"},
		{Peer{PID: 999, UID: me}, "is not below"},
	} {
		err := check(c.p)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v", c.p, err)
		}
	}
}
