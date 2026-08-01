package mirror

import (
	"errors"
	"os"
)

type portableDiagnosticFileOps struct {
	lstat    func(string) (os.FileInfo, error)
	openFile func(string, int, os.FileMode) (*os.File, error)
}

func openDiagnosticLogPortable(path string, ops portableDiagnosticFileOps) (*os.File, error) {
	for range 8 {
		info, err := ops.lstat(path)
		switch {
		case os.IsNotExist(err):
			file, openErr := ops.openFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
			if os.IsExist(openErr) {
				continue
			}
			return validatePortableDiagnosticFile(file, nil, openErr)
		case err != nil:
			return nil, err
		case !info.Mode().IsRegular():
			return nil, errors.New("mirror diagnostic log is not a regular file")
		}

		file, openErr := ops.openFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if os.IsNotExist(openErr) {
			continue
		}
		return validatePortableDiagnosticFile(file, info, openErr)
	}
	return nil, errors.New("mirror diagnostic log changed while opening")
}

func validatePortableDiagnosticFile(file *os.File, expected os.FileInfo, err error) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || expected != nil && !os.SameFile(expected, opened) {
		_ = file.Close()
		return nil, errors.New("mirror diagnostic log changed or is not a regular file")
	}
	return file, nil
}
