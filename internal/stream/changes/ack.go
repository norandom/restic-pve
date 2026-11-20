package changes

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Verb is the word after the token in an ack line.
type Verb string

const (
	VerbOK    Verb = "ok"    // the snapshot was stored, checked and accepted
	VerbArm   Verb = "arm"   // the record was started (mode new, or a disk without record)
	VerbProbe Verb = "probe" // a probe; nothing stored
	VerbFail  Verb = "fail"  // the backup machine gives up; Proxmox must not advance the record
)

// MaxAckLine bounds an ack line, newline included.
const MaxAckLine = 1024

// MaxReason bounds the reason of a fail ack.
const MaxReason = 512

// ErrAck is the class of every ack that does not let the provider job succeed: a fail ack,
// a wrong token or verb, a malformed line, and EOF before the line ended.
var ErrAck = errors.New("changes: no valid ack")

// ErrAckEOF: stdin ended before a whole ack line arrived (the backup machine went away).
var ErrAckEOF = fmt.Errorf("%w: input ended before the ack line", ErrAck)

// Ack is one ack line.
type Ack struct {
	Token    string
	Verb     Verb
	Snapshot string // ok: the restic snapshot id (64 lowercase hex)
	Reason   string // fail: printable ASCII, 1 to MaxReason bytes
}

func (a Ack) validate() error {
	if !isHex(a.Token, 32) {
		return fmt.Errorf("token %q is not 32 lowercase hex characters", a.Token)
	}
	switch a.Verb {
	case VerbOK:
		if !isHex(a.Snapshot, 64) || a.Reason != "" {
			return fmt.Errorf("ok needs a 64-character snapshot id, got %q", a.Snapshot)
		}
	case VerbArm, VerbProbe:
		if a.Snapshot != "" || a.Reason != "" {
			return fmt.Errorf("%s takes nothing more", a.Verb)
		}
	case VerbFail:
		if a.Snapshot != "" || !printable(a.Reason, MaxReason) {
			return fmt.Errorf("fail needs a printable reason of 1 to %d bytes", MaxReason)
		}
	default:
		return fmt.Errorf("verb %q", a.Verb)
	}
	return nil
}

// Line returns the ack as one line with its newline.
func (a Ack) Line() (string, error) {
	if err := a.validate(); err != nil {
		return "", fmt.Errorf("changes: ack: %v", err)
	}
	s := "ack " + a.Token + " " + string(a.Verb)
	switch a.Verb {
	case VerbOK:
		s += " " + a.Snapshot
	case VerbFail:
		s += " " + a.Reason
	}
	return s + "\n", nil
}

// ParseAck parses one ack line without its newline.
func ParseAck(line string) (Ack, error) {
	f := strings.SplitN(line, " ", 4)
	if len(f) < 3 || f[0] != "ack" {
		return Ack{}, fmt.Errorf("%w: malformed line %q", ErrAck, clip(line))
	}
	a := Ack{Token: f[1], Verb: Verb(f[2])}
	switch {
	case a.Verb == VerbOK && len(f) == 4:
		a.Snapshot = f[3]
	case a.Verb == VerbFail && len(f) == 4:
		a.Reason = f[3]
	case len(f) == 4:
		return Ack{}, fmt.Errorf("%w: malformed line %q", ErrAck, clip(line))
	}
	if err := a.validate(); err != nil {
		return Ack{}, fmt.Errorf("%w: %v", ErrAck, err)
	}
	return a, nil
}

// ReadAck reads exactly one ack line from r (byte by byte, so nothing after it is consumed)
// and returns it when it carries token and the success verb want. A fail ack returns the ack
// and an ErrAck error with its reason; everything else that is not the expected success is an
// ErrAck error too.
func ReadAck(r io.Reader, token string, want Verb) (Ack, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			if len(line) >= MaxAckLine-1 {
				return Ack{}, fmt.Errorf("%w: line longer than %d bytes", ErrAck, MaxAckLine)
			}
			line = append(line, b[0])
			continue
		}
		if err == io.EOF {
			return Ack{}, ErrAckEOF
		}
		if err != nil {
			return Ack{}, fmt.Errorf("%w: read: %v", ErrAck, err)
		}
	}
	a, err := ParseAck(string(line))
	if err != nil {
		return Ack{}, err
	}
	if a.Token != token {
		return Ack{}, fmt.Errorf("%w: ack for job %s, this job is %s", ErrAck, a.Token, token)
	}
	if a.Verb == VerbFail {
		return a, fmt.Errorf("%w: the backup machine failed: %s", ErrAck, a.Reason)
	}
	if a.Verb != want {
		return Ack{}, fmt.Errorf("%w: ack %s, this job needs %s", ErrAck, a.Verb, want)
	}
	return a, nil
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
