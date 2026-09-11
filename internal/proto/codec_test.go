package proto

import (
	"bytes"
	"io"
	"net"
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
