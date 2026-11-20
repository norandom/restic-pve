package provider

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/testfix"
)

func TestPluginFileIsSmallAndInert(t *testing.T) {
	disk, err := os.ReadFile("ResticPVEPlugin.pm")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, Plugin) {
		t.Fatal("embedded copy differs from the file")
	}
	if !HasMarker(Plugin) || HasMarker([]byte("# something else\n")) {
		t.Fatal("marker line")
	}
	text := string(Plugin)
	for _, want := range []string{
		"our $PROTOCOL = 1;", "our $SOCKET = '" + Socket + "';",
		"package PVE::Storage::Custom::ResticPVEPlugin;", "package PVE::BackupProvider::Plugin::ResticPVE;",
		"use base qw(PVE::Storage::Plugin);", "use base qw(PVE::BackupProvider::Plugin::Base);",
		"sub type { return 'resticpve' }", "'backup-provider' => 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	// the refusal text, as Go and Perl spell it (the Perl string is split over two lines)
	flat := strings.Join(strings.Fields(strings.ReplaceAll(text, "\"\n    . \"", "")), " ")
	if !strings.Contains(flat, Refusal) {
		t.Error("refusal text differs between plugin.go and the plug-in")
	}
	if n := bytes.Count(Plugin, []byte("\n")); n > 220 {
		t.Errorf("%d lines; the plug-in must stay a thin stub", n)
	}
	if regexp.MustCompile(`v?\d+\.\d+\.\d+`).MatchString(text) {
		t.Error("the plug-in names a release version; it must change only when its code changes")
	}
	// only core modules and the Proxmox base classes
	allowed := map[string]bool{"strict": true, "warnings": true, "base": true, "IO::Socket::UNIX": true,
		"JSON::PP": true, "MIME::Base64": true}
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:use|require)\s+([\w:]+)`).FindAllStringSubmatch(text, -1) {
		if !allowed[m[1]] {
			t.Errorf("uses module %s", m[1])
		}
	}
	// nothing that touches the host: no programs, no files, no processes
	for _, bad := range []string{`\bsystem\b`, `\bexec\b`, "`", `\bqx\b`, `\bopen\s*\(`, `\bopen\s+my`, `\bunlink\b`,
		`\bfork\b`, `\bkill\b`, `\bmkdir\b`, `\brmdir\b`, `\brename\b`, `\bchmod\b`, `\bchown\b`, `\beval\s*"`, `\bdo\s+['"$]`,
		`\$ENV\b`, `\bsysopen\b`, `\bsocket\s*\(`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("plug-in contains %s", bad)
		}
	}
}

// hostRoot writes the embedded plug-in where Proxmox keeps it, below a temporary root.
func hostRoot(t *testing.T) string {
	root := t.TempDir()
	p := filepath.Join(root, "PVE", "Storage", "Custom", "ResticPVEPlugin.pm")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, Plugin, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func stubs(t *testing.T) string {
	p, err := filepath.Abs("testdata/lib")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func perl(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(testfix.Perl(t), args...)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("perl %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestPerlCompiles(t *testing.T) {
	root := hostRoot(t)
	out := perl(t, nil, "-I", root, "-I", stubs(t), "-c", filepath.Join(root, "PVE/Storage/Custom/ResticPVEPlugin.pm"))
	if !strings.Contains(out, "syntax OK") {
		t.Fatalf("perl -c: %s", out)
	}
}

func TestLoaderVersionWindow(t *testing.T) {
	root := hostRoot(t)
	for apiver, want := range map[string]string{
		"17": "loaded api 17", // the tested version
		"11": "loaded api 11", // the first with backup providers
		"20": "loaded api 17", // a newer Proxmox: report the tested one, inside its age window
		"10": "load failed: implements an API version newer than current (17 > 10)",
		"30": "load failed: API version too old, please update the plugin (17 < 22)",
	} {
		cmd := exec.Command(testfix.Perl(t), "-I", root, "-I", stubs(t), "testdata/vzdump-driver.pl", "load", root)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "STUB_APIVER=" + apiver}
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), want) {
			t.Errorf("APIVER %s: %q, want %q", apiver, out, want)
		}
	}
}

// call is one request the stub job server received.
type call struct {
	Method string         `json:"method"`
	Args   map[string]any `json:"args"`
}

// stubServer stands in for the job server (task 2.4): one connection, one JSON line per call,
// the answer from replies (default {"ok":true}).
type stubServer struct {
	mu      sync.Mutex
	calls   []call
	replies map[string]string
	closeOn string // close the connection instead of answering this method
}

func (s *stubServer) start(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "provider.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Minute))
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var k call
			if err := json.Unmarshal(line, &k); err != nil {
				c.Write([]byte(`{"ok":false,"message":"bad request"}` + "\n"))
				return
			}
			s.mu.Lock()
			s.calls = append(s.calls, k)
			s.mu.Unlock()
			if k.Method == s.closeOn {
				return
			}
			reply, ok := s.replies[k.Method]
			if !ok {
				reply = `{"ok":true}`
			}
			c.Write([]byte(reply + "\n"))
		}
	}()
	return p
}

func (s *stubServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.Method)
	}
	return out
}

func (s *stubServer) args(method string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.Method == method {
			return c.Args
		}
	}
	return nil
}

func goodReplies() map[string]string {
	return map[string]string{
		"job_init":                    `{"ok":true,"log":"restic-pve: job 0011 for VM 100"}`,
		"backup_init":                 `{"ok":true,"result":{"archive-name":"restic-pve/vm-100/0011"}}`,
		"backup_get_mechanism":        `{"ok":true,"result":"nbd"}`,
		"backup_vm_query_incremental": `{"ok":true,"result":{"drive-efidisk0":"use","drive-scsi0":"use"}}`,
		"backup_vm":                   `{"ok":true,"log":"restic-pve: 3 pieces sent, ack ok"}`,
		"backup_cleanup":              `{"ok":true,"result":{"stats":{"archive-size":12582912}}}`,
	}
}

func runDriver(t *testing.T, sock string) string {
	t.Helper()
	root := hostRoot(t)
	return perl(t, nil, "-I", root, "-I", stubs(t), "testdata/vzdump-driver.pl", "job", root, sock, "100")
}

func TestDriverWholeJob(t *testing.T) {
	s := &stubServer{replies: goodReplies()}
	out := runDriver(t, s.start(t))
	if !strings.HasSuffix(out, "job ok, guest ok\n") {
		t.Fatalf("driver:\n%s", out)
	}
	want := []string{"job_init", "backup_init", "backup_get_mechanism", "backup_vm_query_incremental",
		"backup_vm", "backup_cleanup", "backup_handle_log_file", "job_cleanup"}
	if got := s.methods(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("calls %v, want %v", got, want)
	}
	for _, l := range []string{"result archive-name restic-pve/vm-100/0011", "result mechanism nbd",
		"result incremental drive-scsi0 use", "result archive-size 12582912", "result provider_name restic-pve",
		"log info restic-pve: 3 pieces sent, ack ok", "log info restic-pve: job 0011 for VM 100"} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("driver output lacks %q", l)
		}
	}
	if a := s.args("job_init"); a["protocol"] != float64(Protocol) {
		t.Errorf("job_init args %v", a)
	}
	if a := s.args("backup_init"); a["vmid"] != float64(100) || a["type"] != "qemu" {
		t.Errorf("backup_init args %v", a)
	}
	vm := s.args("backup_vm")
	cfg, _ := base64.StdEncoding.DecodeString(vm["config_b64"].(string))
	fw, _ := base64.StdEncoding.DecodeString(vm["firewall_b64"].(string))
	if !strings.Contains(string(cfg), "scsi0: local:100/vm-100-disk-0.raw,size=20G\n") || string(fw) != "[OPTIONS]\nenable: 1\n" {
		t.Errorf("config %q firewall %q", cfg, fw)
	}
	if vm["bandwidth_limit"] != float64(40960*1024) {
		t.Errorf("bandwidth_limit %v", vm["bandwidth_limit"])
	}
	vol := vm["volumes"].(map[string]any)["drive-scsi0"].(map[string]any)
	if vol["bitmap-mode"] != "reuse" || vol["size"] != float64(20<<30) || vol["bitmap-name"] != "snapshot-access:restic-pve" ||
		!strings.HasPrefix(vol["nbd-path"].(string), "/run/qemu-server/100.backup-access.") {
		t.Errorf("volume %v", vol)
	}
	if a := s.args("backup_cleanup"); a["success"] != true {
		t.Errorf("backup_cleanup args %v", a)
	}
	if a := s.args("backup_handle_log_file"); a["filename"] != "/var/tmp/vzdumptmp4242/qemu-100.log" {
		t.Errorf("log file args %v", a)
	}
}

func TestDriverBackupFails(t *testing.T) {
	r := goodReplies()
	r["backup_vm"] = `{"ok":false,"message":"the backup machine failed: check C2"}`
	s := &stubServer{replies: r}
	out := runDriver(t, s.start(t))
	if !strings.Contains(out, "guest failed: restic-pve: the backup machine failed: check C2\n") || !strings.HasSuffix(out, "job ok, guest failed\n") {
		t.Fatalf("driver:\n%s", out)
	}
	a := s.args("backup_cleanup")
	if a["success"] != false || !strings.Contains(a["error"].(string), "check C2") {
		t.Fatalf("backup_cleanup after a failure: %v", a)
	}
	if got := strings.Join(s.methods(), " "); !strings.HasSuffix(got, "backup_vm backup_cleanup backup_handle_log_file job_cleanup") {
		t.Fatalf("calls %s", got)
	}
}

func TestDriverNoSocketRefuses(t *testing.T) {
	out := runDriver(t, filepath.Join(t.TempDir(), "provider.sock"))
	if !strings.Contains(out, "job failed: "+Refusal+"\n") {
		t.Fatalf("driver:\n%s", out)
	}
	if strings.Contains(out, "call backup_init") {
		t.Fatal("the guest was touched after the refusal")
	}
}

func TestDriverServerRefusesJob(t *testing.T) {
	r := goodReplies()
	r["job_init"] = `{"ok":false,"message":"not our vzdump"}`
	s := &stubServer{replies: r}
	out := runDriver(t, s.start(t))
	if !strings.Contains(out, "job failed: "+Refusal+"\n") || strings.Contains(out, "call backup_init") {
		t.Fatalf("driver:\n%s", out)
	}
}

func TestDriverConnectionLost(t *testing.T) {
	s := &stubServer{replies: goodReplies(), closeOn: "backup_vm"}
	out := runDriver(t, s.start(t))
	if !strings.Contains(out, "guest failed: restic-pve: connection lost in backup_vm") || !strings.HasSuffix(out, "guest failed\n") ||
		!strings.Contains(out, "log warn backup cleanup for external provider failed - restic-pve: no job connection for backup_cleanup") {
		t.Fatalf("driver:\n%s", out)
	}
}

func TestDriverRefusesBadReplies(t *testing.T) {
	for name, c := range map[string][2]string{
		"archive name with a space": {"backup_init", `{"ok":true,"result":{"archive-name":"a b"}}`},
		"mechanism directory":       {"backup_get_mechanism", `{"ok":true,"result":"directory"}`},
		"incremental mode maybe":    {"backup_vm_query_incremental", `{"ok":true,"result":{"drive-scsi0":"maybe"}}`},
		"reply not JSON":            {"backup_vm", `ok`},
	} {
		r := goodReplies()
		r[c[0]] = c[1]
		s := &stubServer{replies: r}
		if out := runDriver(t, s.start(t)); !strings.HasSuffix(out, "guest failed\n") {
			t.Errorf("%s: driver:\n%s", name, out)
		}
	}
}

// The Go and Perl sides agree on the API window.
func TestAPIWindowMatchesThePlugin(t *testing.T) {
	text := string(Plugin)
	if !strings.Contains(text, "my $APIVER_MIN = "+strconv.Itoa(APIMin)+";") || !strings.Contains(text, "my $APIVER_TESTED = "+strconv.Itoa(APITested)+";") {
		t.Fatal("APIMin/APITested differ from the plug-in")
	}
	if !strings.Contains(text, "sub update_volume_attribute { return }") {
		t.Fatal("update_volume_attribute must do nothing")
	}
}

// The two Perl checks install runs, against the real file and Proxmox's loader (stub): the
// API and the base class, then "loaded"; and what they say when Proxmox refuses the file.
func TestProxmoxChecksInPerl(t *testing.T) {
	root := hostRoot(t)
	run := func(apiver, script string) string {
		cmd := exec.Command(testfix.Perl(t), "-I", root, "-I", stubs(t), "-e", script)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "STUB_APIVER=" + apiver}
		out, _ := cmd.Output()
		return string(out)
	}
	if out := run("17", apiScript); out != "api 17 8 base 1\n" {
		t.Fatalf("api: %q", out)
	}
	if out := run("17", loadScript); out != "loaded\n" {
		t.Fatalf("load: %q", out)
	}
	if out := run("30", loadScript); !strings.Contains(out, "warn: Error loading storage plugin \"PVE::Storage::Custom::ResticPVEPlugin\": API version too old") ||
		!strings.HasSuffix(out, "not loaded\n") {
		t.Fatalf("refused: %q", out)
	}
	if out := run("19", loadScript); !strings.Contains(out, "is implementing an older storage API") || !strings.HasSuffix(out, "loaded\n") {
		t.Fatalf("newer: %q", out)
	}
	// vzdump sets notes and protected when site defaults say so: harmless
	if out := run("17", `require PVE::Storage; my $c = "PVE::Storage::Custom::ResticPVEPlugin"; `+
		`$c->update_volume_attribute({}, "restic-pve", "backup/x", "protected", 1); `+
		`$c->update_volume_attribute({}, "restic-pve", "backup/x", "notes", "n"); print "attr ok\n";`); out != "attr ok\n" {
		t.Fatalf("attributes: %q", out)
	}
}
