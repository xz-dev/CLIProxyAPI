package bodyspool

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Hop names CPA in every explicit error.
const Hop = "cpa"

// PageCacheBound is P_h, the per-request page-cache term of the ticketed charge (D3/D4).
const PageCacheBound = 16 << 20

// ChargeStep is the inline-byte step of the memory charge.
const ChargeStep = 1 << 20

// GinKey stores the *Request of a request on the Gin context.
const GinKey = "CPA_BODYSPOOL_REQUEST"

// Runtime is the active body-spool configuration plus the process-wide memory charge.
type Runtime struct {
	Cfg     config.BodySpoolConfig // effective (defaults applied)
	Charger *Charger
}

// NewRuntime validates cfg (CheckDir, readable cgroup memory files) and returns the runtime, or nil when the switch is off.
// The charge counter carries over from prev so in-flight charges stay accounted across reloads.
func NewRuntime(cfg config.BodySpoolConfig, prev *Runtime) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	eff := cfg.Effective()
	if err := CheckDir(eff.Dir); err != nil {
		return nil, err
	}
	ch := &Charger{CgroupDir: eff.CgroupDir, Hop: Hop}
	if err := ch.Check(); err != nil {
		return nil, err
	}
	if prev != nil && prev.Charger != nil && prev.Charger.CgroupDir == eff.CgroupDir {
		ch = prev.Charger
	}
	return &Runtime{Cfg: eff, Charger: ch}, nil
}

// Scaled returns ceil(k × n) bytes.
func Scaled(k float64, n int64) int64 { return int64(math.Ceil(k * float64(n))) }

// Request is the per-request spool state: ticket lease, spool and memory charges. The middleware
// that creates it calls Close when the handler returns.
type Request struct {
	RT       *Runtime
	TicketID string
	Covered  bool // the route ingests into the spool (otherwise a fallback path bounded by L)

	mu      sync.Mutex
	lease   *Lease
	spool   *Spool
	account *Account
}

// NewRequest starts the state of one request.
func (rt *Runtime) NewRequest(ticketID string, covered bool) *Request {
	return &Request{RT: rt, TicketID: ticketID, Covered: covered, account: rt.Charger.NewAccount()}
}

// Charge charges n bytes against the hop's memory for this request.
func (r *Request) Charge(n int64) error { return r.account.Charge(n) }

// SetLease records the ticket lease held for the handler lifetime.
func (r *Request) SetLease(l *Lease) { r.mu.Lock(); r.lease = l; r.mu.Unlock() }

// Ticketed reports whether a ticket lease is held.
func (r *Request) Ticketed() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.lease != nil }

// SetSpool records the request's spool.
func (r *Request) SetSpool(sp *Spool) { r.mu.Lock(); r.spool = sp; r.mu.Unlock() }

// Spool returns the request's spool (nil when nothing was spooled).
func (r *Request) Spool() *Spool {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spool
}

// CloseSpool frees the spool's blocks (before an error status is committed, D2).
func (r *Request) CloseSpool() {
	if sp := r.Spool(); sp != nil {
		_ = sp.Close()
	}
}

// Close closes the spool, releases the memory charges, then drops the ticket lease.
func (r *Request) Close() {
	if r == nil {
		return
	}
	r.CloseSpool()
	r.account.Release()
	r.mu.Lock()
	l := r.lease
	r.lease = nil
	r.mu.Unlock()
	l.Release()
}

// Fallback returns the full body for a path that is not converted (D2): skel itself when it
// carries no marker, the materialized body charged at k_path × N when it fits L, otherwise the
// named 413 for path.
func (r *Request) Fallback(skel []byte, path string) ([]byte, error) {
	sp := r.Spool()
	if !sp.Has(skel) {
		return skel, nil
	}
	n, err := sp.Size(skel)
	if err != nil {
		return nil, err
	}
	if n > r.RT.Cfg.FallbackLimitBytes {
		return nil, FallbackTooLarge(Hop, path, n, r.RT.Cfg.FallbackLimitBytes)
	}
	if err := r.Charge(Scaled(r.RT.Cfg.KPath, n)); err != nil {
		return nil, err
	}
	return sp.Materialize(skel, r.RT.Cfg.FallbackLimitBytes, path)
}

type ctxKey struct{}

// WithRequest attaches r to ctx.
func WithRequest(ctx context.Context, r *Request) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext returns the request state attached to ctx, or nil.
func FromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(ctxKey{}).(*Request)
	return r
}

// SpoolFrom returns the spool attached to ctx, or nil.
func SpoolFrom(ctx context.Context) *Spool { return FromContext(ctx).Spool() }

// CheckSendable fails closed (D2: markers never leave the hop) with the explicit 500
// marker_unresolved when body carries a marker that the request's spool will not splice: a marker
// of a live spool other than the request's own, or, while the switch is on for the request, any
// marker-shaped token that is not the request's own. Every send path calls it before writing.
func CheckSendable(ctx context.Context, body []byte) error {
	prefix := []byte(MarkerPrefix)
	if !bytes.Contains(body, prefix) {
		return nil
	}
	req := FromContext(ctx)
	sp := req.Spool()
	own := sp.Has(body)
	for rest := body; ; {
		i := bytes.Index(rest, prefix)
		if i < 0 {
			return nil
		}
		rest = rest[i+len(prefix):]
		if len(rest) < nonceHex || !isHex(rest[:nonceHex]) {
			continue
		}
		nonce := string(rest[:nonceHex])
		if own && nonce == sp.nonce {
			continue // resolved by the splice reader (which validates the index)
		}
		if _, live := registry.Load(nonce); live || req != nil {
			return &Error{Status: 500, Code: "marker_unresolved", Hop: Hop,
				Message: "request body carries a spool marker that this request's spool cannot resolve; nothing was sent upstream"}
		}
	}
}

func isHex(b []byte) bool {
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewHTTPRequest builds an upstream request. A body carrying this request's markers is sent as a
// splice stream with an exact Content-Length and a replayable GetBody; any other body is sent as
// is. A body with markers no spool resolves is refused (CheckSendable).
func NewHTTPRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	if err := CheckSendable(ctx, body); err != nil {
		return nil, err
	}
	sp := SpoolFrom(ctx)
	if !sp.Has(body) {
		return http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	}
	r, n, err := sp.Reader(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) {
		again, _, errAgain := sp.Reader(body)
		return io.NopCloser(again), errAgain
	}
	return req, nil
}

// SplicedSize returns the upstream length of body (its spliced length when it carries markers).
func SplicedSize(ctx context.Context, body []byte) (int64, error) {
	sp := SpoolFrom(ctx)
	if !sp.Has(body) {
		return int64(len(body)), nil
	}
	return sp.Size(body)
}

// WriteSpliced writes the spliced view of body to w.
func WriteSpliced(ctx context.Context, w io.Writer, body []byte) error {
	if err := CheckSendable(ctx, body); err != nil {
		return err
	}
	sp := SpoolFrom(ctx)
	if !sp.Has(body) {
		_, err := w.Write(body)
		return err
	}
	r, _, err := sp.Reader(body)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}
