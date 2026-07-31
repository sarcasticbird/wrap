package mirror

import (
	"bytes"
	"strings"
	"testing"
)

func TestSecretGenerationEncodingAndParsing(t *testing.T) {
	source := make([]byte, 32)
	for i := range source {
		source[i] = byte(i)
	}
	secret, err := NewSecret(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if got := secret.String(); got != "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" {
		t.Fatalf("secret = %q", got)
	}
	parsed, err := ParseSecret(secret.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != secret {
		t.Fatal("parsed secret changed")
	}
	if _, err := ParseSecret(secret.String() + "="); err == nil {
		t.Fatal("accepted padded secret")
	}
}

func TestPairingURLKeepsSecretInFragment(t *testing.T) {
	var secret Secret
	for i := range secret {
		secret[i] = byte(i)
	}
	got, err := PairingURL("https://quiet-river.trycloudflare.com", secret)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://quiet-river.trycloudflare.com/#k=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	if got != want {
		t.Fatalf("PairingURL = %q, want %q", got, want)
	}
	for _, invalid := range []string{
		"http://quiet-river.trycloudflare.com",
		"https://trycloudflare.com",
		"https://quiet-river.trycloudflare.com.evil.test",
		"https://user@quiet-river.trycloudflare.com",
		"https://quiet-river.trycloudflare.com:443",
		"https://quiet-river.trycloudflare.com/path",
		"https://quiet-river.trycloudflare.com/?query=yes",
	} {
		if _, err := PairingURL(invalid, secret); err == nil {
			t.Errorf("accepted invalid public URL %q", invalid)
		}
	}
}

func TestTerminalQRIsDeterministicAndHasQuietZone(t *testing.T) {
	const pairing = "https://quiet-river.trycloudflare.com/#k=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	first, err := TerminalQR(pairing)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TerminalQR(pairing)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatal("terminal QR is empty or nondeterministic")
	}
	lines := strings.Split(strings.TrimSuffix(first, "\n"), "\n")
	if len(lines) < 10 {
		t.Fatalf("QR has only %d rows", len(lines))
	}
	if strings.TrimSpace(lines[0]) != "" || strings.TrimSpace(lines[len(lines)-1]) != "" {
		t.Fatalf("QR is missing its vertical quiet zone: first=%q last=%q", lines[0], lines[len(lines)-1])
	}
}
