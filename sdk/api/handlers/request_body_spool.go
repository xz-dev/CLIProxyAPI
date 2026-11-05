package handlers

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
)

// BodySpoolLogBodyKey stores, on the Gin context, the function that hands the redacted skeleton
// to the request log (request-log tee and error capture never see blob bytes).
const BodySpoolLogBodyKey = "CPA_BODYSPOOL_LOG_BODY"

const defaultSpoolCap = 256 << 20

func bodySpoolRequest(c *gin.Context) *bodyspool.Request {
	if c == nil {
		return nil
	}
	v, ok := c.Get(bodyspool.GinKey)
	if !ok {
		return nil
	}
	r, _ := v.(*bodyspool.Request)
	return r
}

// readSpooledRequestBody ingests a covered request into the body spool (D2): ticket lease first,
// then the ticketed charge, then a streaming skeleton read bounded by the inline-bytes limit.
func readSpooledRequestBody(c *gin.Context, sr *bodyspool.Request) ([]byte, error) {
	cfg := sr.RT.Cfg
	size := c.Request.ContentLength
	lease, err := bodyspool.AcquireLease(cfg.LedgerDir, sr.TicketID, size, bodyspool.Hop)
	if err != nil {
		return nil, err
	}
	sr.SetLease(lease)
	inline := cfg.InlineLimitBytes
	if lease != nil {
		inline = cfg.TicketedInlineLimitBytes
		n := size
		if n < 0 {
			if n = c.GetInt64(MaxRequestBodyBytesContextKey); n <= 0 {
				n = defaultSpoolCap
			}
		}
		if err := sr.Charge(bodyspool.Scaled(cfg.KMem, n) + bodyspool.PageCacheBound); err != nil {
			return nil, err
		}
	}
	var body io.Reader = http.NoBody
	if c.Request.Body != nil {
		body = c.Request.Body
	}
	body, closeDecoder, err := spoolDecodedBody(body, c.Request.Header.Get("Content-Encoding"), c.GetInt64(MaxRequestBodyBytesContextKey))
	if err != nil {
		return nil, err
	}
	defer closeDecoder()
	skel, sp, err := bodyspool.Ingest(&ticketGate{r: body, leased: lease != nil}, bodyspool.Options{
		Dir: cfg.Dir, Threshold: cfg.ThresholdBytes, InlineLimit: inline, Reserve: cfg.DiskReserveBytes, Hop: bodyspool.Hop,
		ChargeStep: bodyspool.ChargeStep, Charge: func(n int64) error { return sr.Charge(bodyspool.Scaled(cfg.KSkel, n)) },
	})
	if err != nil {
		return nil, err
	}
	sr.SetSpool(sp)
	if v, ok := c.Get(BodySpoolLogBodyKey); ok {
		if set, okSet := v.(func([]byte)); okSet && set != nil {
			set(sp.Redact(skel))
		}
	}
	c.Writer = &spoolCommitWriter{ResponseWriter: c.Writer, sr: sr}
	return skel, nil
}

// spoolDecodedBody streams a body through one zstd decoder per zstd token of the
// Content-Encoding list, each capped at maxBytes like the legacy decodeRequestBody. A body that
// cannot be decoded but looks like JSON is read raw, matching the legacy fallback for mislabeled
// JSON.
func spoolDecodedBody(r io.Reader, encoding string, maxBytes int64) (io.Reader, func(), error) {
	var tokens []string
	for _, part := range strings.Split(encoding, ",") {
		if enc := strings.ToLower(strings.TrimSpace(part)); enc != "" && enc != "identity" {
			tokens = append(tokens, enc)
		}
	}
	if len(tokens) == 0 {
		return r, func() {}, nil
	}
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	unsupported := ""
	for _, enc := range tokens {
		if enc != "zstd" {
			unsupported = enc
			break
		}
	}
	if unsupported == "" && bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		var cur io.Reader = br
		var decs []*zstd.Decoder
		closeAll := func() {
			for _, d := range decs {
				d.Close()
			}
		}
		for range tokens {
			dec, err := zstd.NewReader(cur, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
			if err != nil {
				closeAll()
				return nil, nil, err
			}
			decs = append(decs, dec)
			cur = dec
			if maxBytes > 0 && maxBytes < math.MaxInt64 {
				cur = &decodedCap{r: dec, left: maxBytes, limit: maxBytes}
			}
		}
		return cur, closeAll, nil
	}
	if first := bytes.TrimLeft(magic, " \t\r\n"); len(first) > 0 && (first[0] == '{' || first[0] == '[') {
		return br, func() {}, nil
	}
	if unsupported == "" {
		return nil, nil, errors.New("failed to decode zstd request body: missing zstd magic")
	}
	return nil, nil, errors.New("unsupported request content encoding: " + unsupported)
}

// decodedCap fails with *http.MaxBytesError once more than limit decoded bytes are read.
type decodedCap struct {
	r     io.Reader
	left  int64
	limit int64
}

func (d *decodedCap) Read(p []byte) (int, error) {
	if d.left < 0 {
		return 0, &http.MaxBytesError{Limit: d.limit}
	}
	if int64(len(p)) > d.left+1 {
		p = p[:d.left+1]
	}
	n, err := d.r.Read(p)
	if d.left -= int64(n); d.left < 0 {
		return 0, &http.MaxBytesError{Limit: d.limit}
	}
	return n, err
}

// ticketGate refuses a ticketless body when its received bytes cross 64 MiB (D4).
type ticketGate struct {
	r      io.Reader
	n      int64
	leased bool
}

func (g *ticketGate) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.n += int64(n)
	if !g.leased && g.n >= bodyspool.TicketThreshold {
		return 0, bodyspool.TicketMissing(bodyspool.Hop, g.n)
	}
	return n, err
}

// spoolCommitWriter closes the spool before an error status is committed to the caller, so the
// caller's retry never overlaps this hop's spool (D2). Success responses close it when the
// handler returns, before the response is finished.
type spoolCommitWriter struct {
	gin.ResponseWriter
	sr *bodyspool.Request
}

func (w *spoolCommitWriter) beforeCommit() {
	if !w.ResponseWriter.Written() && w.ResponseWriter.Status() >= http.StatusBadRequest {
		w.sr.CloseSpool()
	}
}

func (w *spoolCommitWriter) WriteHeaderNow() { w.beforeCommit(); w.ResponseWriter.WriteHeaderNow() }

func (w *spoolCommitWriter) Write(p []byte) (int, error) {
	w.beforeCommit()
	return w.ResponseWriter.Write(p)
}

func (w *spoolCommitWriter) WriteString(s string) (int, error) {
	w.beforeCommit()
	return w.ResponseWriter.WriteString(s)
}

func (w *spoolCommitWriter) Flush() { w.beforeCommit(); w.ResponseWriter.Flush() }

// WriteBodySpoolError writes an explicit body-spool error (named 413/503/507/500 in OpenAI shape)
// and reports whether err was one.
func WriteBodySpoolError(c *gin.Context, err error) bool {
	var e *bodyspool.Error
	if !errors.As(err, &e) {
		return false
	}
	if sr := bodySpoolRequest(c); sr != nil {
		sr.CloseSpool()
	}
	c.Header("Connection", "close")
	c.Data(e.Status, "application/json", []byte(e.Error()))
	return true
}
