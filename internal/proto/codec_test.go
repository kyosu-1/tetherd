package proto

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestEncodeDecodeHello(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	in := Hello{Version: Version, User: "shota", Token: "tok", Incoming: Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if err := enc.Encode(TypeHello, in); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got[len(got)-1] != '\n' {
		t.Fatalf("line must end with newline: %q", got)
	}
	typ, raw, err := NewDecoder(&buf).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeHello {
		t.Fatalf("type = %q", typ)
	}
	var out Hello
	if err := Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("roundtrip mismatch: %+v != %+v", out, in)
	}
}

func TestEncodeNilPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(TypePing, nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "{\"type\":\"ping\"}\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestDecodeEOF(t *testing.T) {
	_, _, err := NewDecoder(bytes.NewReader(nil)).Decode()
	if err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

// ReadHeader must not consume bytes past the first newline, because the
// remainder of a dial/http stream is raw payload.
func TestReadHeaderDoesNotOverread(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		NewEncoder(a).Encode(TypeDial, DialHeader{Addr: "10.0.0.1:5432"})
		a.Write([]byte("payload"))
		a.Close()
	}()
	typ, raw, err := ReadHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeDial {
		t.Fatalf("type = %q", typ)
	}
	var h DialHeader
	if err := Unmarshal(raw, &h); err != nil || h.Addr != "10.0.0.1:5432" {
		t.Fatalf("header = %+v, err = %v", h, err)
	}
	rest, _ := io.ReadAll(b)
	if string(rest) != "payload" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestEncodeTypedNilPayload(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(TypeError, (*Error)(nil)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "{\"type\":\"error\"}\n" {
		t.Fatalf("got %q", buf.String())
	}
}

// ResolveHeader/ResolveReply are the wire protocol a deployed agent speaks.
// The end-to-end tests in internal/session are symmetric (the same struct
// is encoded and decoded on both ends), so they cannot see a JSON tag
// rename break compatibility with a real agent. These golden strings can.
func TestResolveHeaderWireFormat(t *testing.T) {
	var buf bytes.Buffer
	h := ResolveHeader{Name: "api.myapp.internal", QType: "A"}
	if err := NewEncoder(&buf).Encode(TypeResolve, h); err != nil {
		t.Fatal(err)
	}
	want := `{"name":"api.myapp.internal","qtype":"A","type":"resolve"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("ResolveHeader wire format changed:\n got  %q\n want %q", got, want)
	}
}

func TestResolveReplyWireFormat(t *testing.T) {
	var buf bytes.Buffer
	r := ResolveReply{OK: true, Addrs: []string{"10.0.11.229", "10.0.12.7"}, TTL: 30}
	if err := NewEncoder(&buf).Encode(TypeResolve, r); err != nil {
		t.Fatal(err)
	}
	want := `{"addrs":["10.0.11.229","10.0.12.7"],"ok":true,"ttl":30,"type":"resolve"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("ResolveReply wire format changed:\n got  %q\n want %q", got, want)
	}
}

func TestResolveReplyErrorWireFormat(t *testing.T) {
	var buf bytes.Buffer
	r := ResolveReply{Error: "NXDOMAIN"}
	if err := NewEncoder(&buf).Encode(TypeResolve, r); err != nil {
		t.Fatal(err)
	}
	want := `{"error":"NXDOMAIN","ok":false,"type":"resolve"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("ResolveReply (failure) wire format changed:\n got  %q\n want %q", got, want)
	}
}

// TestResolveReplyNotFoundWireFormat pins the additive not_found field: an
// agent built before it existed omits it, which an older CLI (and the
// json:",omitempty" tag) both treat as false, so this stays backward
// compatible in both directions.
func TestResolveReplyNotFoundWireFormat(t *testing.T) {
	var buf bytes.Buffer
	r := ResolveReply{Error: "no such host", NotFound: true}
	if err := NewEncoder(&buf).Encode(TypeResolve, r); err != nil {
		t.Fatal(err)
	}
	want := `{"error":"no such host","not_found":true,"ok":false,"type":"resolve"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("ResolveReply (not found) wire format changed:\n got  %q\n want %q", got, want)
	}
}

func TestEncodeConcurrent(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := enc.Encode(TypePing, nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 800 {
		t.Fatalf("expected 800 lines, got %d", len(lines))
	}
	expected := "{\"type\":\"ping\"}"
	for i, line := range lines {
		if line != expected {
			t.Fatalf("line %d: expected %q, got %q", i, expected, line)
		}
	}
}
