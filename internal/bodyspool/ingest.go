// Package bodyspool implements the skeleton + blob spool pattern (large-request-capacity D2/D3):
// a streamed JSON request body is copied into a small in-memory skeleton while large media
// string values are written to an anonymous on-disk spool and replaced by fixed-length markers.
// Existing JSON code runs unchanged on the skeleton; a splice reader restores the blobs at egress.
package bodyspool

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// MarkerPrefix opens every marker: ~lrcblob:<32 hex nonce>:<8 hex index>~ (all ASCII outside the
// base64 alphabet and never escaped by JSON encoders).
const MarkerPrefix = "~lrcblob:"

const (
	nonceHex  = 32
	indexHex  = 8
	markerLen = len(MarkerPrefix) + nonceHex + 1 + indexHex + 1
	// dataURLKeep bounds the inline data: URL header kept before the marker (media type for translators).
	dataURLKeep = 256
)

// Options configures one ingest.
type Options struct {
	Dir         string                          // spool directory, validated by CheckDir
	Threshold   int                             // T: an eligible string of at least this many bytes is spooled
	InlineLimit int64                           // S: skeleton byte limit; 0 means no limit
	ChargeStep  int64                           // skeleton bytes per Charge call; 0 disables charging
	Charge      func(skeletonBytes int64) error // called before the skeleton grows past each step
	Reserve     int64                           // disk floor kept free by every spool write
	Hop         string                          // hop name used in errors
}

// Ingest copies r into a skeleton. Strings at allow-listed media keys (and data: URLs) of at least
// Threshold bytes are spooled; everything else stays inline. The returned Spool is nil when nothing
// was spooled. The caller closes the Spool on every exit path.
func Ingest(r io.Reader, o Options) ([]byte, *Spool, error) {
	var nonce [nonceHex / 2]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	sc := &scanner{in: bufio.NewReaderSize(r, 64<<10), o: o, nonce: hex.EncodeToString(nonce[:])}
	sc.guard = MarkerPrefix + sc.nonce
	if o.Threshold <= 0 {
		sc.o.Threshold = 64 << 10
	}
	errRun := sc.run()
	if errRun == nil && sc.collision {
		errRun = &Error{Status: 500, Code: "marker_collision", Hop: o.Hop, Message: "request body contains the spool marker nonce"}
	}
	if errRun != nil {
		_ = sc.sp.Close()
		return nil, nil, errRun
	}
	return sc.out.Bytes(), sc.sp, nil
}

type frame struct {
	object, expectKey bool
	key               string // key under which this container sits ("" in arrays / at the root)
}

type scanner struct {
	in        *bufio.Reader
	out       bytes.Buffer
	o         Options
	nonce     string
	guard     string
	sp        *Spool
	stack     []frame
	lastKey   []byte
	charged   int64
	collision bool
	match     int
	buf       []byte
}

// note tracks whether the input contains MarkerPrefix+nonce. '~' occurs only at index 0 of the
// guard, so a mismatch can restart at 0 or 1 without missing an overlap.
func (s *scanner) note(b byte) {
	if b == s.guard[s.match] {
		s.match++
		if s.match == len(s.guard) {
			s.collision, s.match = true, 0
		}
		return
	}
	s.match = 0
	if b == s.guard[0] {
		s.match = 1
	}
}

// grow accounts n skeleton bytes about to be written: inline-bytes limit, then memory charge steps.
func (s *scanner) grow(n int) error {
	next := int64(s.out.Len() + n)
	if s.o.InlineLimit > 0 && next > s.o.InlineLimit {
		return &Error{Status: 413, Code: "request_too_large", Hop: s.o.Hop, Limit: s.o.InlineLimit,
			Message: fmt.Sprintf("%s: request inline bytes (outside spooled media) exceed the inline-bytes limit of %d bytes", s.o.Hop, s.o.InlineLimit)}
	}
	for s.o.ChargeStep > 0 && s.o.Charge != nil && next > s.charged {
		if err := s.o.Charge(s.o.ChargeStep); err != nil {
			return err
		}
		s.charged += s.o.ChargeStep
	}
	return nil
}

func (s *scanner) emit(b byte) error {
	if err := s.grow(1); err != nil {
		return err
	}
	s.out.WriteByte(b)
	return nil
}

func (s *scanner) emitBytes(p []byte) error {
	if err := s.grow(len(p)); err != nil {
		return err
	}
	s.out.Write(p)
	return nil
}

func (s *scanner) run() error {
	for {
		b, err := s.in.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		s.note(b)
		top := len(s.stack) - 1
		switch b {
		case '{', '[':
			key := ""
			if top >= 0 && s.stack[top].object {
				key = string(s.lastKey)
			}
			s.stack = append(s.stack, frame{object: b == '{', expectKey: b == '{', key: key})
		case '}', ']':
			if top >= 0 {
				s.stack = s.stack[:top]
			}
		case ',':
			if top >= 0 && s.stack[top].object {
				s.stack[top].expectKey = true
			}
		case ':':
			if top >= 0 {
				s.stack[top].expectKey = false
			}
		case '"':
			if err := s.emit(b); err != nil {
				return err
			}
			if err := s.str(top); err != nil {
				return err
			}
			continue
		}
		if err := s.emit(b); err != nil {
			return err
		}
	}
}
