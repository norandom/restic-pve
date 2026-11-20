package run

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/source"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// job is one provider job: ssh ... changes <vmid> <mode>, its stream and its ack.
type job struct {
	p     *source.Process
	in    io.WriteCloser
	dec   *changes.Decoder
	h     *changes.Header
	cfgs  []vma.ConfigBlob
	lost  chan int // after the end frame: bytes the host wrote after it, sent when its output ends
	ended bool     // the host has its answer (fail or the ack) or is gone
	acked bool     // the success ack was written
}

// startJob starts a provider job and reads its header.
func (r *runner) startJob(mode string) (*job, error) {
	r.ev.skip = mode != changes.ModeProbe // a stored run's consistency is its snapshot source's
	rd, w, err := source.Open(r.ctx, source.Options{SSHConfig: r.j.SSHConfig, Host: r.j.SSHHost, VMID: r.j.VMID, SSHProgram: r.e.SSHProgram,
		Request: []string{"changes", strconv.FormatUint(uint64(r.j.VMID), 10), mode}, Ack: true, Env: source.SafeEnv(os.Environ())}, r.ev)
	if err != nil {
		return nil, err
	}
	jb := &job{p: w.(*source.Process), dec: changes.NewDecoder(rd)}
	jb.in = jb.p.Stdin()
	f, err := jb.dec.Next()
	if err != nil {
		return nil, jb.stop(fmt.Errorf("changes stream: %w", err), true)
	}
	if jb.h = f.Header; jb.h.VMID != r.j.VMID || jb.h.Mode != mode {
		return nil, jb.stop(withCode(CodeStream, fmt.Errorf("the host answered for VM %d in mode %s", jb.h.VMID, jb.h.Mode)), false)
	}
	return jb, nil
}

// stop ends a job this side gives up, once: the host hears fail (Proxmox keeps its record),
// then the session ends. broke: the stream broke off or broke a rule.
func (jb *job) stop(err error, broke bool) error {
	if jb.ended {
		return err
	}
	jb.ended = true
	reason := []byte(err.Error())
	for i, c := range reason {
		if c < 0x20 || c > 0x7e {
			reason[i] = ' '
		}
	}
	if jb.h != nil {
		if line, lerr := (changes.Ack{Token: jb.h.Token, Verb: changes.VerbFail, Reason: string(reason[:min(len(reason), changes.MaxReason)])}).Line(); lerr == nil {
			io.WriteString(jb.in, line)
		}
	}
	jb.in.Close()
	if jb.lost != nil && !broke { // after the end frame: the host ends the job on fail
		t := time.AfterFunc(10*time.Second, jb.p.Abort)
		defer t.Stop()
		jb.p.Wait()
		return err
	}
	return cut(jb.p, err, broke) // pieces may still be on their way
}

// cut ends a source the reading side gave up: a stream that broke waits a little for the
// host's own reason (ssh lost, plug-in not ready, vzdump failed), which wins over exit 3;
// anything else stops ssh at once.
func cut(p *source.Process, err error, broke bool) error {
	if !broke {
		p.Abort()
		p.Wait()
		return err
	}
	t := time.AfterFunc(10*time.Second, p.Abort)
	defer t.Stop()
	if werr := p.Wait(); werr != nil && !strings.Contains(werr.Error(), "source aborted") {
		return werr
	}
	return withCode(CodeStream, err)
}

// frames reads the job's frames to the end frame: config frames are kept, maps and pieces go
// to the callbacks (an error of theirs stops the job).
func (jb *job) frames(onMap func(changes.Map) error, onPiece func(changes.Piece) error) error {
	for {
		f, err := jb.dec.Next()
		if err != nil {
			return jb.stop(fmt.Errorf("changes stream: %w", err), true)
		}
		switch {
		case f.Config != nil:
			jb.cfgs = append(jb.cfgs, vma.ConfigBlob{Name: f.Config.Name, Data: bytes.Clone(f.Config.Data)})
		case f.Map != nil && onMap != nil:
			err = onMap(*f.Map)
		case f.Piece != nil:
			err = onPiece(*f.Piece)
		case f.End != nil:
			jb.lost = make(chan int, 1)
			go func() { // the host keeps its output open until it has the ack
				var b [1]byte
				n, _ := jb.p.Read(b[:])
				jb.lost <- n
			}()
			return nil
		}
		if err != nil {
			return jb.stop(err, false)
		}
	}
}

// ready says whether the host still waits for the ack.
func (jb *job) ready() error {
	select {
	case n := <-jb.lost:
		jb.lost <- n
		jb.ended = true
		jb.in.Close()
		if err := jb.p.Wait(); err != nil {
			return err
		}
		return withCode(CodeSource, errors.New("the Proxmox side ended the job before the ack"))
	default:
		return nil
	}
}

// commit writes the record state, then sends the ack, and waits for the job's end.
func (r *runner) commit(jb *job, a changes.Ack) error {
	line, err := a.Line()
	if err == nil {
		err = r.saveRecord()
	}
	if err != nil {
		return jb.stop(err, false)
	}
	jb.ended, jb.acked = true, true
	io.WriteString(jb.in, line) // a lost session shows in Wait
	jb.in.Close()
	n := <-jb.lost
	if err := jb.p.Wait(); err != nil {
		return err
	}
	if n > 0 {
		return withCode(CodeStream, errors.New("changes stream: data after the end frame"))
	}
	return nil
}
