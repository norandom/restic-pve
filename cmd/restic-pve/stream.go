package main

import (
	"os"
	"strings"

	"github.com/norandom/restic-pve/internal/stream/cli"
	"github.com/norandom/restic-pve/internal/stream/source"
)

// cmdStream is `restic-pve stream <backup|verify|to-vma|to-raw>`: the former vmastream with its
// contract unchanged (flags, stdout bytes, stderr JSON lines, result line, exit codes 0 and
// 2-10). restic starts it with its own environment, which holds the repository password and
// often more secrets; the stream needs none of them, so they leave this process's environment
// before ssh (or anything else) is started from it.
func cmdStream(args []string, e env) int {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); source.SecretName(k) {
			os.Unsetenv(k)
		}
	}
	return cli.Main(args, e.stdin, e.stdout, e.stderr)
}
