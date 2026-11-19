#!/bin/sh
# Fake ssh for tests (copied to <tmp>/ssh and put first on PATH).
# Records its argv, writes FAKE_SSH_LOG to stderr and FAKE_SSH_STDOUT to
# stdout, optionally sleeps (exec, so a kill reaches it), then exits with
# FAKE_SSH_EXIT.
if [ -n "$FAKE_SSH_ARGS" ]; then printf '%s\n' "$@" > "$FAKE_SSH_ARGS"; fi
if [ -n "$FAKE_SSH_LOG" ]; then cat "$FAKE_SSH_LOG" >&2; fi
if [ -n "$FAKE_SSH_STDOUT" ]; then cat "$FAKE_SSH_STDOUT"; fi
if [ -n "$FAKE_SSH_SLEEP" ]; then exec sleep "$FAKE_SSH_SLEEP"; fi
exit "${FAKE_SSH_EXIT:-0}"
