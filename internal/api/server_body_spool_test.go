package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"golang.org/x/sys/unix"
)

type spoolSeen struct {
	ticketHeader string
	skeleton     []byte
	blobs        int
	leaseHeld    bool
}

func spoolEngine(t *testing.T, ledger string) (*gin.Engine, *spoolSeen) {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	cfg := &config.Config{}
	cfg.BodySpool = config.BodySpoolConfig{Enabled: true, Dir: dir, LedgerDir: ledger, DiskReserveBytes: 1}
	if err := applyBodySpool(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bodySpoolRuntime.Store(nil) })
	gin.SetMode(gin.TestMode)
	seen := &spoolSeen{}
	engine := gin.New()
	engine.Use((&Server{}).bodySpoolMiddleware())
	h := func(c *gin.Context) {
		raw, err := handlers.ReadRequestBody(c)
		if handlers.WriteBodySpoolError(c, err) {
			return
		}
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		seen.ticketHeader = c.Request.Header.Get(bodyspool.TicketHeader)
		seen.skeleton = raw
		if v, ok := c.Get(bodyspool.GinKey); ok {
			sr := v.(*bodyspool.Request)
			seen.blobs = sr.Spool().Blobs()
			seen.leaseHeld = sr.Ticketed()
		}
		c.String(http.StatusOK, "ok")
	}
	engine.POST("/v1/responses", h)
	engine.POST("/v1/responses/compact", h)
	return engine, seen
}

func post(engine *gin.Engine, path string, body io.Reader, size int64, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.ContentLength = size
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error struct{ Code, Hop string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d: non-JSON error body %q", w.Code, w.Body.String())
	}
	return body.Error.Code, body.Error.Hop
}

func TestBodySpoolRouteContracts(t *testing.T) {
	ledger := t.TempDir()
	id := strings.Repeat("0f", 16)
	if err := os.WriteFile(filepath.Join(ledger, id+bodyspool.TicketSuffix), []byte("n\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	engine, seen := spoolEngine(t, ledger)
	img := `{"input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("QUJD", 64<<10) + `"}]}`

	w := post(engine, "/v1/responses", strings.NewReader(img), int64(len(img)), map[string]string{bodyspool.TicketHeader: id})
	if w.Code != http.StatusOK || seen.ticketHeader != "" || seen.blobs != 1 || !seen.leaseHeld || !bytes.Contains(seen.skeleton, []byte(bodyspool.MarkerPrefix)) {
		t.Fatalf("covered route: code=%d header=%q blobs=%d lease=%v", w.Code, seen.ticketHeader, seen.blobs, seen.leaseHeld)
	}
	fd, _ := unix.Open(filepath.Join(ledger, id+bodyspool.TicketSuffix), unix.O_RDONLY, 0)
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lease still held after the handler returned: %v", err)
	}
	_ = unix.Flock(fd, unix.LOCK_UN)

	big := int64(bodyspool.TicketThreshold)
	for name, size := range map[string]int64{"content-length": big, "chunked": -1} {
		media := io.MultiReader(strings.NewReader(`{"image_url":"`), io.LimitReader(zeroReader{}, big))
		w = post(engine, "/v1/responses", media, size, nil)
		if code, hop := errCode(t, w); w.Code != 503 || code != "capacity_ticket_missing" || hop != "cpa" {
			t.Fatalf("%s 64 MiB without ticket: %d %s", name, w.Code, code)
		}
	}

	text := `{"input":"` + strings.Repeat("t", 17<<20) + `"}`
	w = post(engine, "/v1/responses", strings.NewReader(text), int64(len(text)), nil)
	if code, _ := errCode(t, w); w.Code != 413 || code != "request_too_large" || !strings.Contains(w.Body.String(), "inline-bytes limit of 16777216") {
		t.Fatalf("inline limit: %d %s", w.Code, w.Body.String())
	}

	for name, size := range map[string]int64{"content-length": int64(len(text)), "chunked": -1} {
		w = post(engine, "/v1/responses/compact", strings.NewReader(text), size, nil)
		if code, _ := errCode(t, w); w.Code != 413 || code != "request_too_large" || !strings.Contains(w.Body.String(), "fallback limit of /v1/responses/compact") {
			t.Fatalf("%s compact above L: %d %s", name, w.Code, w.Body.String())
		}
	}
	small := `{"input":"hi"}`
	if w = post(engine, "/v1/responses/compact", strings.NewReader(small), int64(len(small)), nil); w.Code != 200 || string(seen.skeleton) != small {
		t.Fatalf("compact below L: %d", w.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}
