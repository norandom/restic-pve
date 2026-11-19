package facts

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the typed part of backup.toml the screens need. Everything else in the file is
// ignored here; backupctl checks it.
type Config struct {
	Base      string        // [repository] base, or RESTIC_REPOSITORY_BASE
	Files     []string      // names of the local sources ([[source]] of any type but proxmox)
	Proxmox   []GuestSource // [[source]] of type proxmox with their guests
	Retention Retention
	StaleDays int             // [alerts] stale_days, default 8
	Schedule  map[string]Cron // [schedule]: local, guests, guest_restore_test (UTC)
}

// GuestSource is a Proxmox host and the guests backed up from it; they share one repository.
type GuestSource struct {
	Name   string
	Guests []Guest
}

// Guest is one guest of a Proxmox source.
type Guest struct {
	VMID     int
	Alias    string
	KeepLast int
}

// Retention are the keep rules of the local sources.
type Retention struct{ Weekly, Monthly, Yearly int }

// Profiles of the schedule.
const (
	ProfileLocal       = "local"
	ProfileGuests      = "guests"
	ProfileRestoreTest = "guest_restore_test"
)

type rawConfig struct {
	Repository struct {
		Base string `toml:"base"`
	} `toml:"repository"`
	Source []struct {
		Name     string `toml:"name"`
		Type     string `toml:"type"`
		KeepLast *int   `toml:"keep_last"`
		Guest    []struct {
			VMID     int    `toml:"vmid"`
			Alias    string `toml:"alias"`
			KeepLast *int   `toml:"keep_last"`
		} `toml:"guest"`
	} `toml:"source"`
	Retention struct {
		Weekly  int `toml:"keep_weekly"`
		Monthly int `toml:"keep_monthly"`
		Yearly  int `toml:"keep_yearly"`
	} `toml:"retention"`
	Alerts struct {
		StaleDays *int `toml:"stale_days"`
	} `toml:"alerts"`
	Schedule map[string]string `toml:"schedule"`
}

// LoadConfig reads the file at path; base, when not empty, overrides [repository] base.
func LoadConfig(path, base string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, &ConfigError{fmt.Sprintf("cannot read the config %s (%s): check %s, for example %s=/app/config/backup.toml",
			path, plainErr(err), ConfigEnv, ConfigEnv)}
	}
	var raw rawConfig
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return Config{}, &ConfigError{fmt.Sprintf("the config %s is not valid TOML: %v. Fix the file; ./setup.sh checks it too", path, err)}
	}
	c := Config{Base: raw.Repository.Base, StaleDays: 8, Schedule: map[string]Cron{},
		Retention: Retention{raw.Retention.Weekly, raw.Retention.Monthly, raw.Retention.Yearly}}
	if base != "" {
		c.Base = base
	}
	if c.Base == "" {
		return Config{}, &ConfigError{fmt.Sprintf("the config %s has no [repository] base: add it, for example base = \"rclone:drive:backups\"", path)}
	}
	if raw.Alerts.StaleDays != nil {
		c.StaleDays = *raw.Alerts.StaleDays
	}
	for _, s := range raw.Source {
		if s.Name == "" {
			continue
		}
		if s.Type != "proxmox" {
			c.Files = append(c.Files, s.Name)
			continue
		}
		keep := 2
		if s.KeepLast != nil {
			keep = *s.KeepLast
		}
		gs := GuestSource{Name: s.Name}
		for _, g := range s.Guest {
			k := keep
			if g.KeepLast != nil {
				k = *g.KeepLast
			}
			gs.Guests = append(gs.Guests, Guest{VMID: g.VMID, Alias: g.Alias, KeepLast: k})
		}
		c.Proxmox = append(c.Proxmox, gs)
	}
	for k, v := range raw.Schedule {
		cr, err := ParseCron(v)
		if err != nil {
			return Config{}, &ConfigError{fmt.Sprintf("the config %s, [schedule] %s: %v", path, k, err)}
		}
		c.Schedule[k] = cr
	}
	return c, nil
}

// Repository is the repository of a source: base/name.
func (c Config) Repository(source string) string { return c.Base + "/" + source }

// Remote is the rclone remote of the repository base ("rclone:drive:backups" gives
// "drive"), or "" when the base is not an rclone one.
func (c Config) Remote() string { return Remote(c.Base) }

// Remote is the rclone remote of a repository base, or "".
func Remote(base string) string {
	rest, ok := strings.CutPrefix(base, "rclone:")
	if !ok {
		return ""
	}
	remote, _, _ := strings.Cut(rest, ":")
	return remote
}

// Interval is the time between two runs of a profile: the gap between its next two fire
// times, or stale_days when the schedule does not name the profile.
func (c Config) Interval(profile string, now time.Time) time.Duration {
	if cr, ok := c.Schedule[profile]; ok {
		if d := cr.Interval(now); d > 0 {
			return d
		}
	}
	return time.Duration(c.StaleDays) * 24 * time.Hour
}

// KeepWords says the keep rules of the local sources: "8 weekly, 6 monthly, 2 yearly".
func (r Retention) KeepWords() string {
	var p []string
	for _, x := range []struct {
		n    int
		word string
	}{{r.Weekly, "weekly"}, {r.Monthly, "monthly"}, {r.Yearly, "yearly"}} {
		if x.n > 0 {
			p = append(p, fmt.Sprintf("%d %s", x.n, x.word))
		}
	}
	if len(p) == 0 {
		return "no keep rule set"
	}
	return strings.Join(p, ", ")
}

// plainErr is the reason of a file error without the path, which the message names already.
func plainErr(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
