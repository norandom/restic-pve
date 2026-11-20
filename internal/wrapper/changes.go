package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/norandom/restic-pve/internal/nbd"
	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/provider"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Bounds of how long a job holds the host: a backup machine that reads nothing of the stream
// for StallLimit, or sends no ack within AckLimit after the end frame, ends the job (Proxmox
// hears failure). Constants, not settings.
const (
	StallLimit = 10 * time.Minute
	AckLimit   = time.Hour
)

// Exit codes of the changes request beyond those of dump.
const (
	exStream   = 3  // the change record or the NBD export broke a rule (misaligned, protocol)
	exNotReady = 79 // the plug-in or the storage entry is not ready: the backup machine reads in full
)

// changesModes are the three modes of `changes <vmid> <mode>`.
var changesModes = map[string]bool{changes.ModeProbe: true, changes.ModeUse: true, changes.ModeNew: true}

// bitmapName is the bitmap qemu-server uses for a provider job to our storage: the target id
// "snapshot-access:<storage>" (qemu-server src/PVE/VZDump/QemuServer.pm).
const bitmapName = "snapshot-access:" + provider.StorageID

// deviceRe is a disk name vzdump hands to a provider: "drive-" and the config key.
var deviceRe = regexp.MustCompile(`^drive-(scsi|virtio|sata|ide|efidisk|tpmstate)[0-9]{1,2}$`)

// infof writes one info line in vzdump's log format, so the backup machine's classifier
// keeps it as info.
func (e *Env) infof(format string, a ...any) {
	fmt.Fprintf(e.Stderr, "INFO: restic-pve: "+format+"\n", a...)
}

// host is the plug-in's side of this host.
func (e *Env) host() *provider.Host {
	return &provider.Host{Root: e.Root,
		Run: func(name string, args ...string) ([]byte, error) {
			return e.command(context.Background(), name, args...).Output()
		},
		Report: func(word, what string) { e.infof("%s %s", word, what) }}
}

// lock takes the host-wide lock without waiting (75 when held) and checks the fleecing
// storage's free space (73). The caller closes the returned file to release the lock.
func (e *Env) lock(c Conf) (*os.File, error) {
	lock, err := os.OpenFile(e.LockFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fail(exBusy, "busy: another dump is running on this host")
	}
	avail, ok := e.availKiB(c.Storage)
	if !ok {
		lock.Close()
		return nil, fail(exNoSpace, "fleecing storage %s: free space unknown (storage inactive?); dump not started", c.Storage)
	}
	if avail < c.minGiB()*1048576 {
		lock.Close()
		return nil, fail(exNoSpace, "fleecing storage %s has %d GiB free, below FLEECING_MIN_FREE_GIB=%s; dump not started",
			c.Storage, avail/1048576, c.MinFreeGiB)
	}
	return lock, nil
}

func notReady(reason, fix string) error {
	return fail(exNotReady, "plug-in not ready: %s; %s", reason, fix)
}

const fixInstall = "run restic-pve install on the Proxmox host"

// ready checks, under the lock, that the plug-in file is restic-pve's (and brings another
// version of it to this binary's copy), that the storage entry exists and that Proxmox
// loaded the plug-in (pvesm status shows the storage active).
func (e *Env) ready() error {
	h := e.host()
	if h.Clustered() {
		return notReady("this host is part of a Proxmox cluster; changed reads work on single hosts for now",
			"run restic-pve install on the Proxmox host and answer no to changed reads")
	}
	st, err := h.PluginState()
	switch {
	case err != nil:
		return notReady(fmt.Sprintf("%s cannot be read (%v)", provider.PluginPath, err), fixInstall)
	case st == provider.Absent:
		return notReady("changed reads are off on this host", fixInstall+" and answer yes to changed reads")
	case st == provider.Foreign:
		return notReady(provider.PluginPath+" was not written by restic-pve", "move it away and "+fixInstall)
	case st == provider.Different:
		if _, err := h.SyncPlugin(); err != nil {
			return notReady(err.Error(), fixInstall)
		}
	}
	typ, err := h.StorageEntry()
	if err != nil || typ != provider.StorageType {
		return notReady("the storage entry "+provider.StorageID+" is missing", fixInstall+" and answer yes to changed reads")
	}
	out, err := e.output("pvesm", "status", "--storage", provider.StorageID)
	active := false
	for _, line := range strings.Split(out, "\n") {
		f := fields(line)
		active = active || field(f, 0) == provider.StorageID && field(f, 2) == "active"
	}
	if err != nil || !active {
		return notReady("Proxmox did not load the plug-in (pvesm status does not show "+provider.StorageID+" active)",
			fixInstall+"; if that does not help, the installed Proxmox may not accept the plug-in")
	}
	return nil
}

// pveVersion is the pve-manager version, or "unknown".
func (e *Env) pveVersion() string {
	pv, _ := e.output("pveversion")
	if m := pveRe.FindStringSubmatch(pv); m != nil {
		return m[1] + "." + m[2] + m[3]
	}
	return "unknown"
}

// changes runs one provider job: `vzdump <vmid> --storage restic-pve` with fixed arguments,
// the job server for the plug-in it loads, the changes stream on stdout and the ack from
// stdin. Proxmox is told success only when the ack says ok, arm or probe for this job.
func (e *Env) changes(vmid, mode string, c Conf) (int, error) {
	lock, err := e.lock(c)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := e.ready(); err != nil {
		return 0, err
	}
	srv, err := provider.Listen(filepath.Join(e.Root, provider.RunDir))
	if err != nil {
		return 0, fail(1, "job socket: %v", err)
	}
	defer srv.Close()
	srv.Logf = func(f string, a ...any) { fmt.Fprintf(e.Stderr, "WARN: "+f+"\n", a...) }

	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return 0, err
	}
	id, _ := strconv.ParseUint(vmid, 10, 32)
	j := &job{e: e, vmid: uint32(id), mode: mode, conf: c, token: hex.EncodeToString(tok[:]), pve: e.pveVersion()}
	j.out = stallWriter(e.Stdout, e.StallTimeout)
	defer j.out.restore()

	// The fixed arguments. The last three neutralise site defaults of /etc/vzdump.conf that
	// would act on this job after the backup machine stored and acked it (pve-guest-common
	// src/PVE/VZDump/Common.pm: protected "If true, mark backup(s) as protected.";
	// 'prune-backups' default "keep-all=1"; remove "Prune older backups according to
	// 'prune-backups'."; pve-manager PVE/VZDump.pm: "$opts->{remove} = 0 if
	// $opts->{'prune-backups'}->{'keep-all'};"). A notes-template is harmless: the plug-in's
	// update_volume_attribute does nothing. A site hook script (option script) runs as for dump.
	cmd := e.command(context.Background(), "nice", "-n", "19", "vzdump", vmid, "--storage", provider.StorageID,
		"--mode", "snapshot", "--bwlimit", c.BwlimitKiB, "--ionice", "7", "--performance", "max-workers="+c.MaxWorkers,
		"--fleecing", "enabled=1,storage="+c.Storage, "--protected", "0", "--prune-backups", "keep-all=1", "--remove", "0")
	cmd.Stdout, cmd.Stderr = e.Stderr, e.Stderr // stdout carries the changes stream; vzdump's log goes to stderr
	cmd.ExtraFiles = []*os.File{lock}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return 0, fail(127, "start vzdump: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		j.serve(ctx, srv, provider.PeerCheck(e.Proc, cmd.Process.Pid))
	}()
	done := make(chan struct{})
	aborted := make(chan bool, 1)
	go e.watchSpace(cmd.Process, c, done, aborted)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var status int
	for {
		select {
		case s := <-sigs:
			if s != syscall.SIGPIPE {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
			j.stop(fmt.Sprintf("the request got %v", s))
			cancel()
			continue
		case err := <-waited:
			status = exitStatus(err)
		}
		break
	}
	close(done)
	cancel()
	select {
	case <-served:
	case <-time.After(30 * time.Second): // a write to a stalled stdout; the exit ends it
	}
	if <-aborted {
		return exNoSpace, nil
	}
	if f := j.fatalErr(); f != nil {
		return 0, f
	}
	if status != 0 {
		return status, nil
	}
	if !j.succeeded() {
		return 0, fail(1, "vzdump ended without a confirmed provider job")
	}
	return 0, nil
}

// job is the state of one provider job, shared by the call handlers.
type job struct {
	e     *Env
	vmid  uint32
	mode  string
	conf  Conf
	token string
	pve   string

	order   provider.Order
	volumes map[string]uint64 // from backup_vm_query_incremental
	out     *stalled          // stdout, bounded by StallLimit per write

	mu        sync.Mutex
	acked     changes.Verb // the success verb of a valid ack
	cleanupOK bool         // the last backup_cleanup said success
	sent      changes.End
	fatal     error
	stopped   string
}

func (j *job) setFatal(code int, format string, a ...any) error {
	err := fail(code, format, a...)
	j.mu.Lock()
	if j.fatal == nil {
		j.fatal = err
	}
	j.mu.Unlock()
	return err
}

func (j *job) fatalErr() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fatal
}

func (j *job) stop(why string) {
	j.mu.Lock()
	j.stopped = why
	j.mu.Unlock()
}

func (j *job) succeeded() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.acked != "" && j.cleanupOK
}

// serve answers the plug-in's calls until it closes the connection.
func (j *job) serve(ctx context.Context, srv *provider.Server, check func(provider.Peer) error) {
	conn, err := srv.Accept(ctx, check)
	if err != nil {
		return // vzdump ended (or never connected): its status says why
	}
	defer conn.Close()
	conn.Timeout = j.e.CallTimeout
	for {
		k, err := conn.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(j.e.Stderr, "WARN: restic-pve: plug-in connection: %v\n", err)
			}
			return
		}
		if err := j.order.Allow(k.Method); err != nil {
			conn.Refuse(err.Error())
			continue
		}
		res, log, err := j.call(ctx, k)
		if err != nil {
			conn.Refuse(err.Error())
			continue
		}
		conn.Reply(res, log)
	}
}

// args decodes a call's arguments strictly: an unknown field is refused.
func args(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("arguments: %v", err)
	}
	return nil
}

type guestArgs struct {
	VMID      uint32  `json:"vmid"`
	Type      string  `json:"type"`
	StartTime float64 `json:"start_time"`
}

func (j *job) guest(g guestArgs) error {
	if g.VMID != j.vmid || g.Type != "qemu" {
		return fmt.Errorf("this job is for VM %d (qemu), not %d (%s)", j.vmid, g.VMID, g.Type)
	}
	return nil
}

type volume struct {
	Size       uint64 `json:"size"`
	BitmapMode string `json:"bitmap-mode"`
	BitmapName string `json:"bitmap-name"`
	NbdPath    string `json:"nbd-path"`
}

func (j *job) call(ctx context.Context, k provider.Call) (any, string, error) {
	switch k.Method {
	case "job_init":
		var a struct {
			Protocol  int     `json:"protocol"`
			StartTime float64 `json:"start_time"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		if a.Protocol != provider.Protocol {
			return nil, "", fmt.Errorf("plug-in protocol %d, this restic-pve speaks %d", a.Protocol, provider.Protocol)
		}
		return nil, fmt.Sprintf("restic-pve: job %s for VM %d, %s", j.token[:8], j.vmid, j.mode), nil
	case "backup_init":
		var a guestArgs
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		if err := j.guest(a); err != nil {
			return nil, "", err
		}
		return map[string]string{"archive-name": fmt.Sprintf("restic-pve/vm-%d/%s", j.vmid, j.token)}, "", nil
	case "backup_get_mechanism":
		var a struct {
			VMID uint32 `json:"vmid"`
			Type string `json:"type"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		if err := j.guest(guestArgs{VMID: a.VMID, Type: a.Type}); err != nil {
			return nil, "", err
		}
		return "nbd", "", nil
	case "backup_vm_query_incremental":
		var a struct {
			VMID    uint32 `json:"vmid"`
			Volumes map[string]struct {
				Size uint64 `json:"size"`
			} `json:"volumes"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		if a.VMID != j.vmid {
			return nil, "", fmt.Errorf("this job is for VM %d", j.vmid)
		}
		if len(a.Volumes) == 0 || len(a.Volumes) > vma.MaxDevID {
			return nil, "", fmt.Errorf("%d disks", len(a.Volumes))
		}
		j.volumes = map[string]uint64{}
		res := map[string]string{}
		for name, v := range a.Volumes {
			if !deviceRe.MatchString(name) || v.Size == 0 || pieces.Pieces(v.Size) > pieces.MaxPieces {
				return nil, "", fmt.Errorf("disk %q of %d bytes", name, v.Size)
			}
			j.volumes[name] = v.Size
			res[name] = "use"
			if j.mode == changes.ModeNew {
				res[name] = "new"
			}
		}
		return res, "", nil
	case "backup_vm":
		var a struct {
			VMID           uint32            `json:"vmid"`
			ConfigB64      string            `json:"config_b64"`
			FirewallB64    *string           `json:"firewall_b64"`
			Volumes        map[string]volume `json:"volumes"`
			BandwidthLimit uint64            `json:"bandwidth_limit"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		return j.backupVM(ctx, a.VMID, a.ConfigB64, a.FirewallB64, a.Volumes, a.BandwidthLimit)
	case "backup_cleanup":
		var a struct {
			guestArgs
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		if err := j.guest(a.guestArgs); err != nil {
			return nil, "", err
		}
		j.mu.Lock()
		acked, sent := j.acked, j.sent
		j.cleanupOK = a.Success && acked != ""
		j.mu.Unlock()
		if a.Success && acked == "" {
			return nil, "", errors.New("Proxmox reports success, but the backup machine confirmed nothing")
		}
		return map[string]any{"stats": map[string]uint64{"archive-size": sent.BytesSent}}, "", nil
	case "backup_handle_log_file":
		var a struct {
			VMID     uint32 `json:"vmid"`
			Filename string `json:"filename"`
		}
		if err := args(k.Args, &a); err != nil {
			return nil, "", err
		}
		return nil, "", nil // the log already went to the backup machine on stderr
	case "job_cleanup":
		if err := args(k.Args, &struct{}{}); err != nil {
			return nil, "", err
		}
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("unknown method %s", k.Method)
}

// nbdPath checks the socket path the plug-in passed: below QemuRun, in the folder qemu-server
// makes for this guest's backup access, and a socket.
func (j *job) nbdPath(p string) error {
	id := strconv.FormatUint(uint64(j.vmid), 10)
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(j.e.QemuRun) + `/` + id + `\.backup-access\.[0-9]{1,10}//?` + id + `-nbd\.backup-access$`)
	if !re.MatchString(p) {
		return fmt.Errorf("nbd-path %q is not the NBD socket of VM %d", p, j.vmid)
	}
	st, err := os.Lstat(p)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("nbd-path %q is not a socket", p)
	}
	return nil
}

// backupVM writes the changes stream for the job and waits for the ack. In mode use, when
// every disk has its record, it reads each piece the record marks, through the read limit,
// and discards it on the export afterwards.
func (j *job) backupVM(ctx context.Context, vmid uint32, cfgB64 string, fwB64 *string, vols map[string]volume, bw uint64) (any, string, error) {
	if vmid != j.vmid {
		return nil, "", fmt.Errorf("this job is for VM %d", j.vmid)
	}
	cfg, err := base64.StdEncoding.DecodeString(cfgB64)
	if err != nil || len(cfg) > vma.MaxBlobSize {
		return nil, "", errors.New("guest config unreadable or too large")
	}
	var fw []byte
	if fwB64 != nil {
		if fw, err = base64.StdEncoding.DecodeString(*fwB64); err != nil || len(fw) > vma.MaxBlobSize {
			return nil, "", errors.New("firewall config unreadable or too large")
		}
	}
	if len(vols) != len(j.volumes) {
		return nil, "", errors.New("the disks differ from those queried")
	}
	names := make([]string, 0, len(vols))
	for name, v := range vols {
		if size, ok := j.volumes[name]; !ok || size != v.Size || !deviceRe.MatchString(name) {
			return nil, "", fmt.Errorf("disk %q differs from the query", name)
		}
		switch v.BitmapMode {
		case changes.BitmapNone, changes.BitmapNew, changes.BitmapReuse:
		default:
			return nil, "", fmt.Errorf("disk %s: bitmap mode %q", name, v.BitmapMode)
		}
		if v.BitmapMode == changes.BitmapReuse && v.BitmapName != bitmapName {
			return nil, "", fmt.Errorf("disk %s: bitmap %q, expected %q", name, v.BitmapName, bitmapName)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	prev := j.prev()
	h := changes.Header{Token: j.token, VMID: j.vmid, Mode: j.mode, PVE: j.pve, Prev: &prev}
	allReuse := true
	for i, name := range names {
		v := vols[name]
		d := changes.Device{ID: uint8(i + 1), Name: name, Size: v.Size, Bitmap: v.BitmapMode}
		if v.BitmapMode == changes.BitmapReuse {
			d.BitmapName = v.BitmapName
		}
		allReuse = allReuse && v.BitmapMode == changes.BitmapReuse
		h.Devices = append(h.Devices, d)
	}
	out := bufio.NewWriterSize(j.out, 1<<20)
	enc := changes.NewEncoder(out)
	written := func(err error) error {
		if err == nil {
			err = out.Flush()
		}
		if err != nil {
			return fmt.Errorf("the backup machine stopped reading the stream: %v", err)
		}
		return nil
	}
	if err := enc.Header(h); err != nil {
		return nil, "", written(err)
	}
	if err := enc.Config("qemu-server.conf", cfg); err != nil {
		return nil, "", written(err)
	}
	if fw != nil {
		if err := enc.Config("qemu-server.fw", fw); err != nil {
			return nil, "", written(err)
		}
	}
	rate := float64(atoi(j.conf.BwlimitKiB)) * 1024
	if bw > 0 && float64(bw) < rate {
		rate = float64(bw)
	}
	limit := newBucket(rate)
	var summary []string
	for _, d := range h.Devices {
		recorded := j.mode == changes.ModeProbe && d.Bitmap == changes.BitmapReuse ||
			j.mode == changes.ModeUse && allReuse
		if !recorded {
			summary = append(summary, d.Name+" record "+d.Bitmap)
			j.e.infof("%s: record %s, no changed-block map (the next backup reads this guest in full)", d.Name, d.Bitmap)
			if err := written(enc.Map(changes.Map{Dev: d.ID})); err != nil {
				return nil, "", err
			}
			continue
		}
		n, err := j.disk(ctx, enc, written, limit, d, vols[d.Name].NbdPath)
		if err != nil {
			return nil, "", err
		}
		summary = append(summary, fmt.Sprintf("%s record reuse, %d of %d pieces changed", d.Name, n, pieces.Pieces(d.Size)))
	}
	if err := written(enc.End()); err != nil {
		return nil, "", err
	}
	a, err := j.readAck(ctx, enc.Expected())
	if err != nil {
		j.e.infof("no confirmed backup: %v", err)
		return nil, "", fmt.Errorf("the backup machine did not confirm the backup: %v", err)
	}
	// Proxmox's record counts from this job once it succeeds: the next job tells the backup
	// machine so. Written before backup_vm returns; a job that fails after it leaves a record
	// that still covers more, never less.
	if err := j.setPrev(); err != nil {
		j.e.infof("no confirmed backup: %v", err)
		return nil, "", fmt.Errorf("the job could not be recorded on this host: %v", err)
	}
	j.mu.Lock()
	j.acked = a.Verb
	j.mu.Unlock()
	// the line Proxmox's task log gets: what really happened, since Proxmox reports every one
	// of these jobs as a successful backup
	what := map[changes.Verb]string{
		changes.VerbProbe: "probe: nothing read or stored",
		changes.VerbArm:   "armed: change record started, nothing stored",
		changes.VerbOK:    "stored: confirmed by the backup machine as snapshot " + a.Snapshot,
	}[a.Verb]
	return nil, fmt.Sprintf("restic-pve: %s (%s)", what, strings.Join(summary, "; ")), nil
}

// prevPath is the file that holds the token of the guest's last provider job that ended with
// an ack ok, arm or probe: every such job moves Proxmox's record of changes on. It lives in the
// job folder on /run (tmpfs): a reboot ends every guest's record too, so Proxmox then reports
// the disks new anyway.
func (j *job) prevPath() string {
	return filepath.Join(j.e.Root, provider.RunDir, fmt.Sprintf("job-%d", j.vmid))
}

// prev is that token, or "" when there is none (or the file is not one of ours).
func (j *job) prev() string {
	b, err := os.ReadFile(j.prevPath())
	if t := strings.TrimSuffix(string(b), "\n"); err == nil && len(t) == 32 && strings.Trim(t, "0123456789abcdef") == "" {
		return t
	}
	return ""
}

// setPrev records this job as the guest's last one: temp file and rename in the job folder.
func (j *job) setPrev() error {
	p := j.prevPath()
	tmp := p + ".new"
	err := os.WriteFile(tmp, []byte(j.token+"\n"), 0o600)
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("record the job in %s: %w", p, err)
	}
	return nil
}

// disk sends one disk's map and, in mode use, its changed pieces. It returns the number of
// pieces the record marks.
func (j *job) disk(ctx context.Context, enc *changes.Encoder, written func(error) error, limit *bucket, d changes.Device, path string) (int, error) {
	if err := j.nbdPath(path); err != nil {
		return 0, j.setFatal(exStream, "%v", err)
	}
	c, err := nbd.Dial(path, d.Name, bitmapName)
	if err != nil {
		return 0, j.setFatal(exStream, "%s: %v", d.Name, err)
	}
	defer c.Close()
	if c.Size() != d.Size {
		return 0, j.setFatal(exStream, "%s: the export has %d bytes, Proxmox said %d", d.Name, c.Size(), d.Size)
	}
	dirty, err := c.Dirty()
	if err != nil {
		return 0, j.setFatal(exStream, "%s: %v", d.Name, err)
	}
	idx, err := nbd.PieceIndexes(dirty, d.Size, pieces.PieceSize)
	if errors.Is(err, nbd.ErrMisaligned) {
		return 0, j.setFatal(exStream, "%s: change record not on 4 MiB boundaries (%v)", d.Name, err)
	}
	if err != nil {
		return 0, j.setFatal(exStream, "%s: %v", d.Name, err)
	}
	j.e.infof("%s: record reuse, %d of %d pieces of 4 MiB changed", d.Name, len(idx), pieces.Pieces(d.Size))
	if err := written(enc.Map(changes.NewMap(d.ID, d.Size, idx))); err != nil {
		return 0, err
	}
	if j.mode != changes.ModeUse {
		return len(idx), nil
	}
	buf := make([]byte, pieces.PieceSize)
	trim := true
	for _, i := range idx {
		if ctx.Err() != nil {
			return 0, errors.New("the request was stopped")
		}
		off := i * pieces.PieceSize
		p := buf[:pieces.PieceLen(d.Size, i)]
		limit.wait(len(p))
		if err := c.Read(off, p); err != nil {
			return 0, j.setFatal(exStream, "%s: read piece %d: %v", d.Name, i, err)
		}
		if err := written(enc.Piece(d.ID, off, p)); err != nil {
			return 0, err
		}
		j.mu.Lock()
		j.sent.PiecesSent++
		j.sent.BytesSent += uint64(len(p))
		j.mu.Unlock()
		if trim {
			// Base.pm: discard after reading, so the fleecing image does not grow
			if err := c.Trim(off, uint64(len(p))); errors.Is(err, nbd.ErrNoTrim) {
				trim = false
			} else if err != nil {
				return 0, j.setFatal(exStream, "%s: discard piece %d: %v", d.Name, i, err)
			}
		}
	}
	return len(idx), nil
}

// readAck waits for the ack line on stdin, the end of the request, or AckTimeout.
func (j *job) readAck(ctx context.Context, want changes.Verb) (changes.Ack, error) {
	type res struct {
		a   changes.Ack
		err error
	}
	ch := make(chan res, 1)
	go func() {
		a, err := changes.ReadAck(j.e.Stdin, j.token, want)
		ch <- res{a, err}
	}()
	select {
	case r := <-ch:
		return r.a, r.err
	case <-ctx.Done():
		j.mu.Lock()
		why := j.stopped
		j.mu.Unlock()
		return changes.Ack{}, fmt.Errorf("%w: %s", changes.ErrAck, why)
	case <-time.After(j.e.AckTimeout):
		return changes.Ack{}, fmt.Errorf("%w: none within %v after the end of the stream", changes.ErrAck, j.e.AckTimeout)
	}
}

// stalled is stdout with a bound: each write of at most 64 KiB must make progress within
// limit, or the job ends ("the backup machine read nothing for ..."). The bound needs a
// pollable descriptor: stdout (a pipe or socket under sshd) is switched to non-blocking for
// the job and back afterwards; where that is not possible, writes block as before.
type stalled struct {
	w     io.Writer
	f     *os.File // the pollable file, nil without a bound
	fd    int
	limit time.Duration
}

// keep holds the pollable stdout for the life of the process: its finalizer would close fd 1.
var keep []*os.File

func stallWriter(f *os.File, limit time.Duration) *stalled {
	s := &stalled{w: f, fd: -1, limit: limit}
	fd := int(f.Fd())
	if syscall.SetNonblock(fd, true) != nil {
		return s
	}
	p := os.NewFile(uintptr(fd), f.Name())
	keep = append(keep, p) // never collected: its finalizer would close the descriptor
	if p.SetWriteDeadline(time.Time{}) != nil {
		syscall.SetNonblock(fd, false)
		return s
	}
	s.w, s.f, s.fd = p, p, fd
	return s
}

func (s *stalled) restore() {
	if s.fd >= 0 {
		syscall.SetNonblock(s.fd, false)
	}
}

func (s *stalled) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n := min(len(p)-done, 64<<10)
		if s.f != nil {
			s.f.SetWriteDeadline(time.Now().Add(s.limit))
		}
		k, err := s.w.Write(p[done : done+n])
		done += k
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return done, fmt.Errorf("the backup machine read nothing of the stream for %v", s.limit)
		}
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// bucket paces reads to rate bytes per second, allowing one piece ahead.
type bucket struct {
	rate  float64
	start time.Time
	total float64
	now   func() time.Time
	sleep func(time.Duration)
}

func newBucket(rate float64) *bucket {
	return &bucket{rate: rate, now: time.Now, sleep: time.Sleep}
}

// wait returns when n more bytes keep the average since the first call at most rate, with a
// burst of one piece.
func (b *bucket) wait(n int) {
	if b.rate <= 0 {
		return
	}
	if b.start.IsZero() {
		b.start = b.now()
	}
	b.total += float64(n)
	due := b.start.Add(time.Duration((b.total - pieces.PieceSize) / b.rate * float64(time.Second)))
	if d := due.Sub(b.now()); d > 0 {
		b.sleep(d)
	}
}
