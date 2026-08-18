package mirror

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	ProtocolVersion = 3

	TagClientHello byte = 0x01
	TagClose       byte = 0x05
	TagInput       byte = 0x06
	TagOutput      byte = 0x07
	TagReady       byte = 0x08
	TagShutdown    byte = 0x0a
	TagError       byte = 0x0b

	MaxWireMessage       = 128 << 10
	MaxClientQueueBytes  = 1 << 20
	MaxHandshakes        = 16
	MaxClients           = 8
	HandshakeTimeout     = 5 * time.Second
	TunnelStartupTimeout = 30 * time.Second
)

type ClientHello struct {
	Version int `json:"version"`
}

type Ready struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
	Columns    uint16 `json:"columns"`
	Rows       uint16 `json:"rows"`
}

type Shutdown struct {
	Reason string `json:"reason"`
	Retry  bool   `json:"retry"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Retry   bool   `json:"retry"`
}

func EncodeControl(tag byte, value any) ([]byte, error) {
	if !isControlTag(tag) {
		return nil, fmt.Errorf("tag 0x%02x does not carry JSON control data", tag)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode control tag 0x%02x: %w", tag, err)
	}
	if len(payload)+1+16 > MaxWireMessage {
		return nil, errors.New("control payload exceeds wire limit")
	}
	return payload, nil
}

func DecodeControl(tag byte, payload []byte, dst any) error {
	if !isControlTag(tag) {
		return fmt.Errorf("tag 0x%02x does not carry JSON control data", tag)
	}
	if len(payload)+1+16 > MaxWireMessage {
		return errors.New("control payload exceeds wire limit")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode control tag 0x%02x: %w", tag, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("control payload contains trailing JSON value")
		}
		return fmt.Errorf("decode trailing control data: %w", err)
	}
	return nil
}

func ValidateClientFrame(tag byte, value any) error {
	switch tag {
	case TagClientHello:
		hello, ok := value.(ClientHello)
		if !ok {
			return errors.New("invalid client hello payload")
		}
		if hello.Version != ProtocolVersion {
			return fmt.Errorf("unsupported protocol version %d", hello.Version)
		}
	case TagClose:
		switch payload := value.(type) {
		case nil:
		case []byte:
			if len(payload) != 0 {
				return errors.New("close payload must be empty")
			}
		default:
			return errors.New("invalid close payload")
		}
	case TagInput:
		payload, ok := value.([]byte)
		if !ok {
			return errors.New("invalid input payload")
		}
		if len(payload)+1+16 > MaxWireMessage {
			return errors.New("input payload exceeds wire limit")
		}
	default:
		return fmt.Errorf("tag 0x%02x is not valid from client", tag)
	}
	return nil
}

func ValidateServerFrame(tag byte, value any) error {
	switch tag {
	case TagClose:
		switch payload := value.(type) {
		case nil:
		case []byte:
			if len(payload) != 0 {
				return errors.New("close acknowledgement payload must be empty")
			}
		default:
			return errors.New("invalid close acknowledgement payload")
		}
	case TagOutput:
		payload, ok := value.([]byte)
		if !ok {
			return errors.New("invalid output payload")
		}
		if len(payload)+1+16 > MaxWireMessage {
			return errors.New("output payload exceeds wire limit")
		}
	case TagReady:
		opened, ok := value.(Ready)
		if !ok {
			return errors.New("invalid opened payload")
		}
		if err := validateIdentity(opened.ID, opened.Generation); err != nil {
			return err
		}
		return validateDimensions(opened.Columns, opened.Rows)
	case TagShutdown:
		if _, ok := value.(Shutdown); !ok {
			return errors.New("invalid shutdown payload")
		}
	case TagError:
		protocolErr, ok := value.(ProtocolError)
		if !ok || protocolErr.Code == "" || protocolErr.Message == "" {
			return errors.New("invalid error payload")
		}
	default:
		return fmt.Errorf("tag 0x%02x is not valid from server", tag)
	}
	return nil
}

func ChunkOutput(data []byte) [][]byte {
	const maxPlainPayload = MaxWireMessage - 1 - 16
	if len(data) == 0 {
		return nil
	}
	chunks := make([][]byte, 0, (len(data)+maxPlainPayload-1)/maxPlainPayload)
	for len(data) > 0 {
		size := min(len(data), maxPlainPayload)
		chunks = append(chunks, data[:size])
		data = data[size:]
	}
	return chunks
}

func validateIdentity(id, generation string) error {
	if id == "" {
		return errors.New("session id is required")
	}
	if generation == "" {
		return errors.New("session generation is required")
	}
	return nil
}

func validateDimensions(columns, rows uint16) error {
	if columns < 2 || columns > 500 {
		return fmt.Errorf("columns %d outside 2..500", columns)
	}
	if rows < 2 || rows > 300 {
		return fmt.Errorf("rows %d outside 2..300", rows)
	}
	return nil
}

func isControlTag(tag byte) bool {
	switch tag {
	case TagClientHello, TagReady, TagShutdown, TagError:
		return true
	default:
		return false
	}
}
