package pieces

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/norandom/restic-pve/internal/testfix"
)

func TestResticArgv(t *testing.T) {
	r := &Restic{Bin: "/usr/local/bin/restic", Env: []string{"RESTIC_REPOSITORY=/r", "RESTIC_PASSWORD_FILE=/p"}}
	cmd := r.Command(context.Background(), "ls", "latest")
	want := []string{"nice", "-n", "19", "ionice", "-c", "3", "/usr/local/bin/restic", "--retry-lock", "30m", "--json", "ls", "latest"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("argv %q, want %q", cmd.Args, want)
	}
	if !slices.Equal(cmd.Env, r.Env) {
		t.Fatalf("env %q, want backupctl's environment unchanged %q", cmd.Env, r.Env)
	}
	none := &Restic{Bin: "restic", Nice: []string{}}
	if got := none.Command(context.Background(), "version").Args; !slices.Equal(got, []string{"restic", "--retry-lock", "30m", "--json", "version"}) {
		t.Fatalf("without nice: %q", got)
	}
}

// fakeRestic is a shell script that stands in for restic: it prints out on stdout, errOut on
// stderr and exits with code.
func fakeRestic(t *testing.T, out, errOut string, code int) *Restic {
	t.Helper()
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o, e := write("out", out), write("err", errOut)
	script := write("restic", fmt.Sprintf("#!/bin/sh\ncat %s\ncat %s >&2\nexit %d\n", o, e, code))
	os.Chmod(script, 0o755)
	return &Restic{Bin: script, Nice: []string{}, Env: []string{"PATH=/usr/bin:/bin"}}
}

func TestResticRunParsesLinesAndErrors(t *testing.T) {
	r := fakeRestic(t, "{\"a\":1}\n{\"a\":2}\n",
		"{\"message_type\":\"error\",\"error\":{\"message\":\"read failed\"},\"during\":\"archival\",\"item\":\"/x\"}\nplain words\n{\"message_type\":\"exit_error\",\"code\":3,\"message\":\"at least one source file could not be read\"}\n", 3)
	var lines []string
	out, err := r.Lines(context.Background(), func(b []byte) error { lines = append(lines, string(b)); return nil }, "backup")
	if err != nil {
		t.Fatal(err)
	}
	if out.Exit != 3 || len(lines) != 2 || lines[1] != `{"a":2}` {
		t.Fatalf("exit %d lines %q", out.Exit, lines)
	}
	if len(out.Messages) != 2 || out.Messages[0] != "/x: read failed" || !strings.Contains(out.Messages[1], "could not be read") {
		t.Fatalf("messages %q", out.Messages)
	}
	if !strings.Contains(out.Stderr, "plain words") {
		t.Fatalf("stderr %q", out.Stderr)
	}
	// a callback error is returned, and the rest of the output is still read (a restic killed
	// by a closed pipe leaves its lock in the repository)
	n := 0
	out, err = r.Lines(context.Background(), func([]byte) error { n++; return errors.New("stop") }, "x")
	if err == nil || n != 1 || out.Exit != 3 {
		t.Fatalf("callback error: %v n %d exit %d", err, n, out.Exit)
	}
	// a non-zero exit is a ResticError for the plain commands
	if _, err := fakeRestic(t, "[]\n", "", 1).Snapshots(context.Background(), "h", nil); !errors.As(err, new(*ResticError)) {
		t.Fatalf("snapshots exit 1: %v", err)
	}
}

func TestListRefusesStrangeTrees(t *testing.T) {
	root := "/state/pieces/web/vm-100"
	snap := `{"id":"` + strings.Repeat("a", 64) + `","paths":["` + root + `"],"hostname":"h","message_type":"snapshot","struct_type":"snapshot"}` + "\n"
	node := func(path, typ string) string {
		return fmt.Sprintf(`{"name":%q,"type":%q,"path":%q,"size":5,"mode":420,"mtime":"2026-11-20T10:00:00.123456789Z","message_type":"node","struct_type":"node"}`+"\n", filepath.Base(path), typ, path)
	}
	ok := snap + node(root, "dir") + node(root+"/disks", "dir") + node(root+"/disks/a", "file")
	if l, err := List(context.Background(), fakeRestic(t, ok, "", 0), strings.Repeat("a", 64), root); err != nil || len(l.Entries) != 3 {
		t.Fatalf("plain tree: %v", err)
	}
	for name, out := range map[string]string{
		"no root (restic ls of a missing path prints only the snapshot)": snap,
		"outside the root": snap + node(root, "dir") + node("/state/pieces/web/vm-1000/x", "file"),
		"a symlink":        snap + node(root, "dir") + node(root+"/l", "symlink"),
		"twice":            snap + node(root, "dir") + node(root+"/a", "file") + node(root+"/a", "file"),
		"dot-dot":          snap + node(root, "dir") + node(root+"/../x", "file"),
		"no snapshot line": node(root, "dir"),
	} {
		if _, err := List(context.Background(), fakeRestic(t, out, "", 0), strings.Repeat("a", 64), root); !errors.Is(err, ErrParent) {
			t.Errorf("%s: %v, want ErrParent", name, err)
		}
	}
}

// backupJSON runs restic backup --json -v and returns the actions per item and the summary.
func backupJSON(t *testing.T, repo *testfix.Repo, args ...string) (map[string]string, map[string]any) {
	t.Helper()
	out, err := repo.Run(append([]string{"backup", "--json", "-v"}, args...)...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	actions := map[string]string{}
	var summary map[string]any
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["message_type"] {
		case "verbose_status":
			if item, _ := m["item"].(string); item != "" && !strings.HasSuffix(item, "/") {
				actions[item], _ = m["action"].(string)
			}
		case "summary":
			summary = m
		}
	}
	if summary == nil {
		t.Fatalf("no summary in\n%s", out)
	}
	return actions, summary
}

// opens watches every directory below root and returns a function that reports the files
// that were opened or read since.
func opens(t *testing.T, root string) func() []string {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	dirs := map[int]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			wd, err := unix.InotifyAddWatch(fd, p, unix.IN_OPEN|unix.IN_ACCESS)
			if err != nil {
				t.Fatal(err)
			}
			dirs[wd] = p
		}
		return nil
	})
	return func() []string {
		var got []string
		buf := make([]byte, 1<<16)
		for {
			n, err := unix.Read(fd, buf)
			if n <= 0 || err != nil {
				return got
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				wd := int32(binary.NativeEndian.Uint32(buf[off:]))
				mask := binary.NativeEndian.Uint32(buf[off+4:])
				l := int(binary.NativeEndian.Uint32(buf[off+12:]))
				name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+l]), "\x00")
				off += unix.SizeofInotifyEvent + l
				if mask&unix.IN_Q_OVERFLOW != 0 {
					t.Fatal("inotify queue overflow")
				}
				if mask&unix.IN_ISDIR == 0 && name != "" {
					got = append(got, filepath.Join(dirs[int(wd)], name))
				}
			}
		}
	}
}

// TestRebuild32000Placeholders: the parent of a 125 GiB disk has 32,000 pieces. Its tree is
// deleted and rebuilt from `restic ls`; restic then takes every piece from the parent without
// opening one. The pieces are 4 KiB here so that the first backup is quick; the count is what
// is measured.
func TestRebuild32000Placeholders(t *testing.T) {
	repo := testfix.NewRepo(t)
	root := filepath.Join(t.TempDir(), "vm-100")
	const n = 32000
	base := time.Date(2026, 11, 20, 10, 0, 0, 123456789, time.UTC)
	type st struct {
		size  int64
		mtime time.Time
	}
	orig := map[string]st{}
	buf := make([]byte, 4096)
	for i := range uint64(n) {
		rel := PiecePath("drive-scsi0", i)
		p := filepath.Join(root, rel)
		if i%GroupSize == 0 {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binary.LittleEndian.PutUint64(buf, i)
		size := 4096 - int(i%7) // sizes differ
		if err := os.WriteFile(p, buf[:size], 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * 1001 * time.Nanosecond) // ns differ
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		orig[p] = st{int64(size), mt}
	}
	_, sum := backupJSON(t, repo, "--host", "test", root)
	parent := sum["snapshot_id"].(string)

	r := &Restic{Bin: repo.Bin, Env: repo.Env()}
	start := time.Now()
	l, err := List(context.Background(), r, parent, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := l.Rebuild(root, PiecesOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("list and rebuild of %d placeholders: %v", files, time.Since(start))
	if files != n {
		t.Fatalf("rebuilt %d placeholders, want %d", files, n)
	}
	for p, want := range orig {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != want.size || !fi.ModTime().Equal(want.mtime) {
			t.Fatalf("%s: size %d mtime %v, want %d %v", p, fi.Size(), fi.ModTime(), want.size, want.mtime)
		}
		var s unix.Stat_t
		if unix.Stat(p, &s) == nil && s.Blocks != 0 {
			t.Fatalf("%s: placeholder holds %d blocks of data", p, s.Blocks)
		}
	}

	opened := opens(t, root)
	actions, sum2 := backupJSON(t, repo, "--host", "test", "--ignore-inode", "--parent", parent, root)
	if got := opened(); len(got) != 0 {
		t.Fatalf("restic opened %d placeholders, e.g. %s", len(got), got[0])
	}
	// control: the watch does see a placeholder that is opened
	probe := filepath.Join(root, PiecePath("drive-scsi0", 31999))
	os.ReadFile(probe)
	if got := opened(); len(got) == 0 || got[0] != probe {
		t.Fatalf("the watch missed an open of %s: %q", probe, got)
	}
	unchanged := 0
	for item, a := range actions {
		if a != "unchanged" {
			t.Fatalf("%s: %s", item, a)
		}
		unchanged++
	}
	if unchanged != n || sum2["files_unmodified"].(float64) != n || sum2["files_new"].(float64) != 0 || sum2["files_changed"].(float64) != 0 {
		t.Fatalf("unchanged %d, summary %v", unchanged, sum2)
	}
}

// layoutSnapshot stores a small snapshot in the piece layout (manifest, config, pieces) at root.
func layoutSnapshot(t *testing.T, repo *testfix.Repo, root, host string, tags []string, sizes map[string]uint64) (string, *Manifest) {
	t.Helper()
	m := testManifest(t, sizes)
	m.Config = []Config{{Name: "qemu-server.conf", SHA256: PieceDigest([]byte("x"))}}
	os.RemoveAll(root)
	os.MkdirAll(filepath.Join(root, ConfigDir), 0o755)
	if err := WriteManifestFile(filepath.Join(root, ManifestName), m); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ConfigPath("qemu-server.conf")), []byte("x"), 0o644)
	for _, d := range m.Devices { // the test manifest's digests are those of zeros
		for i := range d.Pieces {
			p := filepath.Join(root, PiecePath(d.Name, i))
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			os.Truncate(p, int64(PieceLen(d.Size, i)))
		}
	}
	args := []string{"--host", host}
	for _, tg := range tags {
		args = append(args, "--tag", tg)
	}
	_, sum := backupJSON(t, repo, append(args, root)...)
	return sum["snapshot_id"].(string), m
}

func TestLoadParentRules(t *testing.T) {
	repo := testfix.NewRepo(t)
	r := &Restic{Bin: repo.Bin, Env: repo.Env()}
	dir := t.TempDir()
	root := filepath.Join(dir, "vm-100")
	tags := []string{"unit=guest-web", "vmid=100", "kind=guest-image"}
	sizes := map[string]uint64{"drive-scsi0": 2*PieceSize + 7}
	id, m := layoutSnapshot(t, repo, root, "bk", tags, sizes)
	want := ParentWant{Root: root, Host: "bk", Tags: tags, VMID: 100, Alias: "web", Base: id}
	ctx := context.Background()
	p, err := LoadParent(ctx, r, id, want)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != id || p.Manifest.Devices[0].SHA256PT != m.Devices[0].SHA256PT || p.Listing.Snapshot.Hostname != "bk" {
		t.Fatalf("parent %+v", p)
	}
	if e, ok := p.Listing.File(PiecePath("drive-scsi0", 2)); !ok || e.Size != 7 {
		t.Fatalf("last piece %+v %v", e, ok)
	}

	refuse := func(name, id string, w ParentWant, words string) {
		t.Helper()
		_, err := LoadParent(ctx, r, id, w)
		if !errors.Is(err, ErrParent) || !strings.Contains(err.Error(), words) {
			t.Errorf("%s: %v, want ErrParent with %q", name, err, words)
		}
	}
	w := want
	w.Base = strings.Repeat("e", 64)
	refuse("not the record's base", id, w, "base")
	w = want
	w.Host = "other"
	refuse("another host", id, w, "host")
	w = want
	w.Tags = append(slices.Clone(tags), "source=other")
	refuse("a tag missing", id, w, "tag")
	w = want
	w.Alias = "db"
	refuse("another guest's manifest", id, w, "manifest")
	w = want
	w.Root = filepath.Join(dir, "vm-101")
	refuse("another path", id, w, "path")

	// a piece missing from the tree; a stray file; no manifest; a piece of the wrong size
	os.Remove(filepath.Join(root, PiecePath("drive-scsi0", 1)))
	id2, _ := backupJSON2(t, repo, root, "bk", tags)
	w = want
	w.Base = id2
	refuse("a piece missing", id2, w, "does not match")
	layoutSnapshot(t, repo, root, "bk", tags, sizes)
	os.WriteFile(filepath.Join(root, "disks", "stray"), []byte("x"), 0o644)
	id3, _ := backupJSON2(t, repo, root, "bk", tags)
	w.Base = id3
	refuse("a stray file", id3, w, "does not match")
	os.Remove(filepath.Join(root, "disks", "stray"))
	os.Truncate(filepath.Join(root, PiecePath("drive-scsi0", 0)), 10)
	id4, _ := backupJSON2(t, repo, root, "bk", tags)
	w.Base = id4
	refuse("a piece of the wrong size", id4, w, "size")
	os.Remove(filepath.Join(root, ManifestName))
	id5, _ := backupJSON2(t, repo, root, "bk", tags)
	w.Base = id5
	refuse("no manifest", id5, w, "manifest")
	refuse("an id that does not exist", strings.Repeat("0", 64), ParentWant{Root: root, Host: "bk", VMID: 100, Alias: "web"}, "")
}

func backupJSON2(t *testing.T, repo *testfix.Repo, root, host string, tags []string) (string, map[string]any) {
	args := []string{"--host", host}
	for _, tg := range tags {
		args = append(args, "--tag", tg)
	}
	_, sum := backupJSON(t, repo, append(args, root)...)
	return sum["snapshot_id"].(string), sum
}
