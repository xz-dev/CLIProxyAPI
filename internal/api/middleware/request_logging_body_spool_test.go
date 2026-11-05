package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// TestRequestLogTakesSkeletonOnly: with the body spool, the request log and the error-only
// capture record the redacted skeleton; no blob byte and no nonce reaches a log file.
func TestRequestLogTakesSkeletonOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	blob := strings.Repeat("QUJD", 64<<10)
	body := `{"model":"m","input":[{"type":"input_image","image_url":"data:image/png;base64,` + blob + `"},{"type":"input_text","text":"keep me"}]}`
	for _, full := range []bool{true, false} {
		spoolDir, logsDir := t.TempDir(), t.TempDir()
		_ = os.Chmod(spoolDir, 0o700)
		rt, err := bodyspool.NewRuntime(config.BodySpoolConfig{Enabled: true, Dir: spoolDir, DiskReserveBytes: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var nonce string
		router := gin.New()
		router.Use(func(c *gin.Context) {
			sr := rt.NewRequest("", true)
			defer sr.Close()
			c.Set(bodyspool.GinKey, sr)
			c.Next()
		})
		router.Use(RequestLoggingMiddleware(logging.NewFileRequestLogger(full, logsDir, "", 10)))
		router.POST("/v1/responses", func(c *gin.Context) {
			CaptureAuthenticatedRequestBody(c)
			raw, errRead := handlers.ReadRequestBody(c)
			if errRead != nil {
				t.Fatal(errRead)
			}
			i := bytes.Index(raw, []byte(bodyspool.MarkerPrefix))
			nonce = string(raw[i+len(bodyspool.MarkerPrefix) : i+len(bodyspool.MarkerPrefix)+32])
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream failed"})
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		router.ServeHTTP(httptest.NewRecorder(), req)
		var logged []byte
		_ = filepath.Walk(logsDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				b, _ := os.ReadFile(p)
				logged = append(logged, b...)
			}
			return nil
		})
		if !bytes.Contains(logged, []byte("keep me")) || !bytes.Contains(logged, []byte(bodyspool.MarkerPrefix+strings.Repeat("x", 32))) {
			t.Fatalf("full=%v: redacted skeleton not logged (%d bytes)", full, len(logged))
		}
		if bytes.Contains(logged, []byte(blob[:4096])) || nonce == "" || bytes.Contains(logged, []byte(nonce)) {
			t.Fatalf("full=%v: blob bytes or nonce reached the log", full)
		}
	}
}
