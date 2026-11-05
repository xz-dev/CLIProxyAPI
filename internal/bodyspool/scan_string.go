package bodyspool

import (
	"bytes"
	"io"
)

// eligibleKey is the tier A allow-list (design D2): image_url (string or .url), file_data,
// Anthropic source.data. data: URLs at any position are eligible as well (see str).
func eligibleKey(key, parent string) bool {
	switch key {
	case "image_url", "file_data":
		return true
	case "url":
		return parent == "image_url"
	case "data":
		return parent == "source"
	}
	return false
}

var dataPrefix = []byte("data:")

// str consumes one string after its opening quote (already emitted).
func (s *scanner) str(top int) error {
	inObject := top >= 0 && s.stack[top].object
	isKey := inObject && s.stack[top].expectKey
	keyOK := false
	if isKey {
		s.lastKey = s.lastKey[:0]
	} else if inObject {
		keyOK = eligibleKey(string(s.lastKey), s.stack[top].key)
	}
	candidate := !isKey && !s.collision
	buf := s.buf[:0] // pending value bytes while deciding (bounded by Threshold), reused
	defer func() { s.buf = buf[:0] }()
	esc := false
	for {
		b, err := s.in.ReadByte()
		if err != nil {
			if errEmit := s.emitBytes(buf); errEmit != nil {
				return errEmit
			}
			if err == io.EOF {
				return nil // truncated JSON: the skeleton still equals the input
			}
			return err
		}
		s.note(b)
		end := !esc && b == '"'
		esc = !esc && b == '\\'
		if end {
			if errEmit := s.emitBytes(buf); errEmit != nil {
				return errEmit
			}
			return s.emit(b)
		}
		if isKey {
			if len(s.lastKey) < 64 {
				s.lastKey = append(s.lastKey, b)
			}
			if errEmit := s.emit(b); errEmit != nil {
				return errEmit
			}
			continue
		}
		if !candidate {
			if errEmit := s.emit(b); errEmit != nil {
				return errEmit
			}
			continue
		}
		buf = append(buf, b)
		if !keyOK && len(buf) >= len(dataPrefix) && !bytes.HasPrefix(buf, dataPrefix) {
			candidate = false
			if errEmit := s.emitBytes(buf); errEmit != nil {
				return errEmit
			}
			buf = buf[:0]
			continue
		}
		if len(buf) >= s.o.Threshold && !esc && !s.collision {
			return s.spill(buf)
		}
	}
}

// spill keeps a short inline data: URL header, writes a marker, and streams the rest of the string
// value to the spool. It is only called outside a backslash escape.
func (s *scanner) spill(buf []byte) error {
	keep := 0
	if bytes.HasPrefix(buf, dataPrefix) {
		if i := bytes.IndexByte(buf[:min(len(buf), dataURLKeep)], ','); i >= 0 {
			keep = i + 1
		}
	}
	for keep > 0 && (buf[keep-1] >= 0x80 || buf[keep-1] == '\\') { // never split a rune or an escape
		keep--
	}
	if err := s.emitBytes(buf[:keep]); err != nil {
		return err
	}
	if s.sp == nil {
		sp, err := newSpool(s.o.Dir, s.nonce, s.o.Reserve, s.o.Hop)
		if err != nil {
			return err
		}
		s.sp = sp
	}
	blob := s.sp.begin()
	var v validator
	v.feed(buf[keep:])
	if err := blob.write(buf[keep:]); err != nil {
		return err
	}
	esc, closed := false, false
	for {
		b, err := s.in.ReadByte()
		if err == io.EOF {
			break // truncated: the marker still splices back to exactly the input bytes
		}
		if err != nil {
			return err
		}
		s.note(b)
		if !esc && b == '"' {
			closed = true
			break
		}
		esc = !esc && b == '\\'
		v.feedByte(b)
		if err := blob.writeByte(b); err != nil {
			return err
		}
	}
	idx, err := blob.end(v.ok())
	if err != nil {
		return err
	}
	if err := s.emitBytes(s.sp.marker(idx)); err != nil {
		return err
	}
	if closed {
		return s.emit('"')
	}
	return nil
}

// validator applies encoding/json string rules to spooled bytes, so the spool can report whether
// the full body would pass json.Valid (no raw control bytes, only valid escapes).
type validator struct {
	state int // 0 plain, 1 after '\', 2..5 expecting hex digits of \u
	bad   bool
}

func (v *validator) feed(p []byte) {
	for _, b := range p {
		v.feedByte(b)
	}
}

func (v *validator) feedByte(b byte) {
	switch {
	case v.state == 1:
		switch b {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			v.state = 0
		case 'u':
			v.state = 2
		default:
			v.bad, v.state = true, 0
		}
	case v.state >= 2:
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
			v.bad = true
		}
		v.state++
		if v.state == 6 {
			v.state = 0
		}
	case b == '\\':
		v.state = 1
	case b < 0x20:
		v.bad = true
	}
}

func (v *validator) ok() bool { return !v.bad && v.state == 0 }
