// Command relsign creates the release signing key and signs and checks release files
// (Ed25519, base64). The private key never appears in its output.
//
//	relsign genkey <private-file> <public-file>
//	relsign pubkey              (private key from RELEASE_SIGNING_KEY)
//	relsign sign <file>         (writes <file>.sig; key from RELEASE_SIGNING_KEY)
//	relsign verify <file>       (checks <file>.sig against the embedded keys)
//	relsign embedded <pubkey>   (exit 0 when the key is embedded)
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/norandom/restic-pve/internal/relkeys"
)

const usage = "usage: relsign genkey <private-file> <public-file> | pubkey | sign <file> | verify <file> | embedded <base64-pubkey>"

func main() {
	os.Exit(run(os.Args[1:], relkeys.PublicKeys, os.Getenv, os.Stdout, os.Stderr))
}

func run(args, keys []string, getenv func(string) string, stdout, stderr io.Writer) int {
	n := map[string]int{"genkey": 3, "pubkey": 1, "sign": 2, "verify": 2, "embedded": 2}
	if len(args) == 0 || n[args[0]] != len(args) {
		fmt.Fprintln(stderr, usage)
		return 64
	}
	var err error
	switch args[0] {
	case "genkey":
		err = genkey(args[1], args[2])
	case "pubkey":
		var priv ed25519.PrivateKey
		if priv, err = envKey(getenv); err == nil {
			fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
		}
	case "sign":
		err = sign(args[1], getenv)
	case "verify":
		err = verify(args[1], keys)
		if err == nil {
			fmt.Fprintf(stdout, "%s: signature ok\n", args[1])
		}
	case "embedded":
		if !slices.Contains(keys, args[1]) {
			err = errors.New("public key is not embedded in relkeys.PublicKeys")
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "relsign %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func genkey(privPath, pubPath string) error {
	for _, p := range []string{privPath, pubPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s exists, not overwritten", p)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := create(privPath, 0o600, priv); err != nil {
		return err
	}
	if err := create(pubPath, 0o644, pub); err != nil {
		_ = os.Remove(privPath)
		return err
	}
	return nil
}

// create writes base64 of b plus a newline to a new file; an existing file is an error.
func create(path string, mode os.FileMode, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(b) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func envKey(getenv func(string) string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace([]byte(getenv("RELEASE_SIGNING_KEY")))))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("RELEASE_SIGNING_KEY is not base64 of a 64-byte Ed25519 private key")
	}
	priv := ed25519.PrivateKey(b)
	if !bytes.Equal(ed25519.NewKeyFromSeed(priv.Seed()), priv) {
		return nil, errors.New("RELEASE_SIGNING_KEY is inconsistent (public half does not match)")
	}
	return priv, nil
}

func sign(path string, getenv func(string) string) error {
	priv, err := envKey(getenv)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n"
	return os.WriteFile(path+".sig", []byte(sig), 0o644)
}

func verify(path string, keys []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		return err
	}
	return relkeys.Verify(keys, data, sig)
}
