package bodyspool

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
)

// derived is a spool segment computed from a spliced view (for example codex zstd output),
// cached by the SHA-256 of the skeleton it was computed from.
type derived struct {
	key [32]byte
	seg segment
}

// Derive appends fill's output for skel to the spool and returns its segment. The disk floor is
// checked for reserve bytes before anything is written (compression output is reserved, D2), and
// the output is written under the same fdatasync + DONTNEED rules. A repeated call for the same
// skeleton (a retry) reuses the segment.
func (s *Spool) Derive(skel []byte, reserve int64, fill func(w io.Writer, spliced io.Reader) error) (int64, func() io.Reader, error) {
	key := sha256.Sum256(skel)
	s.deriveMu.Lock()
	defer s.deriveMu.Unlock()
	for _, d := range s.derived {
		if d.key == key {
			return d.seg.n, s.segmentReader(d.seg), nil
		}
	}
	f, err := s.file()
	if err != nil {
		return 0, nil, err
	}
	s.tail = max(s.tail, s.size)
	if err := checkFloor(s.dir, reserve, s.reserve, s.hop); err != nil {
		return 0, nil, err
	}
	src, _, err := s.Reader(skel)
	if err != nil {
		return 0, nil, err
	}
	w := &spoolWriter{f: f, dir: s.dir, hop: s.hop, reserve: s.reserve, off: s.tail, synced: s.tail, checked: s.tail + reserve}
	start := s.tail
	if err := fill(&appendWriter{w: w}, src); err != nil {
		return 0, nil, err
	}
	if err := w.finish(); err != nil {
		return 0, nil, err
	}
	s.tail = w.off
	seg := segment{off: start, n: w.off - start}
	s.derived = append(s.derived, derived{key: key, seg: seg})
	return seg.n, s.segmentReader(seg), nil
}

func (s *Spool) segmentReader(seg segment) func() io.Reader {
	return func() io.Reader { return &blobReader{s: s, off: seg.off, end: seg.off + seg.n, dropped: seg.off} }
}

type appendWriter struct{ w *spoolWriter }

func (a *appendWriter) Write(p []byte) (int, error) {
	if err := a.w.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// NewDerivedHTTPRequest builds an upstream request whose body is a derived spool segment.
func NewDerivedHTTPRequest(ctx context.Context, method, url string, n int64, open func() io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, open())
	if err != nil {
		return nil, err
	}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(open()), nil }
	return req, nil
}
