package mirror

import (
	"encoding/hex"
	"errors"
	"testing"
)

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestDeriveKeysMatchesBrowserVector(t *testing.T) {
	secret := mustHex(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	serverNonce := mustHex(t, "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf")
	clientNonce := mustHex(t, "b0b1b2b3b4b5b6b7b8b9babbbcbdbebf")

	c2s, s2c, err := DeriveKeys(secret, serverNonce, clientNonce)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(c2s[:]); got != "48dc51994ede7a7fd3b04b66ba2103331f812109cc821e56e34a0f2b3860a315" {
		t.Errorf("c2s key = %s", got)
	}
	if got := hex.EncodeToString(s2c[:]); got != "a7574bd01ad71ecf2d2339aa9a417029f860d957836d8883a2307599d9f51161" {
		t.Errorf("s2c key = %s", got)
	}
}

func TestCipherMatchesBrowserVectorAndRejectsReplay(t *testing.T) {
	keyBytes := mustHex(t, "48dc51994ede7a7fd3b04b66ba2103331f812109cc821e56e34a0f2b3860a315")
	var key [32]byte
	copy(key[:], keyBytes)
	plain := mustHex(t, "017b2276657273696f6e223a327d")

	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal(plain[0], plain[1:])
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ciphertext); got != "49ac75e8504742f9e9aef44d69fefb855a3694abe34934356b6b0b8d8ffb" {
		t.Fatalf("ciphertext = %s", got)
	}

	opener, err := NewOpener(key)
	if err != nil {
		t.Fatal(err)
	}
	tag, payload, err := opener.Open(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if tag != TagClientHello || hex.EncodeToString(append([]byte{tag}, payload...)) != hex.EncodeToString(plain) {
		t.Fatalf("opened tag/payload = %x/%x", tag, payload)
	}
	if _, _, err := opener.Open(ciphertext); err == nil {
		t.Fatal("replayed counter-zero ciphertext was accepted")
	}
	if _, _, err := opener.Open(ciphertext); !errors.Is(err, ErrCipherPoisoned) {
		t.Fatalf("open after authentication failure = %v, want poisoned", err)
	}
}

func TestCipherRejectsTamperingAndWrongDirection(t *testing.T) {
	var c2s, s2c [32]byte
	copy(c2s[:], mustHex(t, "48dc51994ede7a7fd3b04b66ba2103331f812109cc821e56e34a0f2b3860a315"))
	copy(s2c[:], mustHex(t, "a7574bd01ad71ecf2d2339aa9a417029f860d957836d8883a2307599d9f51161"))

	sealer, _ := NewSealer(c2s)
	ciphertext, err := sealer.Seal(TagInput, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	opener, _ := NewOpener(c2s)
	if _, _, err := opener.Open(tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	wrong, _ := NewOpener(s2c)
	if _, _, err := wrong.Open(ciphertext); err == nil {
		t.Fatal("ciphertext opened with the opposite direction key")
	}
}
