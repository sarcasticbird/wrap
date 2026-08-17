package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const connectionTimeout = 5 * time.Second

type server struct {
	ctx        context.Context
	instanceID string
	handler    Handler
	listener   *net.UnixListener
	opMu       sync.Mutex
	stopOnce   sync.Once
	stop       chan struct{}
	clients    sync.WaitGroup
}

func Serve(ctx context.Context, socketPath, instanceID string, handler Handler) (retErr error) {
	if ctx == nil {
		return errors.New("control context is required")
	}
	if handler == nil {
		return errors.New("control handler is required")
	}
	if !filepath.IsAbs(socketPath) {
		return errors.New("control socket path must be absolute")
	}
	if err := (Request{InstanceID: instanceID, Action: ActionStatus}).validate(); err != nil {
		return err
	}
	if err := prepareSocketDirectory(filepath.Dir(socketPath)); err != nil {
		return err
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return fmt.Errorf("control socket path already exists: %s", socketPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect control socket path: %w", err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = removeOwnedSocket(socketPath, nil)
		return fmt.Errorf("protect control socket: %w", err)
	}
	owned, err := os.Lstat(socketPath)
	if err != nil {
		return fmt.Errorf("inspect created control socket: %w", err)
	}
	defer func() {
		if err := removeOwnedSocket(socketPath, owned); err != nil && retErr == nil {
			retErr = err
		}
	}()

	s := &server{
		ctx:        ctx,
		instanceID: instanceID,
		handler:    handler,
		listener:   listener,
		stop:       make(chan struct{}),
	}
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	defer s.clients.Wait()

	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stop:
				return nil
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		s.clients.Add(1)
		go func() {
			defer s.clients.Done()
			s.handle(conn)
		}()
	}
}

func (s *server) handle(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(connectionTimeout))
	request, requestErr := readRequest(conn)
	if requestErr != nil {
		writeResponse(conn, response{Error: &Error{Code: CodeInvalidRequest, Message: requestErr.Error()}})
		return
	}
	if request.InstanceID != s.instanceID {
		writeResponse(conn, response{Error: &Error{Code: CodeNotFound, Message: "Wrap instance not found"}})
		return
	}

	switch request.Action {
	case ActionStatus:
		status := s.handler.Status()
		writeResponse(conn, response{Status: &status})
	case ActionRename:
		s.opMu.Lock()
		status, err := s.handler.Rename(s.ctx, request.Name)
		s.opMu.Unlock()
		if err != nil {
			writeResponse(conn, response{Error: responseError(err)})
			return
		}
		writeResponse(conn, response{Status: &status})
	case ActionRotate:
		s.opMu.Lock()
		status, err := s.handler.Rotate(s.ctx)
		s.opMu.Unlock()
		if err != nil {
			writeResponse(conn, response{Error: responseError(err)})
			return
		}
		writeResponse(conn, response{Status: &status})
	case ActionShutdown:
		s.opMu.Lock()
		err := s.handler.Shutdown(s.ctx)
		status := s.handler.Status()
		s.opMu.Unlock()
		if err != nil {
			writeResponse(conn, response{Error: responseError(err)})
			return
		}
		writeResponse(conn, response{Status: &status})
		s.stopOnce.Do(func() {
			close(s.stop)
			_ = s.listener.Close()
		})
	}
}

func readRequest(conn io.Reader) (Request, error) {
	reader := bufio.NewReader(io.LimitReader(conn, MaxRequestBytes+1))
	data, err := reader.ReadBytes('\n')
	if len(data) > MaxRequestBytes {
		return Request{}, errors.New("control request is too large")
	}
	if err != nil {
		return Request{}, errors.New("control request must end with a newline")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("invalid control request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("control request contains trailing JSON")
	}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func writeResponse(conn io.Writer, reply response) {
	_ = json.NewEncoder(conn).Encode(reply)
}

func responseError(err error) *Error {
	if errors.Is(err, ErrNotReady) {
		return &Error{Code: CodeNotReady, Message: ErrNotReady.Error()}
	}
	return &Error{Code: CodeInternal, Message: "Wrap operation failed"}
}

func prepareSocketDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create control socket directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect control socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("control socket directory is not a regular directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect control socket directory: %w", err)
	}
	return nil
}

func removeOwnedSocket(path string, owned fs.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket during cleanup: %w", err)
	}
	if owned != nil && !os.SameFile(owned, current) {
		return nil
	}
	if owned == nil && current.Mode()&os.ModeSocket == 0 {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove control socket: %w", err)
	}
	return nil
}
