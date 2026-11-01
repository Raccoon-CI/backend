package vault

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealedValuesOpenOnlyWhereTheyBelong(t *testing.T) {
	v, err := New(map[string][]byte{"1": key(1)}, "1")
	if err != nil {
		t.Fatal(err)
	}
	id, sealed, err := v.Seal("repo:acme/web", "TOKEN", []byte("s3cret"))
	if err != nil || id != "1" {
		t.Fatalf("seal: %q, %v", id, err)
	}
	if bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("the plaintext is in the sealed value")
	}
	got, err := v.Open("repo:acme/web", "TOKEN", id, sealed)
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("open: %q, %v", got, err)
	}
	// Copied to another name or scope, it does not open.
	for _, c := range [][2]string{{"repo:acme/web", "OTHER"}, {"repo:acme/api", "TOKEN"}, {"org:acme", "TOKEN"}} {
		if _, err := v.Open(c[0], c[1], id, sealed); !errors.Is(err, ErrCannotOpen) {
			t.Errorf("opened at %v: %v", c, err)
		}
	}
	// Damaged, it does not open.
	sealed[len(sealed)-1] ^= 1
	if _, err := v.Open("repo:acme/web", "TOKEN", id, sealed); !errors.Is(err, ErrCannotOpen) {
		t.Errorf("damaged value opened: %v", err)
	}
	if _, err := v.Open("repo:acme/web", "TOKEN", id, []byte{1, 2}); !errors.Is(err, ErrCannotOpen) {
		t.Errorf("a short value: %v", err)
	}
}

func TestTwoSealsOfOneValueDiffer(t *testing.T) {
	v, _ := New(map[string][]byte{"1": key(1)}, "1")
	_, a, _ := v.Seal("s", "n", []byte("same"))
	_, b, _ := v.Seal("s", "n", []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("the nonce is reused")
	}
}

func TestRotationOpensOldValuesAndSealsWithTheCurrentKey(t *testing.T) {
	old, _ := New(map[string][]byte{"1": key(1)}, "1")
	id, sealed, _ := old.Seal("s", "n", []byte("v"))
	rotated, err := New(map[string][]byte{"1": key(1), "2": key(2)}, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open("s", "n", id, sealed); err != nil || string(got) != "v" {
		t.Fatalf("an old value after rotation: %q, %v", got, err)
	}
	if newID, _, _ := rotated.Seal("s", "n", []byte("v")); newID != "2" || rotated.Current() != "2" {
		t.Fatalf("sealed with key %q", newID)
	}
	onlyNew, _ := New(map[string][]byte{"2": key(2)}, "2")
	if _, err := onlyNew.Open("s", "n", id, sealed); err == nil || strings.Contains(err.Error(), string(key(1))) {
		t.Fatalf("a value of a dropped key: %v", err)
	}
	wrongKey, _ := New(map[string][]byte{"1": key(9)}, "1")
	if _, err := wrongKey.Open("s", "n", id, sealed); !errors.Is(err, ErrCannotOpen) {
		t.Fatalf("a wrong key opened it: %v", err)
	}
}

func TestBadKeysAreRefused(t *testing.T) {
	if _, err := New(map[string][]byte{"1": []byte("short")}, "1"); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := New(map[string][]byte{"1": key(1)}, "2"); err == nil {
		t.Error("a current key that is not there was accepted")
	}
	if _, err := New(map[string][]byte{"a b": key(1)}, "a b"); err == nil {
		t.Error("a key id with a space was accepted")
	}
}

func TestParseKey(t *testing.T) {
	raw := key(7)
	for name, s := range map[string]string{
		"hex": hex.EncodeToString(raw), "base64": base64.StdEncoding.EncodeToString(raw),
		"raw base64": base64.RawStdEncoding.EncodeToString(raw), "padded with a newline": " " + hex.EncodeToString(raw) + "\n",
	} {
		got, err := ParseKey(s)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, s := range []string{"", "abc", hex.EncodeToString(raw[:16]), strings.Repeat("z", 64)} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}
