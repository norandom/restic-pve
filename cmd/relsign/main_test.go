package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type out struct {
	code           int
	stdout, stderr string
}

func call(keys []string, env map[string]string, args ...string) out {
	var o, e bytes.Buffer
	code := run(args, keys, func(k string) string { return env[k] }, &o, &e)
	return out{code, o.String(), e.String()}
}

func TestGenkeySignVerify(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "key"), filepath.Join(dir, "key.pub")
	if r := call(nil, nil, "genkey", priv, pub); r.code != 0 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	st, _ := os.Stat(priv)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", st.Mode())
	}
	privB, _ := os.ReadFile(priv)
	pubB, _ := os.ReadFile(pub)
	if b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(string(privB), "\n")); err != nil || len(b) != 64 {
		t.Fatalf("private file %v", err)
	}
	pubKey := strings.TrimSuffix(string(pubB), "\n")
	if b, err := base64.StdEncoding.DecodeString(pubKey); err != nil || len(b) != 32 || !strings.HasSuffix(string(pubB), "\n") {
		t.Fatalf("public file %v", err)
	}
	if r := call(nil, nil, "genkey", priv, filepath.Join(dir, "other.pub")); r.code != 1 {
		t.Fatalf("overwrote: %+v", r)
	}
	if after, _ := os.ReadFile(priv); !bytes.Equal(after, privB) {
		t.Fatal("private key changed")
	}

	env := map[string]string{"RELEASE_SIGNING_KEY": string(privB)}
	if r := call(nil, env, "pubkey"); r.code != 0 || r.stdout != pubKey+"\n" {
		t.Fatalf("%+v", r)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte("0000  VERSION\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := call(nil, env, "sign", sums)
	if r.code != 0 || strings.Contains(r.stdout+r.stderr, strings.TrimSpace(string(privB))) {
		t.Fatalf("%+v", r)
	}
	if r := call([]string{pubKey}, nil, "verify", sums); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := call(nil, nil, "verify", sums); r.code != 1 || !strings.Contains(r.stderr, "no release keys") {
		t.Fatalf("empty key list: %+v", r)
	}
	_ = os.WriteFile(sums, []byte("0001  VERSION\n"), 0o644)
	if r := call([]string{pubKey}, nil, "verify", sums); r.code != 1 {
		t.Fatalf("tampered file verified: %+v", r)
	}
	if r := call([]string{pubKey}, nil, "embedded", pubKey); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := call([]string{pubKey}, nil, "embedded", "AAAA"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestBadInput(t *testing.T) {
	for _, args := range [][]string{nil, {"genkey", "a"}, {"sign"}, {"nope"}} {
		if r := call(nil, nil, args...); r.code != 64 {
			t.Errorf("%q: %+v", args, r)
		}
	}
	for _, key := range []string{"", "!!", base64.StdEncoding.EncodeToString(make([]byte, 32)),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64))} {
		if r := call(nil, map[string]string{"RELEASE_SIGNING_KEY": key}, "pubkey"); r.code != 1 || r.stdout != "" {
			t.Errorf("%q: %+v", key, r)
		}
	}
}
