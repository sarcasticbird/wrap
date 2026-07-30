package mirror

import (
	"io/fs"
	"strings"
	"testing"
)

func TestBrowserContractUsesFragmentScopedWebCryptoAndLocalAssets(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`from "/assets/vendor/xterm/xterm.mjs"`,
		`from "/assets/vendor/xterm/addon-fit.mjs"`,
		`new URLSearchParams(location.hash.slice(1))`,
		`history.replaceState(null, "", location.pathname + location.search)`,
		`sessionStorage`,
		`crypto.subtle`,
		`HKDF`,
		`SHA-256`,
		`AES-GCM`,
		`wrap-mirror/v1/c2s`,
		`wrap-mirror/v1/s2c`,
		`binaryType = "arraybuffer"`,
		`await cryptoSelfTest()`,
		`new WebSocket`,
		`setBigUint64(4, counter, false)`,
		`Math.min(5000`,
		`event.code === 1008`,
		`sessionStorage.removeItem(STORAGE_KEY)`,
		`showMessage("Pairing rejected"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing %q", want)
		}
	}
	if strings.Index(source, "await cryptoSelfTest()") > strings.Index(source, "new WebSocket") {
		t.Fatal("browser client constructs WebSocket before its crypto self-test")
	}
	for _, forbidden := range []string{
		"local" + "Storage",
		"document." + "cookie",
		"navigator.send" + "Beacon",
		"service" + "Worker",
		"eval(",
		"new Function",
		"http://",
		"https://",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("browser client contains forbidden capability %q", forbidden)
		}
	}
}

func TestBrowserContractIncludesTerminalAndMobileControls(t *testing.T) {
	htmlBytes, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, control := range []string{
		`data-key="enter"`,
		`data-key="escape"`,
		`data-key="tab"`,
		`data-key="shift-tab"`,
		`data-key="control"`,
		`data-key="up"`,
		`data-key="down"`,
		`data-key="left"`,
		`data-key="right"`,
		`data-key="ctrl-c"`,
		`data-key="ctrl-d"`,
		`data-key="ctrl-l"`,
		`data-key="ctrl-z"`,
	} {
		if !strings.Contains(html, control) {
			t.Errorf("browser HTML missing %s", control)
		}
	}
}

func TestBrowserContractHandlesCloseRevocationAndLargeInputWithoutPoisoning(t *testing.T) {
	sourceBytes, err := fs.ReadFile(assets, "assets/wrap-mirror.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`const MAX_FRAME_PAYLOAD = MAX_WIRE_MESSAGE - 17`,
		`let closing = null`,
		`if (closing)`,
		`closing = null`,
		`case TAG.close:`,
		`function sendInput(data)`,
		`payload.subarray(offset, offset + MAX_FRAME_PAYLOAD)`,
		`const isCurrent = current?.id === revoked.id`,
		`"Incompatible browser"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client missing close/input guard %q", want)
		}
	}
	for _, want := range []string{
		`const closingStillMirrored = sessions.some(`,
		`if (!closingStillMirrored) {`,
		`const isClosing = closing?.id === revoked.id`,
		`if (isCurrent || isClosing) {`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("browser client does not resolve server-side close race %q", want)
		}
	}
}
