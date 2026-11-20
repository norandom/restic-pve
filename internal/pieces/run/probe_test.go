package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/changes"
)

const token = "0123456789abcdef0123456789abcdef"

// TestMain: with FAKE_HOST set the test binary is "ssh" and the Proxmox host behind it: it
// answers "changes <vmid> probe" with a probe stream, reads the ack and exits as the host
// side would. FAKE_HOST names the case; FAKE_STATE the folder for what it saw.
func TestMain(m *testing.M) {
	if c := os.Getenv("FAKE_HOST"); c != "" {
		os.Exit(fakeHost(c, os.Getenv("FAKE_STATE"), os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeHost(c, state string, args []string) int {
	if c == "image" {
		return imageHost(state, args)
	}
	os.WriteFile(filepath.Join(state, "ssh.argv"), []byte(strings.Join(args, " ")+"\n"), 0o644)
	os.WriteFile(filepath.Join(state, "ssh.env"), []byte(strings.Join(os.Environ(), "\n")), 0o644)
	log := func(s string) { fmt.Fprintln(os.Stderr, s) }
	if c == "notready" {
		log("restic-pve: error: plug-in not ready: changed reads are off on this host; run restic-pve install on the Proxmox host and answer yes to changed reads")
		return 79
	}
	log("INFO: starting new backup job: vzdump 100 --storage restic-pve --mode snapshot")
	log("INFO: issuing guest-agent 'fs-freeze' command")
	log("INFO: drive-scsi0: dirty-bitmap status: OK (12.0 MiB of 64.0 MiB dirty)")
	log("INFO: restic-pve: drive-scsi0: record reuse, 3 of 16 pieces of 4 MiB changed")
	out := bufio.NewWriter(os.Stdout)
	enc := changes.NewEncoder(out)
	enc.Header(changes.Header{Token: token, VMID: 100, Mode: "probe", PVE: "9.2.24", Devices: []changes.Device{
		{ID: 1, Name: "drive-efidisk0", Size: 540672, Bitmap: "new"},
		{ID: 2, Name: "drive-scsi0", Size: 64 << 20, Bitmap: "reuse", BitmapName: "snapshot-access:restic-pve"}}})
	enc.Config("qemu-server.conf", []byte("name: web\n"))
	enc.Map(changes.Map{Dev: 1})
	if c == "cut" {
		out.Flush()
		return 255
	}
	enc.Map(changes.NewMap(2, 64<<20, []uint64{0, 4, 5}))
	enc.End()
	out.Flush()
	a, err := changes.ReadAck(os.Stdin, token, changes.VerbProbe)
	os.WriteFile(filepath.Join(state, "ack"), []byte(fmt.Sprintf("%s %v\n", a.Verb, err)), 0o644)
	if err != nil {
		log("ERROR: Backup of VM 100 failed - the backup machine did not confirm the backup")
		return 2
	}
	return 0
}

type fixture struct {
	t          *testing.T
	dir, state string
	job        *Job
}

func newFixture(t *testing.T, c string) *fixture {
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, state: filepath.Join(dir, "state")}
	for _, d := range []string{"bin", "seen", "state"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
	}
	exe, _ := os.Executable()
	os.WriteFile(filepath.Join(dir, "bin", "ssh"), []byte("#!/bin/sh\nFAKE_HOST="+c+" FAKE_STATE="+filepath.Join(dir, "seen")+" exec "+exe+" \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "bin", "restic"), []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "seen", "restic-ran")+"\n"), 0o755)
	t.Setenv("PATH", filepath.Join(dir, "bin")+":/usr/bin:/bin")
	t.Setenv("RESTIC_PASSWORD", "made-up-not-a-secret")
	j := `{"schema":1,"mode":"probe","vmid":100,"alias":"web","source":"site1","ssh_config":"/config/ssh_config","ssh_host":"pve",
"restic_host":"backup","tags":["source=site1","unit=guest-web","kind=guest-image","vmid=100"],"info":{"vmid":100},
"view_dir":"` + filepath.Join(dir, "view") + `","progress_interval_s":30,"deadline":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","full_check_every":7}`
	p := filepath.Join(dir, "job.json")
	os.WriteFile(p, []byte(j), 0o600)
	job, err := LoadJob(p)
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
	return f
}

func (f *fixture) seen(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.dir, "seen", name))
	return string(b), err == nil
}

func (f *fixture) probe() (Result, []string) {
	var out strings.Builder
	r := Probe(t0ctx(), Env{State: f.state, Stderr: &out}, f.job)
	return r, strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
}

func TestProbe(t *testing.T) {
	f := newFixture(t, "ok")
	res, lines := f.probe()
	if res.Code != 0 || res.Kind != "probe" || res.Status != "ok" || res.ReadBytes != 0 || res.StoredBytes != 0 || res.Snapshot != nil {
		t.Fatalf("%+v\n%s", res, strings.Join(lines, "\n"))
	}
	if res.Message != "probe: drive-efidisk0 record new (the next run reads it in full); drive-scsi0 record reuse, 12 MiB changed of 64 MiB; nothing was read or stored" {
		t.Fatalf("message %q", res.Message)
	}
	if fmt.Sprint(res.Disks) != "[{drive-efidisk0 new 540672} {drive-scsi0 reuse 12582912}]" || res.SkippedBytes != 52<<20 {
		t.Fatalf("disks %v skipped %d", res.Disks, res.SkippedBytes)
	}
	// the last line is the result; the log gives the consistency, and no line of the probe
	// is a warning (all info)
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last["type"] != "result" || last["kind"] != "probe" {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
	if lines[0] != `{"type":"consistency","value":"application","detail":"guest-agent fs-freeze issued without error"}` || len(lines) != 2 {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	// the request and the ack
	if argv, _ := f.seen("ssh.argv"); argv != "-F /config/ssh_config -T -o BatchMode=yes pve changes 100 probe\n" {
		t.Fatalf("ssh argv %q", argv)
	}
	if ack, _ := f.seen("ack"); ack != "probe <nil>\n" {
		t.Fatalf("ack %q", ack)
	}
	if env, _ := f.seen("ssh.env"); strings.Contains(env, "RESTIC_PASSWORD") {
		t.Fatal("ssh got the password")
	}
	// the record state: none, with this job
	if r := pieces.LoadRecord(pieces.RecordFile(f.state, "web"), 100); r.State != pieces.RecordNone || r.Job != token {
		t.Fatalf("record %+v", r)
	}
	// the view: kind probe, the record in place
	b, err := os.ReadFile(filepath.Join(f.job.ViewDir, blockmap.LiveFile))
	if err != nil {
		t.Fatal(err)
	}
	v, err := blockmap.Decode(b)
	if err != nil || v.Kind != "probe" || v.Status != "done" || string(v.Map) != "??===??==========" || v.Skipped != 52<<20 ||
		len(v.Records) != 2 || v.Records[1] != (blockmap.Record{Disk: "drive-scsi0", State: "reuse", Changed: 3, Pieces: 16}) {
		t.Fatalf("view %+v %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(f.job.ViewDir, blockmap.LastFile(100))); err == nil {
		t.Fatal("a probe replaced the guest's last map")
	}
	if _, ran := f.seen("restic-ran"); ran {
		t.Fatal("a probe ran restic")
	}
}

func TestProbeFailures(t *testing.T) {
	for _, c := range []struct {
		host string
		code int
		msg  string
	}{
		{"notready", CodeNotReady, "the Proxmox plug-in is not ready: changed reads are off on this host; run restic-pve install on the Proxmox host and answer yes to changed reads"},
		{"cut", CodeSource, "ssh exited 255"},
	} {
		f := newFixture(t, c.host)
		res, lines := f.probe()
		if res.Code != c.code || res.Status != "error" || !strings.Contains(res.Message, c.msg) {
			t.Errorf("%s: %+v\n%s", c.host, res, strings.Join(lines, "\n"))
		}
		if r := pieces.LoadRecord(pieces.RecordFile(f.state, "web"), 100); r.Job != "" {
			t.Errorf("%s: record state written: %+v", c.host, r)
		}
		if b, err := os.ReadFile(filepath.Join(f.job.ViewDir, blockmap.LiveFile)); err == nil {
			if v, _ := blockmap.Decode(b); v == nil || v.Status != "failed" {
				t.Errorf("%s: view %q", c.host, b)
			}
		}
	}
	// the record state cannot be written: the host hears fail, the result is a local error
	f := newFixture(t, "ok")
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only folder")
	}
	dir := filepath.Join(f.state, "pieces")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "web.lock"), nil, 0o600)
	os.Chmod(dir, 0o500) // the lock can be taken, the record state not written
	defer os.Chmod(dir, 0o700)
	res, _ := f.probe()
	if res.Code != CodeLocalIO {
		t.Fatalf("%+v", res)
	}
	if ack, _ := f.seen("ack"); !strings.HasPrefix(ack, "fail changes: no valid ack: the backup machine failed: the record state could not be written") {
		t.Fatalf("ack %q", ack)
	}
}

func TestLoadJobRefusals(t *testing.T) {
	good := map[string]any{"schema": 1, "mode": "probe", "vmid": 100, "alias": "web", "source": "site1", "ssh_config": "/c",
		"ssh_host": "pve", "restic_host": "backup", "tags": []string{"unit=guest-web"}, "info": map[string]any{},
		"view_dir": "/state/guests/view", "progress_interval_s": 30, "deadline": "2026-11-21T03:00:00Z", "full_check_every": 7}
	write := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		p := filepath.Join(t.TempDir(), "job.json")
		os.WriteFile(p, b, 0o600)
		return p
	}
	if _, err := LoadJob(write(good)); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"schema": 2, "mode": "full", "vmid": 99, "alias": "../x", "ssh_host": "-oProxyCommand=x",
		"ssh_config": "a b", "tags": []string{"a;b"}, "view_dir": "relative", "deadline": "tomorrow", "extra": 1,
		"progress_interval_s": 0, "full_check_every": 0, "info": "x", "restic_host": "a b"} {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		if _, err := LoadJob(write(m)); err == nil {
			t.Errorf("%s=%v accepted", k, v)
		}
	}
}

func t0ctx() context.Context { return context.Background() }
