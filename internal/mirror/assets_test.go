package mirror

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedMirrorAssets(t *testing.T) {
	t.Parallel()

	wantFiles := []string{
		"assets/third_party/xterm/xterm.mjs",
		"assets/third_party/xterm/xterm.css",
		"assets/third_party/xterm/addon-fit.mjs",
		"assets/wrap-mirror-bootstrap.js",
		"assets/licenses/xterm-LICENSE",
		"assets/PROVENANCE.md",
	}
	for _, name := range wantFiles {
		for _, component := range strings.Split(name, "/") {
			if component == "vendor" {
				t.Fatalf("required runtime asset uses module-omitted vendor directory: %s", name)
			}
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("embedded %s is empty", name)
		}
	}
	wantHashes := map[string]string{
		"assets/third_party/xterm/xterm.mjs":     "3fd3d0046d2604ea5860235e2f96625a2fab158dcf3d4cfb0f7c1655559a5d9a",
		"assets/third_party/xterm/xterm.css":     "854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6",
		"assets/third_party/xterm/addon-fit.mjs": "aa22c5f28e4d64118ac0e7d60276b3384188e59dd104c96e43760d6e2cedd771",
		"assets/licenses/xterm-LICENSE":          "b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd",
	}
	for name, want := range wantHashes {
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Errorf("%s hash %s, want %s", name, got, want)
		}
	}

	provenance, err := fs.ReadFile(assets, "assets/PROVENANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"@xterm/xterm 6.0.0",
		"@xterm/addon-fit 0.11.0",
		"https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz",
		"https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz",
		"https://github.com/xtermjs/xterm.js",
		"MIT",
		"SHA-256",
	} {
		if !strings.Contains(string(provenance), want) {
			t.Errorf("provenance missing %q", want)
		}
	}

	externalLoad := regexp.MustCompile(`(?i)(?:from\s*|import\s*\(|url\s*\()\s*["']?https?://`)
	for _, name := range wantFiles[:3] {
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		if externalLoad.MatchString(source) {
			t.Errorf("%s contains an external runtime load", name)
		}
		if strings.Contains(source, "sourceMappingURL") {
			t.Errorf("%s references an unvendored source map", name)
		}
	}
}
