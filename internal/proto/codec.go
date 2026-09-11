package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Encoder writes JSON Lines. Safe for concurrent use.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes one line: the fields of v plus "type". v may be nil.
func (e *Encoder) Encode(typ string, v any) error {
	fields := map[string]any{}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &fields); err != nil {
			return fmt.Errorf("proto: payload must be a JSON object: %w", err)
		}
		if fields == nil {
			fields = map[string]any{}
		}
	}
	fields["type"] = typ
	line, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(line)
	return err
}

// Decoder reads JSON Lines from a buffered reader. Use only on streams that
// carry nothing but JSON Lines (the control stream).
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: bufio.NewReader(r)} }

// Decode reads one line and returns its type and raw JSON.
func (d *Decoder) Decode() (string, json.RawMessage, error) {
	line, err := d.r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return "", nil, io.EOF
		}
		if err != io.EOF {
			return "", nil, err
		}
	}
	return parseLine(line)
}

// ReadHeader reads exactly one line from r without buffering past the
// newline, so the caller can continue reading raw payload from r.
func ReadHeader(r io.Reader) (string, json.RawMessage, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			line = append(line, b[0])
			if b[0] == '\n' {
				break
			}
			if len(line) > 64*1024 {
				return "", nil, errors.New("proto: header line too long")
			}
		}
		if err != nil {
			if err == io.EOF && len(line) == 0 {
				return "", nil, io.EOF
			}
			return "", nil, err
		}
	}
	return parseLine(line)
}

func parseLine(line []byte) (string, json.RawMessage, error) {
	line = bytes.TrimRight(line, "\r\n")
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return "", nil, fmt.Errorf("proto: bad line %q: %w", line, err)
	}
	if head.Type == "" {
		return "", nil, fmt.Errorf("proto: line without type: %q", line)
	}
	return head.Type, json.RawMessage(line), nil
}

// Unmarshal decodes raw into v.
func Unmarshal(raw json.RawMessage, v any) error { return json.Unmarshal(raw, v) }
