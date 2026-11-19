package main

import (
	"os"
	"strings"

	"github.com/norandom/restic-pve/internal/stream/cli"
)

// cmdStream is `restic-pve stream <backup|verify|to-vma|to-raw>`: the former vmastream with its
// contract unchanged (flags, stdout bytes, stderr JSON lines, result line, exit codes 0 and
// 2-10). restic starts it with its own environment, which holds the repository password and
// often more secrets; the stream needs none of them, so they leave this process's environment
// before ssh (or anything else) is started from it.
func cmdStream(args []string, e env) int {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); secretName(k) {
			os.Unsetenv(k)
		}
	}
	return cli.Main(args, e.stdin, e.stdout, e.stderr)
}

// secretName reports whether an environment variable holds, or points to, a secret: a
// password, passphrase, token, key, webhook or credentials.
func secretName(k string) bool {
	k = strings.ToUpper(k)
	for _, s := range []string{"PASSWORD", "PASSPHRASE", "SECRET", "TOKEN", "WEBHOOK", "CREDENTIAL"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_PASS")
}
