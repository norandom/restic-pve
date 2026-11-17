package relkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func key(t *testing.T) (string, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func TestVerify(t *testing.T) {
	pubA, privA := key(t)
	pubB, privB := key(t)
	_, privC := key(t)
	msg := []byte("abc  restic-pve-linux-amd64\n")
	sig := func(p ed25519.PrivateKey) []byte {
		return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(p, msg)) + "\n")
	}
	keys := []string{pubA, pubB}
	if err := Verify(keys, msg, sig(privA)); err != nil {
		t.Fatal(err)
	}
	if err := Verify(keys, msg, sig(privB)); err != nil {
		t.Fatal("second key:", err)
	}
	if err := Verify(keys, msg, sig(privC)); !errors.Is(err, ErrBadSignature) {
		t.Fatal("unknown key:", err)
	}
	if err := Verify(keys, append(msg, 'x'), sig(privA)); !errors.Is(err, ErrBadSignature) {
		t.Fatal("tampered message:", err)
	}
	if err := Verify(nil, msg, sig(privA)); !errors.Is(err, ErrNoKeys) {
		t.Fatal("empty list:", err)
	}
	if err := Verify(keys, msg, []byte("not base64!")); err == nil {
		t.Fatal("garbage signature accepted")
	}
	if err := Verify([]string{"short"}, msg, sig(privA)); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestEmbeddedKeysDecode(t *testing.T) {
	for _, k := range PublicKeys {
		if _, err := DecodePublic(k); err != nil {
			t.Error(err)
		}
	}
}
