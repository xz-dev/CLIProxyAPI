package bodyspool

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func img(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

func sampleBody() []byte {
	a, b := img(200<<10), img(150<<10)
	return []byte(fmt.Sprintf(`{"model":"m","stream":true,"instructions":"be brief","input":[`+
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":%q}]},`+
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"ls\"}"},`+
		`{"type":"function_call_output","call_id":"c1","output":"ok\nline2 \u00e9 \"q\""},`+
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":%q},{"type":"input_image","image_url":{"url":%q}},`+
		`{"type":"input_file","filename":"a.pdf","file_data":%q}]}]}`, a, b, a, img(100<<10)))
}

func opts(t testing.TB) Options {
	return Options{Dir: t.TempDir(), Threshold: 64 << 10, Hop: "cpa"}
}

func ingest(t *testing.T, body []byte) ([]byte, *Spool) {
	t.Helper()
	sk, sp, err := Ingest(bytes.NewReader(body), opts(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sk, sp
}

func mustMaterialize(t *testing.T, sk []byte, sp *Spool) []byte {
	t.Helper()
	full, err := sp.Materialize(sk, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := sp.Size(sk); n != int64(len(full)) {
		t.Fatalf("Size %d != materialized %d", n, len(full))
	}
	return full
}

func TestRoundTripIsByteExact(t *testing.T) {
	body := sampleBody()
	sk, sp := ingest(t, body)
	if sp.Blobs() != 4 || len(sk) > 4<<10 {
		t.Fatalf("blobs=%d skeleton=%d", sp.Blobs(), len(sk))
	}
	if !bytes.Equal(mustMaterialize(t, sk, sp), body) {
		t.Fatal("round trip changed bytes")
	}
	if !bytes.Contains(sk, []byte(`"image_url":"data:image/png;base64,~lrcblob:`)) {
		t.Fatalf("data: URL header not kept inline: %.200s", sk)
	}
}

func TestOnlyAllowListedKeysAndDataURLsSpool(t *testing.T) {
	big := strings.Repeat("A", 70<<10)
	for in, want := range map[string]int{
		`{"text":"` + big + `"}`:                   0, // large text stays inline
		`{"output":"` + big + `"}`:                 0,
		`{"url":"` + big + `"}`:                    0, // url only under image_url
		`{"data":"` + big + `"}`:                   0, // data only under source
		`{"source":{"data":"` + big + `"}}`:        1,
		`{"image_url":{"url":"` + big + `"}}`:      1,
		`{"file_data":"` + big + `"}`:              1,
		`{"anything":"data:x;base64,` + big + `"}`: 1,
		`["data:x;base64,` + big + `"]`:            1,
		`{"image_url":"` + big[:1000] + `"}`:       0, // below T
	} {
		sk, sp := ingest(t, []byte(in))
		if sp.Blobs() != want {
			t.Errorf("%.30s: blobs=%d want %d", in, sp.Blobs(), want)
		}
		if got := mustMaterialize(t, sk, sp); !bytes.Equal(got, []byte(in)) {
			t.Errorf("%.30s: round trip changed bytes", in)
		}
	}
}

func TestRoundTripOddInputs(t *testing.T) {
	big := strings.Repeat("A", 70<<10)
	for _, in := range []string{
		"", "{", `{"image_url":"` + big, `{"image_url":"` + big + `\`, `{"image_url":"` + big + `\"x"}`,
		`{"image_url":"` + big + MarkerPrefix + `0123456789abcdef0123456789abcdef:00000000~"}`, `[` + `"` + big + `"]`,
		`{"image_url":"data:image/png;base64,\u00e9` + big + `"}`, "not json \"" + big, `{"image_url":"` + big + `\ud83d\ude00"}`,
	} {
		sk, sp := ingest(t, []byte(in))
		if got := mustMaterialize(t, sk, sp); !bytes.Equal(got, []byte(in)) {
			t.Fatalf("round trip changed %.40q", in)
		}
	}
}

// TestNonCanonicalMediaStaysInline: escapes, non-ASCII, HTML characters or a truncated value
// put a media string back inline, so json.Valid(skeleton) == json.Valid(body) and re-encoding
// translators see original bytes.
func TestNonCanonicalMediaStaysInline(t *testing.T) {
	big := strings.Repeat("A", 70<<10)
	for in, blobs := range map[string]int{
		`{"image_url":"` + big + `"}`: 1, `{"image_url":"` + big + `\/"}`: 0, `{"image_url":"` + big + "\x01" + `"}`: 0,
		`{"image_url":"` + big + `\u00e9"}`: 0, `{"image_url":"` + big + `<&>"}`: 0, `{"image_url":"` + big + "\xff\xfe" + `"}`: 0,
		`{"image_url":"` + big: 0,
	} {
		sk, sp := ingest(t, []byte(in))
		if sp.Blobs() != blobs || json.Valid(sk) != json.Valid([]byte(in)) {
			t.Errorf("%q: blobs=%d want %d, skeleton valid %v", in[len(in)-8:], sp.Blobs(), blobs, json.Valid(sk))
		}
		if got := mustMaterialize(t, sk, sp); !bytes.Equal(got, []byte(in)) {
			t.Errorf("%q: round trip changed bytes", in[len(in)-8:])
		}
	}
}

func TestMarkerNonceCollisionFailsClosed(t *testing.T) {
	sc := &scanner{nonce: "0123456789abcdef0123456789abcdef"}
	sc.guard = MarkerPrefix + sc.nonce
	for _, b := range []byte(`x~~lrcblob:` + sc.nonce + `y`) {
		sc.note(b)
	}
	if !sc.collision {
		t.Fatal("collision not detected")
	}
}

func TestUnresolvedMarkerFailsClosed(t *testing.T) {
	sk, sp := ingest(t, sampleBody())
	forged := bytes.Replace(sk, []byte(":00000000~"), []byte(":000000ff~"), 1)
	var e *Error
	if _, _, err := sp.Reader(forged); !errors.As(err, &e) || e.Code != "marker_unresolved" || e.Status != 500 {
		t.Fatalf("forged marker: %v", err)
	}
}

func TestRedactRemovesNonce(t *testing.T) {
	sk, sp := ingest(t, sampleBody())
	red := sp.Redact(sk)
	if len(red) != len(sk) || bytes.Contains(red, []byte(sp.nonce)) {
		t.Fatal("redaction kept the nonce or changed the length")
	}
}

func TestInlineLimitIsNamed413(t *testing.T) {
	o := opts(t)
	o.InlineLimit = 1 << 20
	body := `{"input":"` + strings.Repeat("t", 2<<20) + `","image_url":"` + strings.Repeat("A", 4<<20) + `"}`
	_, _, err := Ingest(strings.NewReader(body), o)
	var e *Error
	if !errors.As(err, &e) || e.Status != 413 || e.Code != "request_too_large" || e.Limit != 1<<20 || !strings.Contains(err.Error(), `"hop":"cpa"`) ||
		!strings.Contains(err.Error(), `"limit_bytes":1048576`) || strings.Contains(err.Error(), `"limit":`) {
		t.Fatalf("got %v", err)
	}
	// media beyond the limit does not count: 4 MiB spooled, small inline part
	if _, sp, err := Ingest(strings.NewReader(`{"image_url":"`+strings.Repeat("A", 4<<20)+`"}`), o); err != nil {
		t.Fatal(err)
	} else {
		_ = sp.Close()
	}
}

func TestChargeStepsBeforeGrowth(t *testing.T) {
	o := opts(t)
	var calls int64
	o.ChargeStep, o.Charge = 1<<20, func(n int64) error {
		calls++
		if calls == 3 {
			return &Error{Status: 503, Code: "insufficient_memory", Hop: "cpa"}
		}
		return nil
	}
	_, _, err := Ingest(strings.NewReader(`{"t":"`+strings.Repeat("x", 3<<20)+`"}`), o)
	if e := (*Error)(nil); !errors.As(err, &e) || e.Code != "insufficient_memory" || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte(`{"image_url":"data:x,AAAA","source":{"data":"BBBB"}}`))
	f.Add([]byte(`{"image_url":{"url":"\u00e9\"AAAA\\"},"file_data":"data:\\,x"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		o := opts(t)
		o.Threshold = 4
		sk, sp, err := Ingest(bytes.NewReader(in), o)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Close()
		got, err := sp.Materialize(sk, 0, "fuzz")
		if err != nil || !bytes.Equal(got, in) {
			t.Fatalf("round trip changed %q -> %q (%v)", in, got, err)
		}
		if json.Valid(sk) != json.Valid(in) {
			t.Fatalf("validity parity broken for %q", in)
		}
	})
}

// TestIngestIsFlat streams a large body (default 256MiB, generated, never in RAM) and bounds heap growth.
func TestIngestIsFlat(t *testing.T) {
	size := int64(256 << 20)
	if testing.Short() {
		size = 32 << 20
	}
	chunk := []byte(`{"type":"input_image","image_url":"` + img(1<<20) + `"},`)
	head, tail := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[`), []byte(`{"type":"input_text","text":"x"}]}]}`)
	n := int(size) / len(chunk)
	parts := []io.Reader{bytes.NewReader(head)}
	for i := 0; i < n; i++ {
		parts = append(parts, bytes.NewReader(chunk))
	}
	parts = append(parts, bytes.NewReader(tail))
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	sk, sp, err := Ingest(io.MultiReader(parts...), opts(t))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	runtime.ReadMemStats(&m1)
	r, want, err := sp.Reader(sk)
	if err != nil {
		t.Fatal(err)
	}
	got, errCopy := io.Copy(io.Discard, r)
	if errCopy != nil || got != int64(len(head)+n*len(chunk)+len(tail)) || got != want {
		t.Fatalf("splice length %d (Size %d) err %v", got, want, errCopy)
	}
	alloc := m1.TotalAlloc - m0.TotalAlloc
	t.Logf("body %d MiB, skeleton %d B, ingest allocated %.1f MiB", got>>20, len(sk), float64(alloc)/(1<<20))
	if alloc > 8<<20 {
		t.Fatalf("ingest allocated %d bytes, want < 8MiB", alloc)
	}
	if entries, _ := os.ReadDir(sp.dir); len(entries) != 0 {
		t.Fatalf("spool directory holds named files: %v", entries)
	}
}

// TestCheckSendableFailsClosed: a body with a live marker no spool on the context resolves, or any
// marker-shaped token while the switch is on for the request, is refused with 500
// marker_unresolved; the request's own markers and plain "~lrcblob:" text pass.
func TestCheckSendableFailsClosed(t *testing.T) {
	sk, sp := ingest(t, sampleBody())
	unresolved := func(name string, err error) {
		t.Helper()
		var e *Error
		if !errors.As(err, &e) || e.Status != 500 || e.Code != "marker_unresolved" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	unresolved("live marker, no request", CheckSendable(context.Background(), sk))
	rt := &Runtime{Charger: &Charger{Hop: Hop}}
	own := rt.NewRequest("", true)
	own.SetSpool(sp)
	if err := CheckSendable(WithRequest(context.Background(), own), sk); err != nil {
		t.Fatalf("own markers refused: %v", err)
	}
	forged := []byte(`{"t":"` + MarkerPrefix + strings.Repeat("ab", 16) + `:00000000~"}`)
	if err := CheckSendable(context.Background(), forged); err != nil {
		t.Fatalf("switch off: dead nonce refused: %v", err)
	}
	unresolved("switch on, foreign marker", CheckSendable(WithRequest(context.Background(), rt.NewRequest("", true)), forged))
	plain := []byte(`{"t":"` + MarkerPrefix + ` text"}`)
	if err := CheckSendable(WithRequest(context.Background(), rt.NewRequest("", true)), plain); err != nil {
		t.Fatalf("plain text refused: %v", err)
	}
}
