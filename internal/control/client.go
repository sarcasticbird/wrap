package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

const defaultCallTimeout = 5 * time.Second

func Call(ctx context.Context, socketPath string, request Request) (Status, error) {
	if ctx == nil {
		return Status{}, errors.New("control context is required")
	}
	if !filepath.IsAbs(socketPath) {
		return Status{}, errors.New("control socket path must be absolute")
	}
	if err := request.validate(); err != nil {
		return Status{}, &Error{Code: CodeInvalidRequest, Message: err.Error()}
	}
	callCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		callCtx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
	}
	defer cancel()

	var payload bytes.Buffer
	if err := json.NewEncoder(&payload).Encode(request); err != nil {
		return Status{}, fmt.Errorf("encode control request: %w", err)
	}
	if payload.Len() > MaxRequestBytes {
		return Status{}, &Error{Code: CodeInvalidRequest, Message: "control request is too large"}
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(callCtx, "unix", socketPath)
	if err != nil {
		if callCtx.Err() != nil {
			return Status{}, callCtx.Err()
		}
		return Status{}, fmt.Errorf("connect to Wrap control socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stopClose := context.AfterFunc(callCtx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := callCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Status{}, fmt.Errorf("set control deadline: %w", err)
		}
	}
	if _, err := conn.Write(payload.Bytes()); err != nil {
		if contextErr := callContextError(callCtx, err); contextErr != nil {
			return Status{}, contextErr
		}
		return Status{}, fmt.Errorf("write control request: %w", err)
	}

	decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, maxResponseBytes+1)))
	decoder.DisallowUnknownFields()
	var reply response
	if err := decoder.Decode(&reply); err != nil {
		if contextErr := callContextError(callCtx, err); contextErr != nil {
			return Status{}, contextErr
		}
		return Status{}, fmt.Errorf("decode control response: %w", err)
	}
	if reply.Error != nil {
		return Status{}, reply.Error
	}
	if reply.Status == nil {
		return Status{}, errors.New("control response contains no status")
	}
	return *reply.Status, nil
}

func callContextError(ctx context.Context, operationErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var netErr net.Error
	deadline, hasDeadline := ctx.Deadline()
	if errors.As(operationErr, &netErr) && netErr.Timeout() && hasDeadline && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
