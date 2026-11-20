package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeHost is a host root with a fake pvesm that keeps a storage.cfg, and a fake systemctl.
// Every call and every report is recorded in one log, in order.
type fakeHost struct {
	t       *testing.T
	h       *Host
	log     []string
	failRun string // a command line prefix that fails
	apiOut  string // what the API one-liner prints
	loadOut string // what the load check prints
}

func newFakeHost(t *testing.T) *fakeHost {
	f := &fakeHost{t: t, apiOut: "api 17 8 base 1\n", loadOut: "loaded\n"}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/pve"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.h = &Host{Root: root, Run: f.run, Chown: func(string, int, int) error { return nil },
		Report: func(w, what string) { f.log = append(f.log, w+" "+what) }}
	f.writeCfg("dir: local\n\tpath /var/lib/vz\n\tcontent iso,backup\n")
	return f
}

func (f *fakeHost) cfgPath() string { return filepath.Join(f.h.Root, StorageConfig) }

func (f *fakeHost) writeCfg(s string) {
	if err := os.WriteFile(f.cfgPath(), []byte(s), 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeHost) cfg() string {
	b, _ := os.ReadFile(f.cfgPath())
	return string(b)
}

func (f *fakeHost) run(name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if name == "timeout" && len(args) == 4 && args[1] == "perl" {
		switch args[3] {
		case apiScript:
			cmd = "perl api"
		case loadScript:
			cmd = "perl load"
		}
	}
	f.log = append(f.log, "run "+cmd)
	if f.failRun != "" && strings.HasPrefix(cmd, f.failRun) {
		return nil, errors.New("exit status 1")
	}
	switch {
	case cmd == "perl api":
		return []byte(f.apiOut), nil
	case cmd == "perl load":
		return []byte(f.loadOut), nil
	case name == "pvesm" && args[0] == "add":
		f.writeCfg(f.cfg() + "\n" + args[1] + ": " + args[2] + "\n\tcontent backup\n\tnodes " + args[6] + "\n")
	case name == "pvesm" && args[0] == "remove":
		var out []string
		skip := false
		for _, l := range strings.Split(f.cfg(), "\n") {
			if m := sectionRe.FindStringSubmatch(l); m != nil {
				skip = m[2] == args[1]
			}
			if !skip {
				out = append(out, l)
			}
		}
		f.writeCfg(strings.Join(out, "\n"))
	}
	return nil, nil
}

func (f *fakeHost) plugin() string { return filepath.Join(f.h.Root, PluginPath) }

func (f *fakeHost) put(content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.plugin()), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.plugin(), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeHost) content() (string, bool) {
	b, err := os.ReadFile(f.plugin())
	return string(b), err == nil
}

func (f *fakeHost) logged() string {
	s := strings.Join(f.log, "\n")
	f.log = nil
	return s
}

var oldPlugin = Marker + "\n# an older plug-in\n1;\n"
var foreignPlugin = "package PVE::Storage::Custom::ResticPVEPlugin;\n# someone else's\n1;\n"

func TestPluginStates(t *testing.T) {
	f := newFakeHost(t)
	for _, c := range []struct {
		put  *string
		want State
	}{
		{nil, Absent},
		{&oldPlugin, Different},
		{new(string), Foreign}, // an empty file has no marker
	} {
		if c.put != nil {
			f.put(*c.put)
		}
		if st, err := f.h.PluginState(); err != nil || st != c.want {
			t.Errorf("%v: %v %v", c.want, st, err)
		}
	}
	f.put(string(Plugin))
	if st, _ := f.h.PluginState(); st != Current {
		t.Errorf("embedded copy: %v", st)
	}
	f.put(foreignPlugin)
	if st, _ := f.h.PluginState(); st != Foreign {
		t.Errorf("no marker: %v", st)
	}
	_ = os.Remove(f.plugin())
	if err := os.Symlink("/etc/hostname", f.plugin()); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.h.PluginState(); st != Foreign {
		t.Errorf("symlink: %v", st)
	}
	_ = os.Remove(f.plugin())
	_ = os.Mkdir(f.plugin(), 0o755)
	if st, _ := f.h.PluginState(); st != Foreign {
		t.Errorf("directory: %v", st)
	}
}

func TestSyncPluginActions(t *testing.T) {
	f := newFakeHost(t)
	// absent: changed reads are off, nothing is created
	if st, err := f.h.SyncPlugin(); err != nil || st != Absent || f.logged() != "" {
		t.Fatalf("absent: %v %v", st, err)
	}
	if _, ok := f.content(); ok {
		t.Fatal("sync created the plug-in")
	}
	// different: rewritten in one rename, mode 0644, no temp file left
	f.put(oldPlugin)
	if st, err := f.h.SyncPlugin(); err != nil || st != Different || f.logged() != "written "+PluginPath {
		t.Fatalf("different: %v %v", st, err)
	}
	if c, _ := f.content(); c != string(Plugin) {
		t.Fatal("not the embedded copy")
	}
	if st, _ := os.Stat(f.plugin()); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Dir(f.plugin())); len(ents) != 1 {
		t.Fatalf("left in the folder: %v", ents)
	}
	// current: untouched (same inode)
	before, _ := os.Stat(f.plugin())
	if st, err := f.h.SyncPlugin(); err != nil || st != Current || f.logged() != "unchanged "+PluginPath {
		t.Fatalf("current: %v %v", st, err)
	}
	if after, _ := os.Stat(f.plugin()); !os.SameFile(before, after) {
		t.Fatal("a current plug-in was rewritten")
	}
	// foreign: refused, untouched
	f.put(foreignPlugin)
	if st, err := f.h.SyncPlugin(); err == nil || st != Foreign || !strings.Contains(err.Error(), "was not written by restic-pve") {
		t.Fatalf("foreign: %v %v", st, err)
	}
	if c, _ := f.content(); c != foreignPlugin {
		t.Fatal("foreign file changed")
	}
}

// The temp file starts with a dot and does not end in .pm: Proxmox's loader matches ^.*\.pm$.
func TestTempNameNeverLoads(t *testing.T) {
	f := newFakeHost(t)
	f.put(oldPlugin)
	dir := filepath.Dir(f.plugin())
	// make the rename fail: a directory with the temp name's target cannot be replaced by a file
	_ = os.Remove(f.plugin())
	_ = os.Mkdir(f.plugin(), 0o755)
	_ = os.WriteFile(filepath.Join(f.plugin(), "x"), nil, 0o644)
	if err := f.h.writePlugin(); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp file left after a failed write: %v", ents)
	}
	// the name pattern, from a successful write's temp file
	name := "." + filepath.Base(PluginPath) + ".new-123456"
	if !strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".pm") {
		t.Fatal(name)
	}
}

func TestEnableAndDisableOrder(t *testing.T) {
	f := newFakeHost(t)
	if err := f.h.Enable("pve1"); err != nil {
		t.Fatal(err)
	}
	want := "run perl api\nwritten " + PluginPath + "\nrun perl load\n" +
		"run systemctl reload-or-try-restart pvestatd.service pvedaemon.service pveproxy.service\n" +
		"reloaded pvestatd.service pvedaemon.service pveproxy.service\n" +
		"run pvesm add resticpve restic-pve --content backup --nodes pve1\n" +
		"added storage restic-pve (pvesm add)"
	if got := f.logged(); got != want {
		t.Fatalf("enable:\n%s\nwant\n%s", got, want)
	}
	if typ, _ := f.h.StorageEntry(); typ != StorageType || !strings.Contains(f.cfg(), "dir: local\n") {
		t.Fatalf("storage.cfg:\n%s", f.cfg())
	}
	// again: nothing changes, no restart
	if err := f.h.Enable("pve1"); err != nil || f.logged() != "run perl api\nunchanged "+PluginPath+"\nunchanged storage restic-pve" {
		t.Fatal(err)
	}
	// an older plug-in is rewritten, without a restart (the file did not appear)
	f.put(oldPlugin)
	if err := f.h.Enable("pve1"); err != nil || f.logged() != "run perl api\nwritten "+PluginPath+"\nrun perl load\nunchanged storage restic-pve" {
		t.Fatal(err)
	}
	// a job folder from an interrupted job
	_ = os.MkdirAll(filepath.Join(f.h.Root, RunDir), 0o700)
	_ = os.WriteFile(filepath.Join(f.h.Root, RunDir, "provider.sock"), nil, 0o600)
	if err := f.h.Disable(); err != nil {
		t.Fatal(err)
	}
	want = "run pvesm remove restic-pve\nremoved storage restic-pve (pvesm remove)\n" +
		"removed " + PluginPath + "\n" + "removed /usr/share/perl5/PVE/Storage/Custom/ (empty)\n" +
		"run systemctl reload-or-try-restart pvestatd.service pvedaemon.service pveproxy.service\n" +
		"reloaded pvestatd.service pvedaemon.service pveproxy.service\n" +
		"removed " + RunDir + "/"
	if got := f.logged(); got != want {
		t.Fatalf("disable (entry before file):\n%s\nwant\n%s", got, want)
	}
	if _, ok := f.content(); ok {
		t.Fatal("plug-in left")
	}
	if typ, _ := f.h.StorageEntry(); typ != "" || f.cfg() != "dir: local\n\tpath /var/lib/vz\n\tcontent iso,backup\n" {
		t.Fatalf("storage.cfg:\n%q", f.cfg())
	}
	if _, err := os.Lstat(filepath.Join(f.h.Root, RunDir)); err == nil {
		t.Fatal("job folder left")
	}
	// again: everything absent, no command
	if err := f.h.Disable(); err != nil || f.logged() != "absent storage restic-pve\nabsent "+PluginPath+"\nabsent "+RunDir+"/" {
		t.Fatal(err)
	}
}

func TestEnableRefusals(t *testing.T) {
	f := newFakeHost(t)
	for _, node := range []string{"", "-x", "a b", "pve1.example", "x;id"} {
		if err := f.h.Enable(node); err == nil {
			t.Errorf("node %q accepted", node)
		}
	}
	f.put(foreignPlugin)
	if err := f.h.Enable("pve1"); err == nil || !strings.Contains(err.Error(), "not written by restic-pve") || f.logged() != "run perl api" {
		t.Fatalf("foreign file: %v", err)
	}
	if c, _ := f.content(); c != foreignPlugin {
		t.Fatal("foreign file changed")
	}
	_ = os.Remove(f.plugin())
	f.writeCfg("dir: restic-pve\n\tpath /srv\n")
	if err := f.h.Enable("pve1"); err == nil || !strings.Contains(err.Error(), "of type dir exists") || f.logged() != "run perl api" {
		t.Fatalf("storage of another type: %v", err)
	}
	if _, ok := f.content(); ok {
		t.Fatal("plug-in written although the storage name is taken")
	}
	// Disable leaves both alone
	f.put(foreignPlugin)
	if err := f.h.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := f.logged(); got != "kept storage restic-pve (type dir, not restic-pve's)\nkept "+PluginPath+" (not written by restic-pve)\nabsent "+RunDir+"/" {
		t.Fatal(got)
	}
	if c, _ := f.content(); c != foreignPlugin || !strings.Contains(f.cfg(), "dir: restic-pve") {
		t.Fatal("foreign items touched")
	}
}

func TestEnableStopsOnAFailedStep(t *testing.T) {
	f := newFakeHost(t)
	f.failRun = "pvesm add"
	if err := f.h.Enable("pve1"); err == nil || !strings.Contains(err.Error(), "pvesm add resticpve restic-pve") {
		t.Fatal(err)
	}
	f.failRun = "pvesm remove"
	f.writeCfg("resticpve: restic-pve\n\tcontent backup\n")
	f.put(string(Plugin))
	if err := f.h.Disable(); err == nil {
		t.Fatal("a failed pvesm remove was ignored")
	}
	// the file stays when the entry could not be removed: never a storage entry without its plug-in
	if _, ok := f.content(); !ok {
		t.Fatal("plug-in removed before its storage entry")
	}
}

// Item: clusters. On a cluster node "yes" is refused before anything is asked or written.
func TestEnableRefusedOnACluster(t *testing.T) {
	f := newFakeHost(t)
	os.WriteFile(filepath.Join(f.h.Root, CorosyncConfig), []byte("totem {}\n"), 0o640)
	if err := f.h.Enable("pve1"); !errors.Is(err, ErrCluster) || f.logged() != "" {
		t.Fatalf("%v %q", err, f.log)
	}
	if !strings.Contains(ErrCluster.Error(), "changed reads work on single hosts for now") {
		t.Fatal(ErrCluster)
	}
	if _, ok := f.content(); ok {
		t.Fatal("written on a cluster")
	}
}

// Item: a Proxmox without the provider interface, or one past the plug-in's window: refused,
// nothing written. A newer one inside the window: a note.
func TestEnableChecksTheProxmoxAPI(t *testing.T) {
	for out, want := range map[string]string{
		"api 10 2 base 0\n":   "Proxmox VE 8.4 or later (storage API 11)",
		"api 17 8 base 0\n":   "no backup provider interface",
		"api 26 8 base 1\n":   "(storage API 26) no longer accepts the plug-in of this restic-pve (API 17); run restic-pve update",
		"no PVE::Storage\n":   "cannot ask Perl about this Proxmox's storage API",
		"warn: x\nnonsense\n": "cannot ask Perl",
	} {
		f := newFakeHost(t)
		f.apiOut = out
		if err := f.h.Enable("pve1"); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("%q: %v", out, err)
		}
		if _, ok := f.content(); ok || strings.Contains(f.cfg(), "resticpve") {
			t.Errorf("%q: something was written", out)
		}
	}
	f := newFakeHost(t)
	f.apiOut = "api 19 8 base 1\n"
	if err := f.h.Enable("pve1"); err != nil || !strings.Contains(f.logged(), "note this Proxmox (storage API 19) is newer than the plug-in was checked against (17)") {
		t.Fatal(err)
	}
	if !(API{Ver: 18, Age: 8, Base: true}).Newer() || (API{Ver: 17, Age: 8, Base: true}).Newer() {
		t.Fatal("Newer")
	}
}

// Item: the load check and pvesm add. A failure puts the file back as it was.
func TestEnableRestoresOnAFailedLoadOrAdd(t *testing.T) {
	// absent before, Proxmox does not load it: removed again, no reload, no entry
	f := newFakeHost(t)
	f.loadOut = "warn: Error loading storage plugin \"PVE::Storage::Custom::ResticPVEPlugin\": boom\nnot loaded\n"
	err := f.h.Enable("pve1")
	if err == nil || !strings.Contains(err.Error(), "Proxmox did not load the plug-in (warn: Error loading storage plugin") ||
		!strings.Contains(err.Error(), "the plug-in file is as it was before") {
		t.Fatal(err)
	}
	log := f.logged()
	if _, ok := f.content(); ok || strings.Contains(log, "reload") || strings.Contains(log, "pvesm add") {
		t.Fatalf("not restored:\n%s", log)
	}
	if _, err := os.Stat(filepath.Dir(f.plugin())); err == nil {
		t.Fatal("the folder made for it is left")
	}
	// an older version before: put back byte for byte
	f = newFakeHost(t)
	f.put(oldPlugin)
	f.loadOut = "not loaded\n"
	if err := f.h.Enable("pve1"); err == nil {
		t.Fatal("accepted")
	}
	if c, _ := f.content(); c != oldPlugin {
		t.Fatal("the earlier version was not put back")
	}
	// pvesm add fails: the file goes again and the daemons are reloaded once more
	f = newFakeHost(t)
	f.failRun = "pvesm add"
	err = f.h.Enable("pve1")
	if err == nil || !strings.Contains(err.Error(), "pvesm add resticpve restic-pve") || !strings.Contains(err.Error(), "as it was before") {
		t.Fatal(err)
	}
	if _, ok := f.content(); ok || strings.Count(f.logged(), "run systemctl reload-or-try-restart") != 2 {
		t.Fatal("not restored, or the daemons not reloaded after the removal")
	}
}

// Item: a reload that failed is done by the next run, even with nothing else to change.
func TestFailedReloadIsRetried(t *testing.T) {
	f := newFakeHost(t)
	if err := f.h.Enable("pve1"); err != nil {
		t.Fatal(err)
	}
	f.failRun = "systemctl"
	var le *LeftError
	if err := f.h.Disable(); !errors.As(err, &le) || !strings.Contains(le.Finish, "systemctl reload-or-try-restart") {
		t.Fatalf("%v", err)
	}
	if _, ok := f.content(); ok {
		t.Fatal("file kept")
	}
	f.failRun = ""
	f.logged()
	if err := f.h.Disable(); err != nil || !strings.Contains(f.logged(), "reloaded pvestatd.service") {
		t.Fatalf("the owed reload was not done: %v", err)
	}
	if f.h.reloadPending() {
		t.Fatal("marker left")
	}
}

// Item: pvesm remove fails: the plug-in file stays with its entry, and the error says what
// is left and how to finish.
func TestDisableKeepsThePluginWhenTheEntryStays(t *testing.T) {
	f := newFakeHost(t)
	f.h.Enable("pve1")
	f.failRun = "pvesm remove"
	var le *LeftError
	if err := f.h.Disable(); !errors.As(err, &le) || !strings.Contains(le.Left, "the storage restic-pve and "+PluginPath) {
		t.Fatalf("%v", err)
	}
	if _, ok := f.content(); !ok {
		t.Fatal("plug-in removed although its entry stays")
	}
}

// Item: modes whatever the umask.
func TestModesUnderUmask077(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	f := newFakeHost(t)
	if err := f.h.Enable("pve1"); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{
		filepath.Join(f.h.Root, "usr/share/perl5/PVE/Storage/Custom"): 0o755, filepath.Join(f.h.Root, "usr/share/perl5/PVE"): 0o755,
		f.plugin(): 0o644,
	} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v", p, st.Mode(), err)
		}
	}
}
