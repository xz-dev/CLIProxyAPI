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

// canonicalByte reports whether b encodes as itself under every JSON encoder the hop uses
// (gjson/sjson raw, encoding/json with HTML escaping, serde): printable ASCII except '"', '\\',
// '<', '>' and '&'. Only canonical strings stay spooled, so a translator that decodes and
// re-encodes a media value produces the same bytes from the marker as from the original.
func canonicalByte(b byte) bool {
	return b >= 0x20 && b < 0x7f && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&'
}

func canonical(p []byte) bool {
	for _, b := range p {
		if !canonicalByte(b) {
			return false
		}
	}
	return true
}

// spill keeps a short inline data: URL header, writes a marker, and streams the rest of the string
// value to the spool. A value that turns out non-canonical (escapes, non-ASCII, HTML characters)
// or truncated is copied back inline, where it counts against the inline-bytes limit. It is only
// called outside a backslash escape.
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
	canon := canonical(buf[keep:])
	if err := blob.write(buf[keep:]); err != nil {
		return err
	}
	closed, esc := false, false
	for {
		b, err := s.in.ReadByte()
		if err == io.EOF {
			break
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
		canon = canon && canonicalByte(b)
		if err := blob.writeByte(b); err != nil {
			return err
		}
	}
	seg, err := blob.end()
	if err != nil {
		return err
	}
	if canon && closed {
		s.sp.blobs = append(s.sp.blobs, seg)
		if err := s.emitBytes(s.sp.marker(len(s.sp.blobs) - 1)); err != nil {
			return err
		}
		return s.emit('"')
	}
	if err := s.inline(seg); err != nil { // the spooled bytes stay as dead space in the anonymous file
		return err
	}
	if closed {
		return s.emit('"')
	}
	return nil
}

// inline copies a spooled segment back into the skeleton.
func (s *scanner) inline(seg segment) error {
	r := &blobReader{s: s.sp, off: seg.off, end: seg.off + seg.n, dropped: seg.off}
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if errEmit := s.emitBytes(chunk[:n]); errEmit != nil {
				return errEmit
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
