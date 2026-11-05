package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool/bodyspooltest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// readBody runs ReadRequestBody on body with the given encoding and decoded-size limit, through
// the spool (materialized) when spool is set, else through the legacy path.
func readBody(t *testing.T, spool bool, body []byte, encoding string, maxBytes int64) ([]byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Encoding", encoding)
	c.Set(MaxRequestBodyBytesContextKey, maxBytes)
	if !spool {
		return ReadRequestBody(c)
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	rt, err := bodyspool.NewRuntime(config.BodySpoolConfig{Enabled: true, CgroupDir: bodyspooltest.CgroupDir(t), Dir: dir, DiskReserveBytes: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sr := rt.NewRequest("", true)
	t.Cleanup(sr.Close)
	c.Set(bodyspool.GinKey, sr)
	skel, err := ReadRequestBody(c)
	if err != nil {
		return nil, err
	}
	return sr.Spool().Materialize(skel, 0, "test")
}

// TestSpoolDecodesLikeLegacy: the spool ingest decodes Content-Encoding lists the way the legacy
// path does and caps the decoded zstd body at the request body limit (m1).
func TestSpoolDecodesLikeLegacy(t *testing.T) {
	media := `{"model":"m","input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("QUJD", 40<<10) + `"}]}`
	z := zstdBytes(t, []byte(media))
	for _, tc := range []struct {
		name, encoding string
		body           []byte
		maxBytes       int64
		wantMaxBytes   bool
	}{
		{"zstd", "zstd", z, 1 << 20, false},
		{"zstd over limit", "zstd", z, 64 << 10, true},
		{"zstd twice", "zstd, zstd", zstdBytes(t, z), 1 << 20, false},
		{"zstd twice over limit", "zstd,zstd", zstdBytes(t, z), 64 << 10, true},
		{"identity list", "identity, zstd", z, 1 << 20, false},
		{"upper case", "ZSTD", z, 1 << 20, false},
		{"mislabeled json", "gzip", []byte(media), 1 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, errLegacy := readBody(t, false, tc.body, tc.encoding, tc.maxBytes)
			got, err := readBody(t, true, tc.body, tc.encoding, tc.maxBytes)
			var mb *http.MaxBytesError
			if tc.wantMaxBytes {
				if !errors.As(errLegacy, &mb) || !errors.As(err, &mb) || mb.Limit != tc.maxBytes {
					t.Fatalf("legacy err=%v spool err=%v, want MaxBytesError(%d) on both", errLegacy, err, tc.maxBytes)
				}
				return
			}
			if errLegacy != nil || err != nil || !bytes.Equal(got, legacy) || string(got) != media {
				t.Fatalf("legacy err=%v spool err=%v, spool %d bytes vs legacy %d", errLegacy, err, len(got), len(legacy))
			}
		})
	}
	// Not decodable and not JSON: both refuse.
	if _, err := readBody(t, true, []byte("\x1f\x8b junk"), "gzip", 1<<20); err == nil {
		t.Fatal("spool accepted an undecodable body")
	}
}
