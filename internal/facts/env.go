// Package facts collects what the backup role's screens show (status, keys, space, watch) from
// the files of the backup machine and, at most every six hours, from the storage. It reads
// only: config, records, job records, the status files and the locks. Paths and secrets come
// from the environment, never from a built-in default; the same code runs inside a container
// and directly on a machine.
package facts

import (
	"fmt"
	"strings"
)

// The variables the backup role reads.
const (
	StateEnv        = "RESTIC_PVE_STATE"       // state root; the guest profile under guests/
	ConfigEnv       = "BACKUP_CONFIG"          // path of backup.toml
	PasswordEnv     = "RESTIC_PASSWORD"        // the repository password in use
	BaseEnv         = "RESTIC_REPOSITORY_BASE" // overrides [repository] base
	RcloneConfigEnv = "RCLONE_CONFIG"          // the rclone config file
	KeysEnv         = "RESTIC_PVE_KEYS"        // the folder with the key files
	StartHintEnv    = "RESTIC_PVE_START_HINT"  // the command that starts a backup here
	WebhookEnv      = "DISCORD_WEBHOOK_URL"    // alerts on when set; the value is never read into output
)

// MissingError is a variable the command needs and does not have. The command exits 78.
type MissingError struct{ Var string }

var missingWords = map[string][2]string{
	StateEnv:     {"the state folder of this backup machine", "/state"},
	ConfigEnv:    {"the path of backup.toml, the list of what is backed up", "/app/config/backup.toml"},
	PasswordEnv:  {"the repository password; it comes from .env", "the line RESTIC_PASSWORD=<password> in .env"},
	KeysEnv:      {"the folder that holds this machine's key files", "/config"},
	StartHintEnv: {"the command that starts a backup on this machine", "'./backup.sh <source>'"},
}

func (e *MissingError) Error() string {
	w, ok := missingWords[e.Var]
	if !ok {
		return e.Var + " is not set. Nothing was changed."
	}
	ex := e.Var + "=" + w[1]
	if strings.HasPrefix(w[1], "the line") {
		ex = w[1]
	}
	return fmt.Sprintf("%s is not set: it is %s. Nothing was changed. Set it, for example %s.", e.Var, w[0], ex)
}

// Require returns a MissingError for the first of names that is empty.
func Require(getenv func(string) string, names ...string) error {
	for _, n := range names {
		if getenv(n) == "" {
			return &MissingError{n}
		}
	}
	return nil
}

// ConfigError is a config file that cannot be read or holds a value outside the rules. The
// command exits 78.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

// Hint returns the start command for name: the start hint with "<source>" replaced, or the
// hint as it is when it names no placeholder.
func Hint(hint, name string) string {
	if name == "" {
		return hint
	}
	return strings.ReplaceAll(hint, "<source>", name)
}
