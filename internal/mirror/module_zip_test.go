package mirror

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

func TestModuleZipContainsMirrorRuntimeAssets(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate module zip test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	version := module.Version{Path: "github.com/sarcasticbird/wrap", Version: "v0.0.0"}

	archivePath := filepath.Join(t.TempDir(), "wrap.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := modzip.CreateFromDir(archive, version, repositoryRoot); err != nil {
		_ = archive.Close()
		t.Fatalf("create module zip: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close module zip: %v", err)
	}

	extractedRoot := filepath.Join(t.TempDir(), "module")
	if err := modzip.Unzip(extractedRoot, version, archivePath); err != nil {
		t.Fatalf("extract module zip: %v", err)
	}
	for _, name := range []string{
		"internal/mirror/assets/third_party/xterm/xterm.mjs",
		"internal/mirror/assets/third_party/xterm/xterm.css",
		"internal/mirror/assets/third_party/xterm/addon-fit.mjs",
	} {
		if _, err := os.Stat(filepath.Join(extractedRoot, name)); err != nil {
			t.Errorf("published module is missing %s: %v", name, err)
		}
	}

	command := exec.CommandContext(
		t.Context(),
		"go", "test", "./internal/mirror",
		"-run", "^TestEmbeddedMirrorAssets$",
		"-count=1",
	)
	command.Dir = extractedRoot
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("test extracted module assets: %v\n%s", err, output)
	}
}
