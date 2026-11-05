package bodyspool

import (
	"fmt"
	"os"
	"sync"
)

// syncEvery is the D3 page-cache bound: writers fdatasync + DONTNEED and readers DONTNEED
// about every 8 MiB, so a spool keeps at most ~16 MiB of file pages in the cgroup.
const syncEvery = 8 << 20

type segment struct{ off, n int64 }

// Spool owns one anonymous O_TMPFILE file holding the raw (still JSON-escaped) bytes of the
// spooled string values of one request.
type Spool struct {
	mu      sync.Mutex
	f       *os.File
	dir     string
	hop     string
	reserve int64
	nonce   string
	blobs   []segment
	size    int64
	invalid bool // a spooled string breaks encoding/json string rules
	w       *spoolWriter
}

func newSpool(dir, nonce string, reserve int64, hop string) (*Spool, error) {
	f, err := openTmpFile(dir)
	if err != nil {
		return nil, err
	}
	return &Spool{f: f, dir: dir, hop: hop, reserve: reserve, nonce: nonce,
		w: &spoolWriter{f: f, reserve: reserve, dir: dir, hop: hop}}, nil
}

// Close closes the anonymous file, which frees its blocks. Safe on nil and twice.
func (s *Spool) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// Blobs returns the number of spooled blobs.
func (s *Spool) Blobs() int {
	if s == nil {
		return 0
	}
	return len(s.blobs)
}

// BlobBytes returns the number of spooled bytes.
func (s *Spool) BlobBytes() int64 {
	if s == nil {
		return 0
	}
	return s.size
}

// StringsValid reports whether every spooled string obeys encoding/json string rules, so
// json.Valid(full body) == json.Valid(skeleton) && StringsValid().
func (s *Spool) StringsValid() bool { return s == nil || !s.invalid }

func (s *Spool) marker(idx int) []byte {
	return fmt.Appendf(nil, "%s%s:%08x~", MarkerPrefix, s.nonce, idx)
}

func (s *Spool) file() (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil, os.ErrClosed
	}
	return s.f, nil
}

type blobWriter struct {
	s   *Spool
	off int64
	buf []byte
}

func (s *Spool) begin() *blobWriter {
	return &blobWriter{s: s, off: s.size, buf: make([]byte, 0, 64<<10)}
}

func (b *blobWriter) writeByte(c byte) error {
	b.buf = append(b.buf, c)
	if len(b.buf) == cap(b.buf) {
		return b.flush()
	}
	return nil
}

func (b *blobWriter) write(p []byte) error {
	for len(p) > 0 {
		n := copy(b.buf[len(b.buf):cap(b.buf)], p)
		b.buf, p = b.buf[:len(b.buf)+n], p[n:]
		if len(b.buf) == cap(b.buf) {
			if err := b.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *blobWriter) flush() error {
	if len(b.buf) == 0 {
		return nil
	}
	if err := b.s.w.write(b.buf); err != nil {
		return err
	}
	b.s.size += int64(len(b.buf))
	b.buf = b.buf[:0]
	return nil
}

func (b *blobWriter) end(valid bool) (int, error) {
	if err := b.flush(); err != nil {
		return 0, err
	}
	if err := b.s.w.finish(); err != nil {
		return 0, err
	}
	b.s.invalid = b.s.invalid || !valid
	b.s.blobs = append(b.s.blobs, segment{off: b.off, n: b.s.size - b.off})
	return len(b.s.blobs) - 1, nil
}

// spoolWriter appends to an anonymous file under the D3 rules: a disk-floor check before each
// 8 MiB window, then fdatasync + DONTNEED of the window once written. It never falls back to memory.
type spoolWriter struct {
	f       *os.File
	dir     string
	hop     string
	reserve int64
	off     int64 // bytes written
	synced  int64 // bytes synced and dropped from the page cache
	checked int64 // floor checked up to this offset
}

func (w *spoolWriter) write(p []byte) error {
	if end := w.off + int64(len(p)); end > w.checked {
		if err := checkFloor(w.dir, syncEvery, w.reserve, w.hop); err != nil {
			return err
		}
		w.checked = w.off + syncEvery
	}
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	if err != nil {
		return err
	}
	if w.off-w.synced >= syncEvery {
		return w.finish()
	}
	return nil
}

// finish syncs and drops everything written so far.
func (w *spoolWriter) finish() error {
	if w.off == w.synced {
		return nil
	}
	if err := syncDrop(w.f, w.synced, w.off-w.synced); err != nil {
		return err
	}
	w.synced = w.off
	return nil
}
