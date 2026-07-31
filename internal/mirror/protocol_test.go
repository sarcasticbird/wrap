package mirror

import (
	"bytes"
	"strings"
	"testing"
)

func TestControlPayloadRoundTripAndStrictDecode(t *testing.T) {
	encoded, err := EncodeControl(TagOpen, OpenRequest{
		ID:         "$7",
		Generation: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got OpenRequest
	if err := DecodeControl(TagOpen, encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "$7" || got.Generation != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("decoded open = %+v", got)
	}

	for _, malformed := range [][]byte{
		[]byte(`{"id":"$7","generation":"0123456789abcdef0123456789abcdef","columns":80,"rows":24}`),
		[]byte(`{"id":"$7","generation":"0123456789abcdef0123456789abcdef"}{}`),
	} {
		if err := DecodeControl(TagOpen, malformed, &got); err == nil {
			t.Fatalf("accepted malformed payload %s", malformed)
		}
	}
}

func TestProtocolValidatesDirectionIdentityAndDimensions(t *testing.T) {
	valid := OpenRequest{
		ID:         "$7",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	if err := ValidateClientFrame(TagOpen, valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []OpenRequest{
		{Generation: valid.Generation},
		{ID: valid.ID},
	} {
		if err := ValidateClientFrame(TagOpen, bad); err == nil {
			t.Fatalf("accepted invalid open %+v", bad)
		}
	}
	if err := ValidateClientFrame(TagOutput, []byte("server only")); err == nil {
		t.Fatal("accepted server-only output tag from a client")
	}
	if err := ValidateServerFrame(TagInput, []byte("client only")); err == nil {
		t.Fatal("accepted client-only input tag from a server")
	}
	opened := Opened{
		ID: valid.ID, Generation: valid.Generation, Columns: 80, Rows: 24,
	}
	if err := ValidateServerFrame(TagOpened, opened); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Opened{
		{Generation: opened.Generation, Columns: 80, Rows: 24},
		{ID: opened.ID, Columns: 80, Rows: 24},
		{ID: opened.ID, Generation: opened.Generation, Columns: 1, Rows: 24},
		{ID: opened.ID, Generation: opened.Generation, Columns: 501, Rows: 24},
		{ID: opened.ID, Generation: opened.Generation, Columns: 80, Rows: 1},
		{ID: opened.ID, Generation: opened.Generation, Columns: 80, Rows: 301},
	} {
		if err := ValidateServerFrame(TagOpened, bad); err == nil {
			t.Fatalf("accepted invalid opened payload %+v", bad)
		}
	}
}

func TestServerCloseAcknowledgementMustBeEmpty(t *testing.T) {
	for _, payload := range []any{nil, []byte{}} {
		if err := ValidateServerFrame(TagClose, payload); err != nil {
			t.Fatalf("empty close acknowledgement %T rejected: %v", payload, err)
		}
	}
	if err := ValidateServerFrame(TagClose, []byte("unexpected")); err == nil {
		t.Fatal("non-empty close acknowledgement accepted")
	}
}

func TestChunkOutputKeepsCiphertextWithinWireLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), MaxWireMessage*2)
	chunks := ChunkOutput(data)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want at least 3", len(chunks))
	}
	var joined []byte
	for _, chunk := range chunks {
		if len(chunk)+1+16 > MaxWireMessage {
			t.Fatalf("plaintext chunk %d produces oversized GCM message", len(chunk))
		}
		joined = append(joined, chunk...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("chunking changed output")
	}
}

func TestClientHelloRequiresVersionTwo(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Fatalf("protocol version = %d, want 2", ProtocolVersion)
	}
	for _, version := range []int{0, 1, 3} {
		payload, err := EncodeControl(TagClientHello, ClientHello{Version: version})
		if err != nil {
			t.Fatal(err)
		}
		var hello ClientHello
		if err := DecodeControl(TagClientHello, payload, &hello); err != nil {
			t.Fatal(err)
		}
		if err := ValidateClientFrame(TagClientHello, hello); err == nil ||
			!strings.Contains(err.Error(), "version") {
			t.Fatalf("version %d validation = %v", version, err)
		}
	}
}
