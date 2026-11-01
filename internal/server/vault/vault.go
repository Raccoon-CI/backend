// Package vault seals secrets for the database: AES-256-GCM under a master key
// the server is given (never stored with the data), bound to the secret's scope
// and name, so a sealed value copied to another row does not open there.
//
// A value is sealed with the current key, and says which key id did it; keys
// from before can stay in the set to open old values while new ones are
// written with the current key (rotation). Nothing here logs or returns a key.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize is the size of a master key, in bytes.
const KeySize = 32

// Vault seals and opens values.
type Vault struct {
	keys    map[string]cipher.AEAD
	current string
}

// New makes a vault from keys (id to key) and the id to seal with.
func New(keys map[string][]byte, current string) (*Vault, error) {
	v := &Vault{keys: map[string]cipher.AEAD{}, current: current}
	for id, k := range keys {
		if len(k) != KeySize {
			return nil, fmt.Errorf("vault: key %q has %d bytes, want %d", id, len(k), KeySize)
		}
		if id == "" || strings.ContainsAny(id, " \t\n/") {
			return nil, fmt.Errorf("vault: bad key id %q", id)
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		v.keys[id] = gcm
	}
	if _, ok := v.keys[current]; !ok {
		return nil, fmt.Errorf("vault: the key to seal with (%q) is not among the keys", current)
	}
	return v, nil
}

// ParseKey reads a master key written as 64 hex digits or as base64 (what
// `openssl rand -base64 32` or `-hex 32` prints).
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == KeySize {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, errors.New("the master key must be 32 bytes, as 64 hex digits or base64")
}

// aad binds a sealed value to the row it belongs to.
func aad(scope, name string) []byte { return []byte(scope + "\x00" + name) }

// Seal encrypts plaintext for the secret (scope, name) and returns the id of
// the key used and the sealed bytes (nonce first).
func (v *Vault) Seal(scope, name string, plaintext []byte) (keyID string, sealed []byte, err error) {
	gcm := v.keys[v.current]
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", nil, err
	}
	return v.current, gcm.Seal(nonce, nonce, plaintext, aad(scope, name)), nil
}

// ErrCannotOpen is returned for a value that does not open: wrong key, wrong
// row, or damage. It says nothing more, on purpose.
var ErrCannotOpen = errors.New("vault: the value cannot be opened")

// Open decrypts a value sealed for (scope, name) with the key keyID.
func (v *Vault) Open(scope, name, keyID string, sealed []byte) ([]byte, error) {
	gcm, ok := v.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("vault: the key %q that sealed this value is not available", keyID)
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, ErrCannotOpen
	}
	pt, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad(scope, name))
	if err != nil {
		return nil, ErrCannotOpen
	}
	return pt, nil
}

// Current is the id new values are sealed with.
func (v *Vault) Current() string { return v.current }
