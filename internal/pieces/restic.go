package pieces

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultNice runs restic at the lowest CPU and I/O priority: the backup machine is busy with
// other work, and a slow run is fine where a failed one is not.
var DefaultNice = []string{"nice", "-n", "19", "ionice", "-c", "3"}

// RetryLock is how long restic waits for a lock another restic holds (maintenance, another
// unit) before it gives up.
const RetryLock = "30m"

// maxLine bounds one JSON line of restic's output; a verbose_status or ls line is a few
// hundred bytes.
const maxLine = 1 << 20

// Restic runs restic as a child: `nice -n 19 ionice -c 3 restic --retry-lock 30m --json ...`
// with the environment backupctl passes (repository, password file, rclone), unchanged. Every
// command reads restic's output to its end, also after an error, because a restic killed by a
// closed pipe leaves its lock in the repository.
type Restic struct {
	Bin  string   // the restic program; "" is restic in PATH
	Env  []string // backupctl's restic environment
	Nice []string // nil: DefaultNice; empty: none (tests)
}

// Outcome is what a restic run left besides its stdout.
type Outcome struct {
	Exit     int      // exit code; -1 when it was killed
	Messages []string // the messages of its JSON error and exit_error lines on stderr
	Stderr   string   // the last 8 KiB of the other stderr lines
}

// ResticError is a restic command that ended with an exit code other than 0.
type ResticError struct {
	Command string
	Outcome
}

func (e *ResticError) Error() string {
	msg := strings.Join(e.Messages, "; ")
	if msg == "" {
		msg = strings.TrimSpace(e.Stderr)
	}
	return fmt.Sprintf("restic %s: exit %d: %s", e.Command, e.Exit, msg)
}

// Command returns the restic child for args, not started. It ends restic with SIGINT when
// ctx is done (restic then removes its lock), and kills it 30 seconds later.
func (r *Restic) Command(ctx context.Context, args ...string) *exec.Cmd {
	bin := r.Bin
	if bin == "" {
		bin = "restic"
	}
	prefix := r.Nice
	if prefix == nil {
		prefix = DefaultNice
	}
	argv := append(append(append([]string{}, prefix...), bin, "--retry-lock", RetryLock, "--json"), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = r.Env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

// Raw runs restic with args and hands its stdout to consume, which should read it to the end;
// whatever consume leaves is drained. A non-zero exit is not an error here: it is in the
// Outcome. The error is a failed start or consume's error.
func (r *Restic) Raw(ctx context.Context, consume func(io.Reader) error, args ...string) (Outcome, error) {
	cmd := r.Command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{Exit: -1}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Outcome{Exit: -1}, err
	}
	if err := cmd.Start(); err != nil {
		return Outcome{Exit: -1}, fmt.Errorf("start restic: %w", err)
	}
	var out Outcome
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out.Messages, out.Stderr = readStderr(stderr)
	}()
	cerr := consume(stdout)
	io.Copy(io.Discard, stdout)
	wg.Wait()
	werr := cmd.Wait()
	out.Exit = cmd.ProcessState.ExitCode()
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) && cerr == nil {
		cerr = fmt.Errorf("restic: %w", werr)
	}
	return out, cerr
}

// Lines runs restic with args and hands each stdout line (one JSON value) to line.
func (r *Restic) Lines(ctx context.Context, line func([]byte) error, args ...string) (Outcome, error) {
	return r.Raw(ctx, func(rd io.Reader) error {
		br := bufio.NewReaderSize(rd, 64<<10)
		var first error
		for {
			b, err := readLine(br)
			if len(b) > 0 && first == nil {
				first = line(b)
			}
			if err == io.EOF {
				return first
			}
			if err != nil {
				if first == nil {
					first = err
				}
				return first
			}
		}
	}, args...)
}

// readLine returns the next line without its newline; a line longer than maxLine is an error
// (and is skipped).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			return nil, fmt.Errorf("restic: an output line is longer than %d bytes", maxLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

func readStderr(rd io.Reader) ([]string, string) {
	var msgs []string
	var plain []byte
	br := bufio.NewReaderSize(rd, 64<<10)
	for {
		b, err := readLine(br)
		if len(b) > 0 {
			var m struct {
				Type  string `json:"message_type"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Message string `json:"message"`
				Item    string `json:"item"`
			}
			switch {
			case json.Unmarshal(b, &m) == nil && m.Type == "error":
				msg := m.Error.Message
				if m.Item != "" {
					msg = m.Item + ": " + msg
				}
				msgs = append(msgs, msg)
			case m.Type == "exit_error":
				msgs = append(msgs, m.Message)
			default:
				plain = append(append(plain, b...), '\n')
				if len(plain) > 8<<10 {
					plain = plain[len(plain)-8<<10:]
				}
			}
		}
		if err == io.EOF {
			return msgs, string(plain)
		}
		if err != nil && !strings.Contains(err.Error(), "longer than") {
			return msgs, string(plain)
		}
	}
}

// must turns a non-zero exit into a ResticError.
func must(out Outcome, err error, command string) error {
	if err != nil {
		return err
	}
	if out.Exit != 0 {
		return &ResticError{Command: command, Outcome: out}
	}
	return nil
}

// Snapshot is one snapshot as `restic snapshots --json` and `restic ls --json` list it.
type Snapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Parent   string    `json:"parent"`
	Tree     string    `json:"tree"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
}

// HasTags reports whether s carries every tag of want.
func (s *Snapshot) HasTags(want []string) bool {
	for _, w := range want {
		found := false
		for _, t := range s.Tags {
			found = found || t == w
		}
		if !found {
			return false
		}
	}
	return true
}

// Node is one entry of `restic ls --json`.
type Node struct {
	Name  string      `json:"name"`
	Type  string      `json:"type"`
	Path  string      `json:"path"`
	Size  uint64      `json:"size"`
	Mode  os.FileMode `json:"mode"`
	MTime time.Time   `json:"mtime"`
	ATime time.Time   `json:"atime"`
}

// Snapshots lists the snapshots of host that carry every tag of tags, oldest first.
func (r *Restic) Snapshots(ctx context.Context, host string, tags []string) ([]Snapshot, error) {
	args := []string{"snapshots", "--host", host}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	var snaps []Snapshot
	out, err := r.Raw(ctx, func(rd io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(rd, 256<<20))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &snaps); err != nil {
			return fmt.Errorf("restic snapshots: %w", err)
		}
		return nil
	}, args...)
	if out.Exit != 0 {
		return nil, &ResticError{Command: "snapshots", Outcome: out}
	}
	return snaps, err
}

// Ls lists path in snapshot id recursively: the snapshot first, then each node.
func (r *Restic) Ls(ctx context.Context, id, path string, snap func(Snapshot) error, node func(Node) error) error {
	first := true
	out, err := r.Lines(ctx, func(b []byte) error {
		var kind struct {
			Type string `json:"message_type"`
		}
		if err := json.Unmarshal(b, &kind); err != nil {
			return fmt.Errorf("restic ls: %w", err)
		}
		switch {
		case first && kind.Type == "snapshot":
			first = false
			var s Snapshot
			if err := json.Unmarshal(b, &s); err != nil {
				return fmt.Errorf("restic ls: %w", err)
			}
			return snap(s)
		case !first && kind.Type == "node":
			var n Node
			if err := json.Unmarshal(b, &n); err != nil {
				return fmt.Errorf("restic ls: %w", err)
			}
			return node(n)
		}
		return fmt.Errorf("%w: restic ls: unexpected line %.200s", ErrParent, b)
	}, "ls", "--recursive", id, path)
	if err := must(out, err, "ls"); err != nil {
		return err
	}
	if first {
		return fmt.Errorf("%w: restic ls printed no snapshot", ErrParent)
	}
	return nil
}

// DumpFile returns the file path of snapshot id, at most max bytes.
func (r *Restic) DumpFile(ctx context.Context, id, path string, max int64) ([]byte, error) {
	var b []byte
	out, err := r.Raw(ctx, func(rd io.Reader) error {
		var err error
		b, err = io.ReadAll(io.LimitReader(rd, max+1))
		if err == nil && int64(len(b)) > max {
			err = fmt.Errorf("restic dump %s: larger than %d bytes", path, max)
		}
		return err
	}, "dump", id, path)
	return b, must(out, err, "dump")
}

// BackupArgs is one batch run: `restic backup -v --host <h> --tag <tags> --ignore-inode
// --parent <id> <root>`. Without a parent, --force: restic must not choose a parent of its
// own (it would take the newest snapshot of the host and path, a partial of a crashed run
// among them); the tree then holds only pieces this run wrote.
type BackupArgs struct {
	Root   string
	Host   string
	Tags   []string
	Parent string // "" : no parent
	Force  bool
}

// BackupReport is what restic said about one batch run.
type BackupReport struct {
	Outcome
	Actions map[string]string // per file below the root: new, modified or unchanged
	Twice   []string          // files reported more than once
	Outside []string          // files reported that are not below the root
	Summary *BackupSummary
}

// BackupSummary is restic's summary line.
type BackupSummary struct {
	FilesNew        int    `json:"files_new"`
	FilesChanged    int    `json:"files_changed"`
	FilesUnmodified int    `json:"files_unmodified"`
	DataAdded       uint64 `json:"data_added"`
	SnapshotID      string `json:"snapshot_id"`
}

// Backup runs one batch and parses its verbose_status lines (one per file and folder) and its
// summary as a stream; the error is only a failed start or unreadable output.
func (r *Restic) Backup(ctx context.Context, a BackupArgs) (*BackupReport, error) {
	args := []string{"backup", "-v", "--host", a.Host, "--ignore-inode"}
	if len(a.Tags) > 0 {
		args = append(args, "--tag", strings.Join(a.Tags, ","))
	}
	if a.Parent != "" {
		args = append(args, "--parent", a.Parent)
	}
	if a.Force || a.Parent == "" {
		args = append(args, "--force")
	}
	args = append(args, a.Root)
	rep := &BackupReport{Actions: map[string]string{}}
	out, err := r.Lines(ctx, func(b []byte) error {
		var m struct {
			Type   string `json:"message_type"`
			Action string `json:"action"`
			Item   string `json:"item"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("restic backup: %w", err)
		}
		switch m.Type {
		case "verbose_status":
			if m.Item == "" || strings.HasSuffix(m.Item, "/") {
				return nil // scan_finished, folders
			}
			rel, ok := relPath(a.Root, m.Item)
			if !ok || rel == "" {
				rep.Outside = append(rep.Outside, m.Item)
				return nil
			}
			if _, dup := rep.Actions[rel]; dup {
				rep.Twice = append(rep.Twice, rel)
			}
			rep.Actions[rel] = m.Action
		case "summary":
			var s BackupSummary
			if err := json.Unmarshal(b, &s); err != nil {
				return fmt.Errorf("restic backup: summary: %w", err)
			}
			rep.Summary = &s
		}
		return nil
	}, args...)
	rep.Outcome = out
	return rep, err
}

// DiffChange is one changed path of `restic diff --json`, relative to the root (folders end
// in "/"); Modifier is restic's: + added, - removed, M content, T type, U metadata.
type DiffChange struct {
	Rel      string
	Modifier string
}

// Diff lists what changed from snapshot a to b below root. restic 0.19.1 lists content and
// type changes, additions and removals; a change of metadata alone is not listed.
func (r *Restic) Diff(ctx context.Context, a, b, root string) ([]DiffChange, error) {
	var out []DiffChange
	o, err := r.Lines(ctx, func(line []byte) error {
		var m struct {
			Type     string `json:"message_type"`
			Path     string `json:"path"`
			Modifier string `json:"modifier"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("restic diff: %w", err)
		}
		if m.Type != "change" {
			return nil
		}
		p := strings.TrimSuffix(m.Path, "/")
		rel, ok := relPath(root, p)
		if !ok {
			rel = "../" + m.Path // outside the root: C3 refuses it
		} else if strings.HasSuffix(m.Path, "/") {
			rel += "/"
		}
		out = append(out, DiffChange{Rel: rel, Modifier: m.Modifier})
		return nil
	}, "diff", a, b)
	return out, must(o, err, "diff")
}

// TagSet replaces the tags of snapshot id and returns the new snapshot's id (restic writes a
// new snapshot and removes the old one).
func (r *Restic) TagSet(ctx context.Context, id string, tags []string) (string, error) {
	args := []string{"tag"}
	for _, t := range tags {
		args = append(args, "--set", t)
	}
	newID := ""
	o, err := r.Lines(ctx, func(b []byte) error {
		var m struct {
			Type string `json:"message_type"`
			Old  string `json:"old_snapshot_id"`
			New  string `json:"new_snapshot_id"`
		}
		if json.Unmarshal(b, &m) == nil && m.Type == "changed" && m.Old == id {
			newID = m.New
		}
		return nil
	}, append(args, id)...)
	if err := must(o, err, "tag"); err != nil {
		return "", err
	}
	if newID == "" {
		return "", fmt.Errorf("restic tag: no new snapshot for %s", short(id))
	}
	return newID, nil
}

// Forget removes the snapshots ids (restic ignores an id that does not exist).
func (r *Restic) Forget(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	o, err := r.Lines(ctx, func([]byte) error { return nil }, append([]string{"forget"}, ids...)...)
	return must(o, err, "forget")
}
