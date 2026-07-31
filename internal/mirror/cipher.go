package mirror

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

var (
	ErrCipherPoisoned   = errors.New("mirror cipher is poisoned")
	ErrCounterExhausted = errors.New("mirror cipher counter exhausted")
)

func DeriveKeys(secret, serverNonce, clientNonce []byte) (c2s, s2c [32]byte, err error) {
	if len(secret) != 32 {
		return c2s, s2c, errors.New("pairing secret must be 32 bytes")
	}
	if len(serverNonce) != 16 || len(clientNonce) != 16 {
		return c2s, s2c, errors.New("handshake nonces must be 16 bytes")
	}
	salt := make([]byte, 0, 32)
	salt = append(salt, serverNonce...)
	salt = append(salt, clientNonce...)
	c2sBytes, err := hkdf.Key(sha256.New, secret, salt, "wrap-mirror/v1/c2s", len(c2s))
	if err != nil {
		return c2s, s2c, fmt.Errorf("derive client-to-server key: %w", err)
	}
	s2cBytes, err := hkdf.Key(sha256.New, secret, salt, "wrap-mirror/v1/s2c", len(s2c))
	if err != nil {
		return c2s, s2c, fmt.Errorf("derive server-to-client key: %w", err)
	}
	copy(c2s[:], c2sBytes)
	copy(s2c[:], s2cBytes)
	return c2s, s2c, nil
}

type Sealer struct {
	aead     cipher.AEAD
	counter  uint64
	poisoned bool
}

type Opener struct {
	aead     cipher.AEAD
	counter  uint64
	poisoned bool
}

func NewSealer(key [32]byte) (*Sealer, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func NewOpener(key [32]byte) (*Opener, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &Opener{aead: aead}, nil
}

func (s *Sealer) Seal(tag byte, payload []byte) ([]byte, error) {
	if s.poisoned {
		return nil, ErrCipherPoisoned
	}
	if s.counter == math.MaxUint64 {
		s.poisoned = true
		return nil, ErrCounterExhausted
	}
	if len(payload)+1+s.aead.Overhead() > MaxWireMessage {
		s.poisoned = true
		return nil, errors.New("plaintext exceeds wire limit")
	}
	plain := make([]byte, 1+len(payload))
	plain[0] = tag
	copy(plain[1:], payload)
	ciphertext := s.aead.Seal(nil, counterNonce(s.counter), plain, nil)
	s.counter++
	return ciphertext, nil
}

func (o *Opener) Open(ciphertext []byte) (byte, []byte, error) {
	if o.poisoned {
		return 0, nil, ErrCipherPoisoned
	}
	if o.counter == math.MaxUint64 {
		o.poisoned = true
		return 0, nil, ErrCounterExhausted
	}
	if len(ciphertext) > MaxWireMessage {
		o.poisoned = true
		return 0, nil, errors.New("ciphertext exceeds wire limit")
	}
	plain, err := o.aead.Open(nil, counterNonce(o.counter), ciphertext, nil)
	if err != nil {
		o.poisoned = true
		return 0, nil, fmt.Errorf("authenticate mirror frame: %w", err)
	}
	if len(plain) == 0 {
		o.poisoned = true
		return 0, nil, errors.New("mirror frame has no tag")
	}
	o.counter++
	return plain[0], plain[1:], nil
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return aead, nil
}

func counterNonce(counter uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}
