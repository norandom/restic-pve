package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/source"
	"github.com/norandom/restic-pve/internal/stream/verify"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// version is reported in the trailer and manifest ("vmastream <version>"). It stays "dev", the
// value of every vmastream build so far: the trailer is part of the stream bytes restic stores,
// and the switch to restic-pve stream keeps them byte for byte the same.
var version = "dev"

func toolName() string { return "vmastream " + version }

// ramMarginBytes is the RAM the reorder buffer must have on top of the
// zero-cluster bitmaps (a few clusters or hash slots), so that --ram-mib can
// never be entirely taken by bitmaps.
const ramMarginBytes = 4 * reorder.ClusterSize

// cutGrace is the time allowed for a source to exit after its stream ends
// early. This allows its real exit status to be reported before it is killed.
const cutGrace = 30 * time.Second

// checkRAMCap rejects a RAM cap that cannot hold the zero-cluster bitmaps
// of the guest's devices plus ramMarginBytes.
func checkRAMCap(ramBytes int64, l reorder.Layout) error {
	need := reorder.BitmapBytes(l) + ramMarginBytes
	if ramBytes < need {
		return usagef("--ram-mib %d MiB is too small for this guest: the zero-cluster bitmaps need %d bytes plus a %d byte margin (%d MiB minimum)",
			ramBytes>>20, reorder.BitmapBytes(l), int64(ramMarginBytes), (need+(1<<20)-1)>>20)
	}
	return nil
}

func cmdBackup(args []string, _ io.Reader, stdout, stderr io.Writer) (exitCode, bool, error) {
	o, err := parseBackup(args, stderr)
	if err != nil {
		return exitUsage, false, err
	}
	return backup(context.Background(), o, stdout, stderr)
}

// --- stderr JSON lines ---

type progressLine struct {
	Type       string  `json:"type"`
	Dev        string  `json:"dev"`
	BytesDone  uint64  `json:"bytes_done"`
	TotalBytes uint64  `json:"total_bytes"`
	Percent    float64 `json:"percent"`
	RAMBytes   int64   `json:"ram_bytes"`
	Patches    uint64  `json:"patches"`
}

type consistencyLine struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

type vzdumpLine struct {
	Type  string `json:"type"`
	Level string `json:"level"`
	Line  string `json:"line"`
}

// eventLog writes the JSON lines to stderr. It is a source.Events, so its
// methods are called from the source's stderr goroutine as well.
type eventLog struct {
	mu   sync.Mutex
	w    io.Writer
	once sync.Once
}

func (e *eventLog) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(append(b, '\n'))
}

// Consistency emits the consistency line (once).
func (e *eventLog) Consistency(value, detail string) {
	e.once.Do(func() { e.emit(consistencyLine{Type: "consistency", Value: value, Detail: detail}) })
}

// VzdumpLine forwards one warn/error line of the vzdump log.
func (e *eventLog) VzdumpLine(level, line string) {
	e.emit(vzdumpLine{Type: "vzdump", Level: level, Line: line})
}

// progress emits a progress line every interval and a final one.
type progress struct {
	ev    *eventLog
	every time.Duration
	next  time.Time
	sizes map[uint8]vma.DevInfo
	total uint64
	done  uint64
	dev   string
}

func newProgress(ev *eventLog, every time.Duration, devs []vma.DevInfo) *progress {
	p := &progress{ev: ev, every: every, next: time.Now().Add(every), sizes: map[uint8]vma.DevInfo{}}
	for _, d := range devs {
		p.sizes[d.ID] = d
		p.total += d.Size
	}
	return p
}

func (p *progress) add(c vma.Cluster) {
	d := p.sizes[c.Dev]
	p.dev = d.Name
	lo := uint64(c.Index) * vma.ClusterSize
	if lo < d.Size {
		p.done += min(vma.ClusterSize, d.Size-lo)
	}
}

func (p *progress) tick(st reorder.Stats) {
	if now := time.Now(); !now.Before(p.next) {
		p.next = now.Add(p.every)
		p.emit(st)
	}
}

func (p *progress) emit(st reorder.Stats) {
	pct := 100.0
	if p.total > 0 {
		pct = math.Round(float64(p.done)*1000/float64(p.total)) / 10
	}
	p.ev.emit(progressLine{Type: "progress", Dev: p.dev, BytesDone: p.done, TotalBytes: p.total, Percent: pct,
		RAMBytes: st.RAMInUse, Patches: st.PatchCount})
}

// --- pipeline ---

// failure holds a pipeline failure and its exit class. eof means the stream
// ended early (truncated or missing clusters). readErr means a plain read
// error. For both, the source's exit status decides the final result.
type failure struct {
	code    exitCode
	eof     bool
	readErr bool
	err     error
}

func (f *failure) dependsOnSource() bool { return f.eof || f.readErr }

// teeSink feeds the reordered stream to the fingerprint and the RVS1 writer, and tells the
// block view (tr, nil when it is off) what was written.
type teeSink struct {
	fp *verify.Fingerprint
	w  *rvs.Writer
	tr *blockmap.Tracker
}

func (t teeSink) InOrder(dev uint8, index uint32, data []byte) error {
	if err := t.fp.InOrder(dev, index, data); err != nil {
		return err
	}
	if err := t.w.InOrder(dev, index, data); err != nil {
		return err
	}
	if t.tr != nil {
		t.tr.InOrder(dev, index, data != nil)
	}
	return nil
}

func (t teeSink) Patch(dev uint8, index uint32, data []byte) error {
	if err := t.fp.Patch(dev, index, data); err != nil {
		return err
	}
	if err := t.w.Patch(dev, index, data); err != nil {
		return err
	}
	if t.tr != nil {
		t.tr.Patch(dev, index)
	}
	return nil
}

// streamed is the state of a pipeline that consumed the whole VMA stream.
type streamed struct {
	header  vma.Header
	writer  *rvs.Writer
	devices []rvs.TrailerDevice
	stats   reorder.Stats
}

func readFailure(err error) *failure {
	var fe *vma.FormatError
	if errors.As(err, &fe) {
		switch fe.Kind {
		case vma.ErrForbiddenDevice:
			return &failure{code: exitForbiddenContent, err: err}
		case vma.ErrTruncated, vma.ErrMissingCluster:
			return &failure{code: exitSourceFailed, eof: true, err: fmt.Errorf("stream cut: %w", err)}
		default:
			return &failure{code: exitIntegrity, err: err}
		}
	}
	return &failure{code: exitLocalIO, readErr: true, err: err}
}

func bufferFailure(err error, fp *verify.Fingerprint) *failure {
	switch {
	case fp.Err() != nil:
		return &failure{code: exitInternal, err: fmt.Errorf("fingerprint: %w", fp.Err())}
	case errors.Is(err, reorder.ErrCapacity):
		return &failure{code: exitLocalIO, err: fmt.Errorf("%w (raise --ram-mib or lower MAX_WORKERS in /etc/restic-pve.conf)", err)}
	case errors.Is(err, rvs.ErrSequence), errors.Is(err, reorder.ErrInvalidCluster), errors.Is(err, reorder.ErrMissingCluster):
		return &failure{code: exitInternal, err: err}
	default: // RVS1 output (stdout) write error
		return &failure{code: exitLocalIO, err: err}
	}
}

// pipeline runs VMA reader → reorder buffer → (fingerprint, RVS1 writer).
// It writes everything but the trailer; nothing it leaves on stdout is a
// complete RVS1 stream.
func pipeline(o *backupOptions, in io.Reader, stdout io.Writer, ev *eventLog, view *blockView) (*streamed, *failure) {
	r, err := vma.NewReader(in)
	if err != nil {
		return nil, readFailure(err)
	}
	h := *r.Header()
	layout := reorder.Layout{Devices: h.Devices}
	ramBytes := int64(o.RAMMiB) << 20
	if err := checkRAMCap(ramBytes, layout); err != nil {
		return nil, &failure{code: exitUsage, err: err}
	}
	w, err := rvs.NewWriter(stdout, h)
	if err != nil {
		if errors.Is(err, rvs.ErrSequence) {
			return nil, &failure{code: exitInternal, err: err}
		}
		return nil, &failure{code: exitLocalIO, err: err}
	}
	fp := verify.NewFingerprint(layout)
	tr := view.begin(h.Devices)
	caps := reorder.Caps{RAMBytes: ramBytes}
	buf, err := reorder.New(caps, layout, teeSink{fp: fp, w: w, tr: tr}, fp)
	if err != nil {
		return nil, &failure{code: exitInternal, err: err}
	}
	prog := newProgress(ev, o.ProgressInterval, h.Devices)
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, readFailure(err)
		}
		prog.add(c)
		if tr != nil {
			tr.Arrived(c)
		}
		if err := buf.Put(c); err != nil {
			return nil, bufferFailure(err, fp)
		}
		prog.tick(buf.Stats())
	}
	if err := buf.Finish(); err != nil {
		return nil, bufferFailure(err, fp)
	}
	devices, err := fp.Devices()
	if err != nil {
		return nil, &failure{code: exitInternal, err: err}
	}
	st := buf.Stats()
	prog.emit(st)
	return &streamed{header: h, writer: w, devices: devices, stats: st}, nil
}

// decide combines the pipeline outcome with the source outcome.
func decide(f *failure, srcErr error) (exitCode, bool, error) {
	var se *source.Error
	errors.As(srcErr, &se)
	if errors.Is(srcErr, source.ErrGuestFilesystem) {
		return exitGuestFilesystem, false, srcErr
	}
	if errors.Is(srcErr, source.ErrFleecing) {
		return exitFleecingInactive, false, srcErr
	}
	if f != nil {
		if f.dependsOnSource() && se != nil {
			return exitSourceFailed, se.Transient, fmt.Errorf("%v (%v)", f.err, srcErr)
		}
		return f.code, false, f.err
	}
	if srcErr != nil {
		transient := se != nil && se.Transient
		return exitSourceFailed, transient, srcErr
	}
	return exitOK, false, nil
}

func backup(ctx context.Context, o *backupOptions, stdout, stderr io.Writer) (code exitCode, transient bool, err error) {
	started := time.Now().UTC()
	ev := &eventLog{w: stderr}
	view := startView(o, ev, started)
	if view != nil {
		stdout = view.pub.Wrap(stdout)
		defer func() { view.finish(code, err) }()
	}
	return backupStream(ctx, o, stdout, ev, started, view)
}

// backupStream runs the stream; view is nil when the block view is off.
func backupStream(ctx context.Context, o *backupOptions, stdout io.Writer, ev *eventLog, started time.Time,
	view *blockView) (exitCode, bool, error) {
	stderr := ev.w
	var in io.Reader
	var src source.Waiter
	if o.Input != "" {
		f, err := os.Open(o.Input)
		if err != nil {
			return exitLocalIO, false, err
		}
		defer f.Close()
		in = bufio.NewReaderSize(f, 1<<20)
	} else {
		r, w, err := source.Open(ctx, source.Options{SSHConfig: o.SSHConfig, Host: o.SSHHost, VMID: o.VMID}, ev)
		if err != nil {
			if errors.Is(err, source.ErrOptions) {
				return exitUsage, false, &usageError{err.Error()}
			}
			var se *source.Error
			transient := errors.As(err, &se) && se.Transient
			return exitSourceFailed, transient, err
		}
		defer r.Close()
		in = bufio.NewReaderSize(r, 1<<20)
		src = w
	}

	st, f := pipeline(o, in, stdout, ev, view)

	cls := source.NewClassifier()
	var srcErr error
	if src != nil {
		switch {
		case f == nil:
		case f.eof: // the stream ended: let the source exit with its own status
			t := time.AfterFunc(cutGrace, src.Abort)
			defer t.Stop()
		default:
			src.Abort()
		}
		srcErr = src.Wait()
		cls = src.Classifier()
	} else {
		ev.Consistency(cls.Consistency())
	}
	if code, transient, err := decide(f, srcErr); code != exitOK {
		return code, transient, err
	}

	value, detail := cls.Consistency()
	h := st.header
	uuid := formatUUID(h.UUID)
	tr := rvs.Trailer{
		VMAUUID: uuid, VMACTime: h.CTime, Devices: st.devices,
		PatchCount: st.stats.PatchCount, EarlyClusters: st.stats.EarlyClusters,
		BufferRAMPeak: st.stats.RAMPeak,
		Consistency:   value, VzdumpWarnings: cls.Warnings(), Tool: toolName(),
	}
	if err := st.writer.Close(tr); err != nil {
		if errors.Is(err, rvs.ErrSequence) {
			return exitInternal, false, err
		}
		return exitLocalIO, false, err
	}
	name, ostype := guestConfigValues(h.Configs)
	m := &verify.Manifest{
		Schema: verify.ManifestSchema, VMID: o.VMID, PVEName: name, OSType: ostype,
		Consistency: value, ConsistencyDetail: detail,
		Devices: st.devices, Configs: verify.ConfigDigests(h.Configs),
		VMAUUID: uuid, VMACTime: h.CTime, PatchCount: st.stats.PatchCount,
		BufferRAMPeak: st.stats.RAMPeak,
		Started:       started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339),
		Tool: toolName(),
	}
	if err := verify.WriteManifestFile(o.ManifestOut, m); err != nil {
		return exitLocalIO, false, err
	}
	writeResult(stderr, exitOK, false, "backup: ok", st.stats)
	return exitOK, false, nil
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// guestConfigValues reads "name:" and "ostype:" from the current section of
// qemu-server.conf (snapshot sections start with "[").
func guestConfigValues(cfgs []vma.ConfigBlob) (name, ostype string) {
	for _, c := range cfgs {
		if c.Name != "qemu-server.conf" {
			continue
		}
		for _, l := range bytes.Split(c.Data, []byte("\n")) {
			s := strings.TrimSpace(string(l))
			if strings.HasPrefix(s, "[") {
				break
			}
			if k, v, ok := strings.Cut(s, ":"); ok {
				switch strings.TrimSpace(k) {
				case "name":
					name = strings.TrimSpace(v)
				case "ostype":
					ostype = strings.TrimSpace(v)
				}
			}
		}
	}
	return name, ostype
}
