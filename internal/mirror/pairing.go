package mirror

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"

	qrcode "github.com/skip2/go-qrcode"
)

var quickTunnelHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.trycloudflare\.com$`)

type Secret [32]byte

func NewSecret(random io.Reader) (Secret, error) {
	var secret Secret
	if random == nil {
		return secret, errors.New("random source is required")
	}
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return Secret{}, fmt.Errorf("generate pairing secret: %w", err)
	}
	return secret, nil
}

func ParseSecret(raw string) (Secret, error) {
	var secret Secret
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return secret, errors.New("invalid pairing secret encoding")
	}
	if len(decoded) != len(secret) {
		return secret, errors.New("pairing secret must decode to 32 bytes")
	}
	copy(secret[:], decoded)
	return secret, nil
}

func (s Secret) String() string {
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func PairingURL(publicURL string, secret Secret) (string, error) {
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return "", fmt.Errorf("parse tunnel URL: %w", err)
	}
	host := parsed.Hostname()
	if parsed.Scheme != "https" ||
		!quickTunnelHost.MatchString(host) ||
		parsed.Host != host ||
		parsed.User != nil ||
		parsed.Path != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", errors.New("tunnel URL must be an HTTPS trycloudflare origin")
	}
	if net.ParseIP(host) != nil {
		return "", errors.New("tunnel URL host must be a DNS name")
	}
	parsed.Path = "/"
	parsed.Fragment = "k=" + secret.String()
	return parsed.String(), nil
}

func TerminalQR(pairingURL string) (string, error) {
	code, err := qrcode.New(pairingURL, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("encode pairing QR: %w", err)
	}
	return code.ToSmallString(true), nil
}
