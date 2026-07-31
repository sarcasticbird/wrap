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
	if got := hex.EncodeToString(c2s[:]); got != "5d5ad6f9ba296141fa55a0d9fc730036700a9d59e0e7244b08b763b4380ac1d8" {
		t.Errorf("c2s key = %s", got)
	}
	if got := hex.EncodeToString(s2c[:]); got != "7d0a505e5c8410cd4e19fa87368ef82168f2533c4757b57c56a1d6ae5b3b3120" {
		t.Errorf("s2c key = %s", got)
	}
}

func TestCipherMatchesBrowserVectorAndRejectsReplay(t *testing.T) {
	keyBytes := mustHex(t, "5d5ad6f9ba296141fa55a0d9fc730036700a9d59e0e7244b08b763b4380ac1d8")
	var key [32]byte
	copy(key[:], keyBytes)
	plain := mustHex(t, "017b2276657273696f6e223a317d")

	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal(plain[0], plain[1:])
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ciphertext); got != "b93c018983645fd740b104f3de1f1f2b17c8b24b171062cc1c7845c4f47c" {
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
	copy(c2s[:], mustHex(t, "5d5ad6f9ba296141fa55a0d9fc730036700a9d59e0e7244b08b763b4380ac1d8"))
	copy(s2c[:], mustHex(t, "7d0a505e5c8410cd4e19fa87368ef82168f2533c4757b57c56a1d6ae5b3b3120"))

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
