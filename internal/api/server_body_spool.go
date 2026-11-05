package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var bodySpoolRuntime atomic.Pointer[bodyspool.Runtime]

// ErrBodySpoolStartup marks a server start refused by the body-spool configuration; the process
// exits non-zero on it.
var ErrBodySpoolStartup = errors.New("body spool configuration refused")

// applyBodySpool validates and activates the body-spool configuration. At startup an error
// refuses to start; on reload the previous runtime stays active and the error is returned.
func applyBodySpool(cfg *config.Config) error {
	var bs config.BodySpoolConfig
	if cfg != nil {
		bs = cfg.BodySpool
	}
	rt, err := bodyspool.NewRuntime(bs, bodySpoolRuntime.Load())
	if err != nil {
		return err
	}
	bodySpoolRuntime.Store(rt)
	return nil
}

// bodySpoolCoveredPath reports whether a route ingests into the spool (the covered set, D2).
// /responses/compact and every other route are fallback paths bounded by L.
func bodySpoolCoveredPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/responses", "/backend-api/codex/responses", "/v1/chat/completions":
		return true
	}
	return false
}

func bodySpoolManagementPath(path string) bool {
	return strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/v8/management") || strings.HasPrefix(path, "/management")
}

// bodySpoolMiddleware consumes the capacity ticket header (it never reaches a provider), creates
// the per-request spool state, bounds fallback paths by L, and releases spool, charges and lease
// when the handler returns.
func (s *Server) bodySpoolMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			c.Next()
			return
		}
		ticket := strings.TrimSpace(c.Request.Header.Get(bodyspool.TicketHeader))
		c.Request.Header.Del(bodyspool.TicketHeader)
		rt := bodySpoolRuntime.Load()
		if rt == nil {
			c.Next()
			return
		}
		path := ""
		if c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		covered := bodySpoolCoveredPath(c.Request.Method, path)
		sr := rt.NewRequest(ticket, covered)
		defer sr.Close()
		c.Set(bodyspool.GinKey, sr)
		hasBody := c.Request.Body != nil && c.Request.Body != http.NoBody && (c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) > 0)
		if !covered && hasBody && !bodySpoolManagementPath(path) {
			limit := rt.Cfg.FallbackLimitBytes
			if c.Request.ContentLength > limit {
				refuseBodySpool(c, bodyspool.FallbackTooLarge(bodyspool.Hop, path, c.Request.ContentLength, limit))
				return
			}
			c.Request.Body = &fallbackBody{ReadCloser: c.Request.Body, sr: sr, path: path, limit: limit}
		}
		c.Next()
	}
}

// releaseLargeRequestState must be the outermost middleware. gin keeps a finished request's
// Context in a sync.Pool until another request reuses it, so its Keys (the request-log bodies and
// upstream request copies, the spool state), its Request (the request context with the executor
// values) and its Writer chain stay reachable through one more GC after the request. After a
// request whose released charge was at least bodyspool.ReclaimAfterRelease, once every other
// middleware has returned, it drops those references and returns the freed heap to the OS
// (Charger.ReclaimReleased). Smaller requests are left as they are.
func releaseLargeRequestState() gin.HandlerFunc {
	return func(c *gin.Context) {
		w := c.Writer // &c.writermem: inner middlewares wrap it
		c.Next()
		v, _ := c.Get(bodyspool.GinKey)
		sr, _ := v.(*bodyspool.Request)
		if sr == nil || sr.ReleasedBytes() < bodyspool.ReclaimAfterRelease {
			return
		}
		if c.Request != nil {
			r := c.Request.WithContext(context.Background())
			r.Body, r.GetBody = http.NoBody, nil
			c.Request = r
		}
		// gin itself clears Keys without a lock when it reuses the Context, so nothing may read
		// them after the handler chain has returned.
		c.Keys = nil
		c.Writer = w
		if sr.RT != nil {
			sr.RT.Charger.ReclaimReleased()
		}
	}
}

func refuseBodySpool(c *gin.Context, e *bodyspool.Error) {
	closeUnconsumedRequestOnAbort(c)
	c.Abort()
	c.Header("Connection", "close")
	c.Data(e.Status, "application/json", []byte(e.Error()))
}

// fallbackBody bounds a fallback path's body by L (named 413) and charges k_path per byte read,
// in ChargeStep steps taken before the bytes are handed to the handler.
type fallbackBody struct {
	io.ReadCloser
	sr      *bodyspool.Request
	path    string
	limit   int64
	n       int64
	charged int64
	err     error
}

func (b *fallbackBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		b.err = bodyspool.FallbackTooLarge(bodyspool.Hop, b.path, b.n, b.limit)
		return 0, b.err
	}
	for b.n > b.charged {
		if errCharge := b.sr.Charge(bodyspool.Scaled(b.sr.RT.Cfg.KPath, bodyspool.ChargeStep)); errCharge != nil {
			b.err = errCharge
			return 0, b.err
		}
		b.charged += bodyspool.ChargeStep
	}
	return n, err
}
