package bodyspool

import (
	"bytes"
	"fmt"
	"io"
)

// part is one piece of the spliced view: skeleton bytes, or a spooled blob when blob >= 0.
type part struct {
	skel []byte
	blob int
}

// parts resolves every marker of this spool's nonce in skel. A marker that carries the nonce but
// does not resolve to a blob fails closed (D2: markers never leave the hop).
func (s *Spool) parts(skel []byte) ([]part, error) {
	if s == nil || len(s.blobs) == 0 {
		return []part{{skel: skel, blob: -1}}, nil
	}
	guard := []byte(MarkerPrefix + s.nonce)
	var out []part
	last := 0
	for i := 0; ; {
		j := bytes.Index(skel[i:], guard)
		if j < 0 {
			break
		}
		start := i + j
		end := start + markerLen
		idx, ok := -1, end <= len(skel) && skel[start+len(guard)] == ':' && skel[end-1] == '~'
		if ok {
			var v uint32
			_, err := fmt.Sscanf(string(skel[start+len(guard)+1:end-1]), "%08x", &v)
			ok = err == nil && int(v) < len(s.blobs)
			idx = int(v)
		}
		if !ok {
			return nil, &Error{Status: 500, Code: "marker_unresolved", Hop: s.hop, Message: "spool marker could not be resolved; nothing was sent upstream"}
		}
		out = append(out, part{skel: skel[last:start], blob: -1}, part{blob: idx})
		last, i = end, end
	}
	return append(out, part{skel: skel[last:], blob: -1}), nil
}

// Has reports whether skel contains a marker of this spool.
func (s *Spool) Has(skel []byte) bool {
	return s != nil && len(s.blobs) > 0 && bytes.Contains(skel, []byte(MarkerPrefix+s.nonce))
}

// Size returns the byte length of the spliced view of skel.
func (s *Spool) Size(skel []byte) (int64, error) {
	ps, err := s.parts(skel)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range ps {
		if p.blob >= 0 {
			n += s.blobs[p.blob].n
		} else {
			n += int64(len(p.skel))
		}
	}
	return n, nil
}

// Reader returns a fresh reader over the spliced view and its length; call it once per attempt.
// Blob bytes are read with positional reads, so concurrent attempts are safe.
func (s *Spool) Reader(skel []byte) (io.Reader, int64, error) {
	ps, err := s.parts(skel)
	if err != nil {
		return nil, 0, err
	}
	readers := make([]io.Reader, 0, len(ps))
	var n int64
	for _, p := range ps {
		if p.blob < 0 {
			readers = append(readers, bytes.NewReader(p.skel))
			n += int64(len(p.skel))
			continue
		}
		seg := s.blobs[p.blob]
		readers = append(readers, &blobReader{s: s, off: seg.off, end: seg.off + seg.n, dropped: seg.off})
		n += seg.n
	}
	return io.MultiReader(readers...), n, nil
}

// Materialize returns the full spliced bytes when they fit in limit (fallback paths, D2); above the
// limit it returns the named 413 for path without reading the blobs.
func (s *Spool) Materialize(skel []byte, limit int64, path string) ([]byte, error) {
	r, n, err := s.Reader(skel)
	if err != nil {
		return nil, err
	}
	if limit > 0 && n > limit {
		hop := "cpa"
		if s != nil {
			hop = s.hop
		}
		return nil, FallbackTooLarge(hop, path, n, limit)
	}
	out := bytes.NewBuffer(make([]byte, 0, n))
	if _, err := io.Copy(out, r); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Redact returns skel with every marker of this spool replaced by a same-length placeholder that
// carries no nonce; logs and error capture receive only this (D2).
func (s *Spool) Redact(skel []byte) []byte {
	if !s.Has(skel) {
		return skel
	}
	guard := []byte(MarkerPrefix + s.nonce)
	repl := []byte(MarkerPrefix + string(bytes.Repeat([]byte("x"), nonceHex)))
	return bytes.ReplaceAll(skel, guard, repl)
}

// blobReader reads one blob with pread and drops consumed ranges from the page cache every 8 MiB.
type blobReader struct {
	s                 *Spool
	off, end, dropped int64
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}
	f, err := r.s.file()
	if err != nil {
		return 0, err
	}
	if rem := r.end - r.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := f.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && r.off < r.end {
		err = io.ErrUnexpectedEOF
	} else if err == io.EOF {
		err = nil
	}
	if r.off-r.dropped >= syncEvery || (r.off >= r.end && r.off > r.dropped) {
		if errDrop := dropRange(f, r.dropped, r.off-r.dropped); errDrop != nil && err == nil {
			err = errDrop
		}
		r.dropped = r.off
	}
	return n, err
}
