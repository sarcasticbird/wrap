package mirror

import (
	"bytes"
	"testing"
)

func TestReadyControlRoundTripAndStrictDecode(t *testing.T) {
	want := Ready{ID: "$3", Generation: "generation", Columns: 120, Rows: 40}
	encoded, err := EncodeControl(TagReady, want)
	if err != nil {
		t.Fatal(err)
	}
	var got Ready
	if err := DecodeControl(TagReady, encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ready = %+v, want %+v", got, want)
	}
	if err := DecodeControl(TagReady, append(encoded, []byte("{}")...), &got); err == nil {
		t.Fatal("DecodeControl accepted trailing JSON")
	}
}

func TestProtocolRejectsRemovedSelectionFrames(t *testing.T) {
	for _, tag := range []byte{0x02, 0x03, 0x04, 0x09} {
		if err := ValidateClientFrame(tag, nil); err == nil {
			t.Fatalf("client selection tag 0x%02x accepted", tag)
		}
		if err := ValidateServerFrame(tag, nil); err == nil {
			t.Fatalf("server selection tag 0x%02x accepted", tag)
		}
		if _, err := EncodeControl(tag, struct{}{}); err == nil {
			t.Fatalf("selection control tag 0x%02x encoded", tag)
		}
	}
}

func TestProtocolValidatesReadyDimensionsAndVersion(t *testing.T) {
	if err := ValidateServerFrame(TagReady, Ready{ID: "$1", Generation: "g", Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateServerFrame(TagReady, Ready{ID: "$1", Generation: "g", Columns: 1, Rows: 24}); err == nil {
		t.Fatal("Ready accepted invalid dimensions")
	}
	if err := ValidateClientFrame(TagClientHello, ClientHello{Version: ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateClientFrame(TagClientHello, ClientHello{Version: 2}); err == nil {
		t.Fatal("client hello accepted protocol v2")
	}
}

func TestServerCloseAcknowledgementMustBeEmpty(t *testing.T) {
	if err := ValidateServerFrame(TagClose, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateServerFrame(TagClose, []byte("x")); err == nil {
		t.Fatal("server close accepted payload")
	}
}

func TestChunkOutputKeepsCiphertextWithinWireLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 3*MaxWireMessage)
	chunks := ChunkOutput(data)
	var total int
	for _, chunk := range chunks {
		if len(chunk)+1+16 > MaxWireMessage {
			t.Fatalf("chunk ciphertext size = %d", len(chunk)+17)
		}
		total += len(chunk)
	}
	if total != len(data) {
		t.Fatalf("chunked bytes = %d, want %d", total, len(data))
	}
}
