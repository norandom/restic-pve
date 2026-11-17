// Package relkeys holds the release signing public keys compiled into the binary and
// verifies release signatures against them.
package relkeys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// PublicKeys are the accepted release keys: base64 (std encoding) of 32-byte Ed25519
// public keys. Add a key as one line between the markers (docs/release-signing.md).
var PublicKeys = []string{
	// BEGIN KEYS
	"GZbiCBDmasmhnbWJhq5Oev6UEBwOZZSz6T6LmdqANJI=",
	// END KEYS
}

// ErrNoKeys means the build contains no release key, so nothing can be verified.
var ErrNoKeys = errors.New("no release keys in this build")

// ErrBadSignature means no listed key made the signature.
var ErrBadSignature = errors.New("signature does not match any release key")

// Verify accepts sigBase64 (base64 of a 64-byte Ed25519 signature, trailing newline
// allowed) when any key in keys signed message.
func Verify(keys []string, message, sigBase64 []byte) error {
	if len(keys) == 0 {
		return ErrNoKeys
	}
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sigBase64)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is not base64 of %d bytes", ed25519.SignatureSize)
	}
	for _, k := range keys {
		pub, err := DecodePublic(k)
		if err != nil {
			return err
		}
		if ed25519.Verify(pub, message, sig) {
			return nil
		}
	}
	return ErrBadSignature
}

// DecodePublic decodes one base64 public key.
func DecodePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("release key %q is not base64 of %d bytes", s, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
