package mirror

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedAssetProvenance(t *testing.T) {
	t.Parallel()

	wantFiles := []string{
		"assets/vendor/xterm/xterm.mjs",
		"assets/vendor/xterm/xterm.css",
		"assets/vendor/xterm/addon-fit.mjs",
		"assets/licenses/xterm-LICENSE",
		"assets/PROVENANCE.md",
	}
	for _, name := range wantFiles {
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("embedded %s is empty", name)
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
