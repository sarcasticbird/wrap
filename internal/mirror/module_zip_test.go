package mirror

import (
	"bytes"
	"errors"
	"io/fs"
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
	repositoryRoot = moduleSourceTree(t, repositoryRoot)
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
		"internal/mirror/assets/wrap-mirror-viewport.js",
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

func moduleSourceTree(t *testing.T, repositoryRoot string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".git")); err != nil {
		return repositoryRoot
	}
	command := exec.CommandContext(
		t.Context(), "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard",
	)
	command.Dir = repositoryRoot
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list publishable module files: %v", err)
	}
	cleanRoot := filepath.Join(t.TempDir(), "source")
	for _, rawName := range bytes.Split(output, []byte{0}) {
		if len(rawName) == 0 {
			continue
		}
		name := string(rawName)
		source := filepath.Join(repositoryRoot, name)
		info, err := os.Lstat(source)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("inspect module file %s: %v", name, err)
		}
		destination := filepath.Join(cleanRoot, name)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, destination); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	return cleanRoot
}
