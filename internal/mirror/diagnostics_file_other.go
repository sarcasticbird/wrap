//go:build !darwin && !linux

package mirror

import (
	"os"
)

func openDiagnosticLog(path string) (*os.File, error) {
	return openDiagnosticLogPortable(path, portableDiagnosticFileOps{
		lstat: os.Lstat, openFile: os.OpenFile,
	})
}
