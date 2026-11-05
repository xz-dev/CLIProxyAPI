package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool/bodyspooltest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type releaseTestKey struct{}

// After a large request, the references gin's pooled Context still holds (Keys, the request
// context) must be gone one GC after the request, and the freed heap returned once. A small
// request triggers no release.
func TestReleaseLargeRequestStateDropsPooledReferences(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	cfg := &config.Config{}
	cfg.BodySpool = config.BodySpoolConfig{Enabled: true, CgroupDir: bodyspooltest.CgroupDir(t), Dir: dir, LedgerDir: t.TempDir(), DiskReserveBytes: 1}
	if err := applyBodySpool(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bodySpoolRuntime.Store(nil) })
	reclaims := make(chan struct{}, 4)
	bodySpoolRuntime.Load().Charger.ReleaseReclaim = func() { reclaims <- struct{}{} }

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(releaseLargeRequestState())
	engine.Use((&Server{}).bodySpoolMiddleware())
	var wKeys, wCtx weak.Pointer[byte]
	engine.POST("/v1/responses", func(c *gin.Context) {
		sr := c.MustGet(bodyspool.GinKey).(*bodyspool.Request)
		n := bodyspool.ReclaimAfterRelease
		if c.Query("small") != "" {
			n = bodyspool.ChargeStep
		}
		if err := sr.Charge(n); err != nil {
			t.Error(err)
		}
		inKeys, inCtx := make([]byte, 4<<20), make([]byte, 4<<20)
		wKeys, wCtx = weak.Make(&inKeys[0]), weak.Make(&inCtx[0])
		c.Set("request-copy", inKeys)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), releaseTestKey{}, inCtx))
		c.String(http.StatusOK, "ok")
	})
	serve := func(path string) {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}

	serve("/v1/responses?small=1")
	if len(reclaims) != 0 {
		t.Fatalf("a %d-byte request triggered a release", bodyspool.ChargeStep)
	}

	serve("/v1/responses")
	runtime.GC() // one GC: the pooled Context survives it in the pool's victim cache
	if wKeys.Value() != nil || wCtx.Value() != nil {
		t.Fatalf("pooled Context still holds the request: keys=%v ctx=%v", wKeys.Value() != nil, wCtx.Value() != nil)
	}
	<-reclaims // the release ran once for the large request
}
