package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MaxRequest bounds one request line of the plug-in (backup_vm carries the guest config).
const MaxRequest = 4 << 20

// SocketName is the job server's socket inside RunDir.
const SocketName = "provider.sock"

// Server is the job server of one provider job: a Unix socket in a root-only folder that
// accepts exactly one connection, from a process below the vzdump the request started.
type Server struct {
	dir, path string
	l         *net.UnixListener
	// Logf reports a refused connection (stderr of the request, so the vzdump log keeps it).
	Logf func(format string, args ...any)
}

// Listen creates dir (mode 0700; an existing one must be a real folder of this user), removes
// a socket left by an interrupted job and listens on dir/provider.sock (mode 0600). The caller
// holds the host-wide lock, so no other job uses the folder.
func Listen(dir string) (*Server, error) {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case !st.IsDir():
		return nil, fmt.Errorf("%s is not a folder", dir)
	default:
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
			return nil, fmt.Errorf("%s belongs to uid %d", dir, sys.Uid)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, SocketName)
	if _, err := os.Lstat(p); err == nil {
		if err := os.Remove(p); err != nil {
			return nil, fmt.Errorf("remove the socket of an interrupted job: %w", err)
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chmod(p, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return &Server{dir: dir, path: p, l: l, Logf: func(string, ...any) {}}, nil
}

// Path is the socket's path.
func (s *Server) Path() string { return s.path }

// Close stops listening and removes the socket and the folder (when empty).
func (s *Server) Close() error {
	err := s.l.Close()
	_ = os.Remove(s.path)
	_ = os.Remove(s.dir)
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

// Peer is the process on the other end of a connection (SO_PEERCRED).
type Peer struct {
	PID      int
	UID, GID int
}

func peerOf(c *net.UnixConn) (Peer, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Peer{}, err
	}
	if serr != nil {
		return Peer{}, serr
	}
	return Peer{int(cred.Pid), int(cred.Uid), int(cred.Gid)}, nil
}

// Descends reports whether process pid is ancestor or one of its descendants, following the
// parent ids in <proc>/<pid>/stat (at most 64 steps).
func Descends(proc string, pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 0; i++ {
		if pid == ancestor {
			return true
		}
		if pid == 1 {
			return false
		}
		b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
		if err != nil {
			return false
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parentheses
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			return false
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			return false
		}
		if pid, err = strconv.Atoi(f[1]); err != nil {
			return false
		}
	}
	return false
}

// PeerCheck returns the check of a job started as process vzdump: the peer runs as this
// process's user (root in production) and is vzdump or below it.
func PeerCheck(proc string, vzdump int) func(Peer) error {
	return func(p Peer) error {
		if p.UID != os.Geteuid() {
			return fmt.Errorf("peer uid %d is not %d", p.UID, os.Geteuid())
		}
		if !Descends(proc, p.PID, vzdump) {
			return fmt.Errorf("peer process %d is not below the vzdump this request started (%d)", p.PID, vzdump)
		}
		return nil
	}
}

// Accept waits for the one connection whose peer passes check. A connection that fails it is
// answered with a refusal, closed and logged, and the server keeps waiting. After the first
// accepted connection the socket is removed, so nobody else can connect. ctx ends the wait.
func (s *Server) Accept(ctx context.Context, check func(Peer) error) (*Conn, error) {
	stop := context.AfterFunc(ctx, func() { s.l.Close() })
	defer stop()
	for {
		c, err := s.l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		p, err := peerOf(c)
		if err == nil {
			err = check(p)
		}
		if err != nil {
			s.Logf("restic-pve: refused a connection to the job socket: %v", err)
			c.SetWriteDeadline(time.Now().Add(time.Second))
			c.Write([]byte(`{"ok":false,"message":"not a job started by restic-pve"}` + "\n"))
			c.Close()
			continue
		}
		s.l.Close() // one connection per job; the socket is unlinked
		return &Conn{c: c, r: bufio.NewReaderSize(c, 64<<10), Timeout: time.Hour, Peer: p}, nil
	}
}

// Conn is the plug-in's connection: one JSON request line in, one reply line out.
type Conn struct {
	c       *net.UnixConn
	r       *bufio.Reader
	Peer    Peer
	Timeout time.Duration // the longest wait for the next call
}

// Call is one request of the plug-in.
type Call struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// Next reads the next call; io.EOF when the plug-in closed the connection.
func (c *Conn) Next() (Call, error) {
	if c.Timeout > 0 {
		c.c.SetReadDeadline(time.Now().Add(c.Timeout))
	}
	var line []byte
	for {
		part, err := c.r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxRequest {
			return Call{}, fmt.Errorf("request longer than %d bytes", MaxRequest)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return Call{}, err
		}
	}
	var k Call
	dec := json.NewDecoder(strings.NewReader(string(line)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&k); err != nil {
		return Call{}, fmt.Errorf("unreadable request: %v", err)
	}
	if len(k.Args) == 0 {
		k.Args = json.RawMessage("{}")
	}
	return k, nil
}

type reply struct {
	OK      bool   `json:"ok"`
	Result  any    `json:"result,omitempty"`
	Log     string `json:"log,omitempty"`
	Message string `json:"message,omitempty"`
}

func (c *Conn) send(r reply) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	c.c.SetWriteDeadline(time.Now().Add(time.Minute))
	_, err = c.c.Write(append(b, '\n'))
	return err
}

// Reply answers a call with ok, an optional result and an optional info line for the task log.
func (c *Conn) Reply(result any, log string) error {
	return c.send(reply{OK: true, Result: result, Log: log})
}

// Refuse answers a call with ok false; the plug-in dies with "restic-pve: <message>".
func (c *Conn) Refuse(message string) error { return c.send(reply{OK: false, Message: message}) }

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Order is the call order of one job for one guest, as vzdump calls the provider
// (pve-manager PVE/VZDump.pm and qemu-server src/PVE/VZDump/QemuServer.pm): job_init;
// backup_init, backup_get_mechanism, backup_vm_query_incremental, backup_vm; backup_cleanup
// (after a failure possibly twice, and from any point after backup_init);
// backup_handle_log_file; job_cleanup (after an error possibly again). A second guest, a
// call out of this order and an unknown method are refused.
type Order struct{ last string }

var next = map[string][]string{
	"":                            {"job_init"},
	"job_init":                    {"backup_init", "job_cleanup"},
	"backup_init":                 {"backup_get_mechanism", "backup_cleanup"},
	"backup_get_mechanism":        {"backup_vm_query_incremental", "backup_cleanup"},
	"backup_vm_query_incremental": {"backup_vm", "backup_cleanup"},
	"backup_vm":                   {"backup_cleanup"},
	"backup_cleanup":              {"backup_cleanup", "backup_handle_log_file", "job_cleanup"},
	"backup_handle_log_file":      {"job_cleanup"},
	"job_cleanup":                 {"job_cleanup"},
}

// Allow accepts method as the next call or says why not.
func (o *Order) Allow(method string) error {
	for _, m := range next[o.last] {
		if m == method {
			o.last = method
			return nil
		}
	}
	if o.last == "" {
		return fmt.Errorf("%s before job_init", method)
	}
	return fmt.Errorf("%s after %s is not in vzdump's order", method, o.last)
}

// Last is the last call allowed.
func (o *Order) Last() string { return o.last }
