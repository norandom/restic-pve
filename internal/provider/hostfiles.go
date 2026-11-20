package provider

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The storage entry and the folders of a provider job.
const (
	// StorageID is the name of the storage entry in Proxmox's storage configuration.
	StorageID = "restic-pve"
	// StorageType is the plug-in's storage type (sub type in the plug-in).
	StorageType = "resticpve"
	// StorageConfig is Proxmox's storage configuration. It is only read here; the entry is
	// added and removed through pvesm.
	StorageConfig = "/etc/pve/storage.cfg"
	// RunDir holds the job server's socket while a provider job runs (root only).
	RunDir = "/run/restic-pve"
	// ReloadPending marks a daemon reload that is still owed (tmpfs: a reboot clears it).
	ReloadPending = "/run/restic-pve.reload-pending"
)

// Daemons are the Proxmox services reloaded when the plug-in file appears or disappears, so
// that the screens and the status daemon know the storage type.
var Daemons = []string{"pvestatd.service", "pvedaemon.service", "pveproxy.service"}

// State is the state of the plug-in file on the host.
type State int

const (
	Absent    State = iota // no file
	Current                // the embedded copy, byte for byte
	Different              // ours (marker), but another version
	Foreign                // a file without the marker, or not a regular file: never touched
)

func (s State) String() string {
	return [...]string{"absent", "current", "different", "foreign"}[s]
}

// maxPluginRead bounds what is read of a file at PluginPath.
const maxPluginRead = 1 << 20

// Host is the Proxmox host as far as the plug-in is concerned. Tests give a temporary Root
// and a fake Run.
type Host struct {
	Root   string                                            // prefix for every path ("" in production)
	Run    func(name string, args ...string) ([]byte, error) // runs pvesm and systemctl
	Chown  func(path string, uid, gid int) error             // nil: os.Chown when root
	Report func(word, what string)                           // one line per step; nil: silent
}

func (h *Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h *Host) report(word, what string) {
	if h.Report != nil {
		h.Report(word, what)
	}
}

func (h *Host) run(name string, args ...string) error {
	if _, err := h.Run(name, args...); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// PluginState reads the plug-in file and compares it with the embedded copy.
func (h *Host) PluginState() (State, error) {
	p := h.path(PluginPath)
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return Absent, nil
	}
	if err != nil {
		return Foreign, err
	}
	if !st.Mode().IsRegular() {
		return Foreign, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return Foreign, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPluginRead+1))
	if err != nil {
		return Foreign, err
	}
	switch {
	case !HasMarker(b):
		return Foreign, nil
	case bytes.Equal(b, Plugin):
		return Current, nil
	}
	return Different, nil
}

// foreignErr is the refusal for a file at PluginPath that restic-pve did not write.
func foreignErr() error {
	return fmt.Errorf("%s was not written by restic-pve (no marker line); it was left as it is: "+
		"move it away and run restic-pve install again", PluginPath)
}

// mkdirs creates dir and its missing parents with mode 0755 whatever the umask.
func mkdirs(dir string) error {
	var made []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		made = append(made, d)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, d := range made {
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// writePlugin writes the embedded copy (see writeContent) and reports it.
func (h *Host) writePlugin() error {
	if err := h.writeContent(Plugin); err != nil {
		return err
	}
	h.report("written", PluginPath)
	return nil
}

// writeContent writes content to PluginPath with a temp file in the same folder, fsync, mode
// 0644 owner root, and one rename. The temp name starts with a dot and does not end in ".pm",
// so Proxmox's loader (it loads names matching ^.*\.pm$) never sees it.
func (h *Host) writeContent(content []byte) error {
	dir := filepath.Dir(h.path(PluginPath))
	if err := mkdirs(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(PluginPath)+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	chown := h.Chown
	if chown == nil {
		chown = func(p string, uid, gid int) error {
			if os.Geteuid() != 0 {
				return nil // a test as a plain user: the file is that user's
			}
			return os.Chown(p, uid, gid)
		}
	}
	err = func() error {
		_, err := f.Write(content)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = chown(tmp, 0, 0)
		}
		if err == nil {
			err = os.Chmod(tmp, 0o644)
		}
		if err == nil {
			err = os.Rename(tmp, h.path(PluginPath))
		}
		return err
	}()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", PluginPath, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// SyncPlugin brings a plug-in file of another version to the embedded copy. It never creates
// the file (changed reads are off when it is absent) and never touches a foreign one. The
// caller holds the lock, so no backup runs while the file changes. It returns the state found.
func (h *Host) SyncPlugin() (State, error) {
	st, err := h.PluginState()
	switch {
	case err != nil:
		return st, fmt.Errorf("read %s: %w", PluginPath, err)
	case st == Different:
		return st, h.writePlugin()
	case st == Current:
		h.report("unchanged", PluginPath)
	case st == Foreign:
		return st, foreignErr()
	}
	return st, nil
}

var sectionRe = regexp.MustCompile(`^([a-z][a-z0-9]*):\s+(\S+)\s*$`)

// StorageEntry returns the type of the storage entry named StorageID in the storage
// configuration, or "" when there is none (or no configuration).
func (h *Host) StorageEntry() (string, error) {
	f, err := os.Open(h.path(StorageConfig))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 16<<20))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if m := sectionRe.FindStringSubmatch(sc.Text()); m != nil && m[2] == StorageID {
			return m[1], nil
		}
	}
	return "", sc.Err()
}

// reload reloads the Proxmox daemons, as pve-manager's own packaging does
// ("deb-systemd-invoke reload-or-try-restart pvedaemon.service", the same for pvestatd and
// pveproxy, in debian/postinst), so open sessions and running tasks go on. A marker in /run
// says a reload is still owed until it succeeded, so a run after a failed reload does it
// again; a reboot clears the marker and restarts the daemons anyway.
func (h *Host) reload() error {
	marker := h.path(ReloadPending)
	_ = mkdirs(filepath.Dir(marker))
	_ = os.WriteFile(marker, nil, 0o600)
	if err := h.run("systemctl", append([]string{"reload-or-try-restart"}, Daemons...)...); err != nil {
		return fmt.Errorf("%v; the Proxmox daemons were not reloaded: run the same restic-pve command again (it reloads them), or systemctl reload-or-try-restart %s", err, strings.Join(Daemons, " "))
	}
	_ = os.Remove(marker)
	h.report("reloaded", strings.Join(Daemons, " "))
	return nil
}

// reloadPending reports whether an earlier run could not reload the daemons.
func (h *Host) reloadPending() bool {
	_, err := os.Lstat(h.path(ReloadPending))
	return err == nil
}

// nodeRe is a Proxmox node name (a short host name).
var nodeRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// CanEnable checks, without changing anything, that changed reads can be turned on here: not
// a cluster, a Proxmox with the backup provider interface and a storage API version the
// plug-in fits, no foreign file at PluginPath, no foreign storage named StorageID. It returns
// what Perl reported of Proxmox's storage API.
func (h *Host) CanEnable() (API, error) {
	if h.Clustered() {
		return API{}, ErrCluster
	}
	api, err := h.ProxmoxAPI()
	if err != nil {
		return api, err
	}
	if err := api.Problem(); err != nil {
		return api, err
	}
	st, err := h.PluginState()
	if err != nil {
		return api, fmt.Errorf("read %s: %w", PluginPath, err)
	}
	if st == Foreign {
		return api, foreignErr()
	}
	typ, err := h.StorageEntry()
	if err != nil {
		return api, fmt.Errorf("read %s: %w", StorageConfig, err)
	}
	if typ != "" && typ != StorageType {
		return api, fmt.Errorf("a storage named %s of type %s exists; it is not restic-pve's: rename it in Proxmox and run restic-pve install again", StorageID, typ)
	}
	return api, nil
}

// Enable turns changed reads on: after CanEnable, it writes the plug-in file when it is
// absent or of another version and checks that Proxmox loads it; it reloads the Proxmox
// daemons when the file appeared (or a reload is still owed); then it adds the storage entry
// for node when it is missing. When the load check or pvesm add fails, the plug-in file is put
// back as it was (removed, or the earlier version) and the error says what failed.
func (h *Host) Enable(node string) error {
	if !nodeRe.MatchString(node) {
		return fmt.Errorf("node name %q is not a Proxmox node name", node)
	}
	api, err := h.CanEnable()
	if err != nil {
		return err
	}
	if api.Newer() {
		h.report("note", api.NewerNote())
	}
	st, _ := h.PluginState()
	typ, _ := h.StorageEntry()
	var old []byte
	if st == Different {
		if old, err = os.ReadFile(h.path(PluginPath)); err != nil {
			return fmt.Errorf("read %s: %w", PluginPath, err)
		}
	}
	pending := h.reloadPending()
	if st == Current {
		h.report("unchanged", PluginPath)
	} else {
		if err := h.writePlugin(); err != nil {
			return err
		}
		if err := h.Loaded(); err != nil {
			return h.restore(st, old, false, err)
		}
	}
	if st == Absent || pending {
		if err := h.reload(); err != nil {
			return err
		}
	}
	if typ == StorageType {
		h.report("unchanged", "storage "+StorageID)
		return nil
	}
	if err := h.run("pvesm", "add", StorageType, StorageID, "--content", "backup", "--nodes", node); err != nil {
		return h.restore(st, old, st == Absent, err)
	}
	h.report("added", "storage "+StorageID+" (pvesm add)")
	return nil
}

// restore puts the plug-in file back as it was before Enable (absent, or the earlier
// version) after cause, reloads the daemons when they had seen the new file, and returns
// cause with what was done.
func (h *Host) restore(st State, old []byte, reload bool, cause error) error {
	var err error
	switch st {
	case Absent:
		err = os.Remove(h.path(PluginPath))
		_ = os.Remove(filepath.Dir(h.path(PluginPath)))
	case Different:
		err = h.writeContent(old)
	}
	if err != nil {
		return fmt.Errorf("%v; putting back the earlier plug-in file failed too: %v", cause, err)
	}
	h.report("restored", PluginPath+" ("+st.String()+" as before)")
	if reload {
		if rerr := h.reload(); rerr != nil {
			return fmt.Errorf("%v; %v", cause, rerr)
		}
	}
	return fmt.Errorf("%v; the plug-in file is as it was before, nothing else changed", cause)
}

// Disable removes, in this order: the storage entry (pvesm remove), the plug-in file and its
// folder when empty, the daemon reload when the file was removed (or a reload is still owed),
// and the job folder RunDir. Each step is reported. Neither a foreign file nor a storage of
// another type is touched. When pvesm remove fails, the plug-in file is kept, so the entry
// keeps a plug-in that Proxmox can load.
func (h *Host) Disable() error {
	pending := h.reloadPending()
	typ, err := h.StorageEntry()
	if err != nil {
		return fmt.Errorf("read %s: %w", StorageConfig, err)
	}
	switch typ {
	case "":
		h.report("absent", "storage "+StorageID)
	case StorageType:
		if err := h.run("pvesm", "remove", StorageID); err != nil {
			return &LeftError{Err: err, Left: "the storage " + StorageID + " and " + PluginPath,
				Finish: "pvesm remove " + StorageID + " (see the error above)"}
		}
		h.report("removed", "storage "+StorageID+" (pvesm remove)")
	default:
		h.report("kept", fmt.Sprintf("storage %s (type %s, not restic-pve's)", StorageID, typ))
	}
	st, err := h.PluginState()
	if err != nil {
		return fmt.Errorf("read %s: %w", PluginPath, err)
	}
	switch st {
	case Absent:
		h.report("absent", PluginPath)
	case Foreign:
		h.report("kept", PluginPath+" (not written by restic-pve)")
	default:
		if err := os.Remove(h.path(PluginPath)); err != nil {
			return fmt.Errorf("remove %s: %w", PluginPath, err)
		}
		h.report("removed", PluginPath)
		// the plug-in folder, when install made it and nothing else is in it
		if os.Remove(filepath.Dir(h.path(PluginPath))) == nil {
			h.report("removed", filepath.Dir(PluginPath)+"/ (empty)")
		}
		pending = true
	}
	if pending {
		if err := h.reload(); err != nil {
			return &LeftError{Err: err, Left: "a reload of " + strings.Join(Daemons, " "),
				Finish: "systemctl reload-or-try-restart " + strings.Join(Daemons, " ")}
		}
	}
	return h.RemoveRunDir()
}

// LeftError is a removal that could not be finished: what is left and how to finish it.
type LeftError struct {
	Err          error
	Left, Finish string
}

func (e *LeftError) Error() string { return e.Err.Error() }
func (e *LeftError) Unwrap() error { return e.Err }

// RemoveRunDir removes the job folder and the socket in it. The caller holds the lock.
func (h *Host) RemoveRunDir() error {
	p := h.path(RunDir)
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		h.report("absent", RunDir+"/")
		return nil
	}
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("remove %s: %w", RunDir, err)
	}
	h.report("removed", RunDir+"/")
	return nil
}
