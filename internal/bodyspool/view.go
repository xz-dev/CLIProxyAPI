package bodyspool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
)

// registry maps a live spool's nonce to the spool, so content fingerprints computed without a
// request context (session affinity turns) can read the spliced value of a skeleton fragment.
// Only fingerprints use it; upstream sends resolve markers through the request context only.
var registry sync.Map // nonce -> *Spool

func register(s *Spool)   { registry.Store(s.nonce, s) }
func unregister(s *Spool) { registry.Delete(s.nonce) }

func lookup(fragment []byte) *Spool {
	i := bytes.Index(fragment, []byte(MarkerPrefix))
	if i < 0 || i+len(MarkerPrefix)+nonceHex > len(fragment) {
		return nil
	}
	v, ok := registry.Load(string(fragment[i+len(MarkerPrefix) : i+len(MarkerPrefix)+nonceHex]))
	if !ok {
		return nil
	}
	return v.(*Spool)
}

// PartDigest is the fingerprint view of a large spliced value: its length, a sparse sample of
// head, middle and tail (limit bytes in total) and the SHA-256 of the whole spliced value.
type PartDigest struct {
	Size   int
	Sample string
	Digest string
}

// SplicedPartDigest computes the fingerprint view of a skeleton fragment as if it were the
// original bytes. ok is false when the fragment carries no live marker.
func SplicedPartDigest(fragment string, limit int) (PartDigest, bool) {
	sp := lookup([]byte(fragment))
	if sp == nil || !sp.Has([]byte(fragment)) {
		return PartDigest{}, false
	}
	key := fragment
	if v, ok := sp.digests.Load(key); ok {
		return v.(PartDigest), true
	}
	ps, err := sp.parts([]byte(fragment))
	if err != nil {
		return PartDigest{}, false
	}
	view := &splicedView{s: sp, parts: ps}
	for _, p := range ps {
		if p.blob >= 0 {
			view.size += sp.blobs[p.blob].n
		} else {
			view.size += int64(len(p.skel))
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(view, 0, view.size)); err != nil {
		return PartDigest{}, false
	}
	n := int(view.size)
	sample := make([]byte, 0, limit)
	if n <= limit {
		sample = append(sample, readRange(view, 0, n)...)
	} else {
		head, middle := limit/3, limit/3
		tail := limit - head - middle
		sample = append(sample, readRange(view, 0, head)...)
		sample = append(sample, readRange(view, (n-middle)/2, middle)...)
		sample = append(sample, readRange(view, n-tail, tail)...)
	}
	d := PartDigest{Size: n, Sample: string(sample), Digest: hex.EncodeToString(h.Sum(nil))}
	sp.digests.Store(key, d)
	return d, true
}

func readRange(r io.ReaderAt, off, n int) []byte {
	b := make([]byte, n)
	got, _ := r.ReadAt(b, int64(off))
	return b[:got]
}

// splicedView is an io.ReaderAt over the spliced view of a skeleton fragment.
type splicedView struct {
	s     *Spool
	parts []part
	size  int64
}

func (v *splicedView) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	var pos int64
	for _, pt := range v.parts {
		n := int64(len(pt.skel))
		if pt.blob >= 0 {
			n = v.s.blobs[pt.blob].n
		}
		if len(p) == 0 {
			break
		}
		if off >= pos+n {
			pos += n
			continue
		}
		rel := off - pos
		want := min(int64(len(p)), n-rel)
		if pt.blob < 0 {
			copy(p, pt.skel[rel:rel+want])
		} else {
			f, err := v.s.file()
			if err != nil {
				return total, err
			}
			seg := v.s.blobs[pt.blob]
			if _, err := f.ReadAt(p[:want], seg.off+rel); err != nil {
				return total, err
			}
			_ = dropRange(f, seg.off+rel, want)
		}
		p, off, total, pos = p[want:], off+want, total+int(want), pos+n
	}
	if len(p) > 0 {
		return total, io.EOF
	}
	return total, nil
}

// SplicedSHA256 hashes encoded as if the skeleton fragments it was built from were the original
// bytes (spooled values are canonical, so any JSON re-encoding left them unchanged). ok is false
// when encoded carries no live marker.
func SplicedSHA256(encoded []byte) (sum [32]byte, ok bool) {
	sp := lookup(encoded)
	if sp == nil || !sp.Has(encoded) {
		return sum, false
	}
	r, _, err := sp.Reader(encoded)
	if err != nil {
		return sum, false
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return sum, false
	}
	copy(sum[:], h.Sum(nil))
	return sum, true
}
