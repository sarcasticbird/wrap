//go:build darwin || linux

package mirror

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openDiagnosticLog(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create mirror diagnostic file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("mirror diagnostic log is not a regular file")
	}
	return file, nil
}
