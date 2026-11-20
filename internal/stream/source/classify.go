package source

import (
	"regexp"
	"strings"
	"sync"
)

// Level is the severity of one vzdump stderr line.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Consistency values (trailer, manifest and the "consistency" JSON line).
const (
	Application = "application"
	Crash       = "crash"
	// Stopped: the guest was not running; vzdump read its disks offline
	// (backup mode stop, no fs-freeze needed). The cleanest state.
	Stopped = "stopped"
	// DetailStopped is the detail of a stopped guest.
	DetailStopped = "guest was stopped; disks read offline, no fs-freeze needed"
	// DetailUnknown is the detail when the log has no recognisable freeze line.
	DetailUnknown = "agent status unknown"
)

const (
	// MaxWarnings bounds the warn/error lines kept for the trailer.
	MaxWarnings = 100
	// MaxLineBytes bounds one kept or forwarded line.
	MaxLineBytes = 1000
)

// effect is what a matching pattern does to the classification.
type effect int

const (
	effFleecing      effect = iota + 1 // fleecing not active: abort (exit 5)
	effFreezeSkipped                   // fs-freeze skipped: crash-consistent
	effFreezeFailed                    // fs-freeze failed or timed out: crash-consistent
	effFreezeIssued                    // fs-freeze issued: application-consistent unless it failed
	effGuestFS                         // filesystem freeze error: abort (exit 10)
	effBackupFailed                    // explicit vzdump failure reason decides retry
	effStopped                         // vzdump fell back to stop mode: the guest was not running
)

// pattern is one row of the classification table. minLevel restricts the
// row to lines of at least that level ("" = any level).
type pattern struct {
	re       *regexp.Regexp
	minLevel Level
	eff      effect
}

// patterns is the single vzdump log classification table (design: source).
// Rows are tried in order; the first match wins.
var patterns = []pattern{
	{regexp.MustCompile(`(?i)(fs-?freeze|freeze guest fs).*(Input/output error|I/O error|Structure needs cleaning|Read-only file system)`), "", effGuestFS},
	{regexp.MustCompile(`^ERROR: Backup of VM [0-9]+ failed - (.+)$`), "", effBackupFailed},
	// Abort on any fleecing warning or error. If fleecing is unsupported, vzdump
	// continues without it. This must never happen (Requirement 6.3).
	{regexp.MustCompile(`(?i)fleecing`), LevelWarn, effFleecing},
	// Informational fallbacks that mean fleecing is not active.
	{regexp.MustCompile(`(?i)fleecing.*(not supported|unsupported|not active|disabled|without fleecing|fail)|without fleecing`), "", effFleecing},
	{regexp.MustCompile(`(?i)skipping guest-agent 'fs-freeze'`), "", effFreezeSkipped},
	// A warn/error line about the freeze command, or any line reporting its failure.
	{regexp.MustCompile(`(?i)(fs-?freeze|freeze guest fs)`), LevelWarn, effFreezeFailed},
	{regexp.MustCompile(`(?i)(fs-?freeze|freeze guest fs).*(fail|error|timeout|timed out|unable)|(fail|error|timeout|timed out|unable).*(fs-?freeze|freeze guest fs)`), "", effFreezeFailed},
	{regexp.MustCompile(`(?i)issuing guest-agent 'fs-freeze'`), "", effFreezeIssued},
	// vzdump --mode snapshot switches to stop mode only for a guest that is not running.
	{regexp.MustCompile(`^INFO: backup mode: stop$`), "", effStopped},
}

// benignLines are normal output from qemu-img and the PVE notification system.
// They stay informational regardless of vzdump's prefix and never enter warnings.
var benignLines = []*regexp.Regexp{
	// qemu-img image creation
	regexp.MustCompile(`^Formatting '`),
	// PVE notification system
	regexp.MustCompile(`does not have a configured email address`),
	regexp.MustCompile(`could not notify via target`),
	regexp.MustCompile(`could not notify via endpoint`),
	regexp.MustCompile(`no recipients provided`),
	// Proxmox newer than restic-pve's plug-in was checked against: it warns in every process
	// that loads storage plug-ins, the dump request's vzdump too; restic-pve info and install
	// tell the admin to update restic-pve
	regexp.MustCompile(`^Plugin "PVE::Storage::Custom::ResticPVEPlugin" is implementing an older storage API, an upgrade is recommended$`),
}

var transientReason = regexp.MustCompile(`(?i)\b(lock|locked|busy|timeout|timed out|temporarily|try again|connection)\b`)

var levelPrefix = regexp.MustCompile(`^(INFO|WARN|WARNING|ERROR|ERR):\s?`)

func benign(msg string) bool {
	for _, re := range benignLines {
		if re.MatchString(msg) {
			return true
		}
	}
	return false
}

// ParseLevel returns the level of a vzdump log line and its message without
// the prefix. A line without a vzdump level prefix comes from ssh or the PVE
// wrapper and is an error, except for the known benignLines.
func ParseLevel(line string) (Level, string) {
	m := levelPrefix.FindStringSubmatch(line)
	msg := line
	if m != nil {
		msg = line[len(m[0]):]
	}
	if benign(msg) {
		return LevelInfo, msg
	}
	if m == nil {
		return LevelError, line
	}
	switch m[1] {
	case "INFO":
		return LevelInfo, msg
	case "WARN", "WARNING":
		return LevelWarn, msg
	default:
		return LevelError, msg
	}
}

func atLeast(l, min Level) bool {
	rank := map[Level]int{"": 0, LevelInfo: 1, LevelWarn: 2, LevelError: 3}
	return rank[l] >= rank[min]
}

// Verdict is the classification of one line.
type Verdict struct {
	Level    Level
	Forward  bool // forward as a {"type":"vzdump"} line (warn/error only)
	Fleecing bool // fleecing not active: the backup must abort (exit 5)
	GuestFS  bool // guest filesystem error: the backup must abort (exit 10)
}

// Classifier accumulates the vzdump log of one backup. It is safe for
// concurrent use.
type Classifier struct {
	mu       sync.Mutex
	issued   bool
	stopped  bool
	problem  string // freeze skipped/failed detail
	fleecing string // first fleecing problem line
	guestFS  string // first guest filesystem freeze error
	failure  string // first explicit vzdump backup failure reason
	warnings []string
}

// NewClassifier returns an empty classifier (no log: crash, agent status unknown).
func NewClassifier() *Classifier { return &Classifier{} }

func clip(s string) string {
	if len(s) > MaxLineBytes {
		return s[:MaxLineBytes]
	}
	return s
}

// Line classifies one stderr line (without its newline).
func (c *Classifier) Line(line string) Verdict {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return Verdict{Level: LevelInfo}
	}
	lvl, msg := ParseLevel(line)
	v := Verdict{Level: lvl, Forward: lvl != LevelInfo}
	if benign(msg) {
		return v
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.Forward {
		if len(c.warnings) < MaxWarnings {
			c.warnings = append(c.warnings, clip(line))
		}
	}
	for _, p := range patterns {
		if !atLeast(lvl, p.minLevel) || !p.re.MatchString(line) {
			continue
		}
		switch p.eff {
		case effGuestFS:
			v.GuestFS = true
			if c.guestFS == "" {
				c.guestFS = clip(line)
			}
			if c.problem == "" {
				c.problem = "guest-agent fs-freeze failed: " + clip(msg)
			}
		case effBackupFailed:
			if c.failure == "" {
				c.failure = p.re.FindStringSubmatch(line)[1]
			}
		case effFleecing:
			v.Fleecing = true
			if c.fleecing == "" {
				c.fleecing = clip(line)
			}
		case effFreezeSkipped:
			if c.problem == "" {
				c.problem = "guest-agent fs-freeze skipped: " + clip(msg)
			}
		case effFreezeFailed:
			if c.problem == "" {
				c.problem = "guest-agent fs-freeze failed: " + clip(msg)
			}
		case effFreezeIssued:
			c.issued = true
		case effStopped:
			c.stopped = true
		}
		break
	}
	return v
}

// Consistency returns the result for the log seen so far: crash with a detail
// after a freeze problem, stopped when vzdump ran in stop mode (guest not
// running), application when fs-freeze was issued, crash otherwise.
func (c *Classifier) Consistency() (value, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.problem != "":
		return Crash, c.problem
	case c.stopped:
		return Stopped, DetailStopped
	case c.issued:
		return Application, "guest-agent fs-freeze issued without error"
	default:
		return Crash, DetailUnknown
	}
}

// FleecingProblem returns the first line showing fleecing is not active, or "".
func (c *Classifier) FleecingProblem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fleecing
}

// GuestFSProblem returns the first guest filesystem freeze error, or "".
// Unlike a missing agent or timeout, this requires an immediate abort (exit 10).
func (c *Classifier) GuestFSProblem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guestFS
}

// BackupFailure returns vzdump's failure reason and whether it is transient.
// Only lock/locked, busy, timeout/timed out, temporarily, try again and
// connection failures permit retry; all other explicit reasons are fatal.
func (c *Classifier) BackupFailure() (reason string, transient bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clip(c.failure), transientReason.MatchString(c.failure)
}

// Warnings returns the warn/error lines (at most MaxWarnings, each at most
// MaxLineBytes) for the trailer.
func (c *Classifier) Warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.warnings...)
}
