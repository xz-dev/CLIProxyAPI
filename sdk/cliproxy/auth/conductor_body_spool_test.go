package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func spoolRequestCtx(t *testing.T, body string, fallback int64) (context.Context, []byte) {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	rt, err := bodyspool.NewRuntime(internalconfig.BodySpoolConfig{Enabled: true, Dir: dir, DiskReserveBytes: 1, FallbackLimitBytes: fallback}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sk, sp, err := bodyspool.Ingest(strings.NewReader(body), bodyspool.Options{Dir: dir, Threshold: 64 << 10, Reserve: 1, Hop: "cpa"})
	if err != nil || sp.Blobs() == 0 {
		t.Fatalf("ingest: %v", err)
	}
	sr := rt.NewRequest("", true)
	sr.SetSpool(sp)
	t.Cleanup(sr.Close)
	return bodyspool.WithRequest(context.Background(), sr), sk
}

func registerSpoolAuths(t *testing.T, m *Manager, provider string, n int) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for i := 0; i < n; i++ {
		a := &Auth{ID: provider + "-spool-" + string(rune('a'+i)), Provider: provider, Status: StatusActive}
		reg.RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: "m"}})
		id := a.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
}

type splicingMock struct{ mockCustomErrorExecutor }

func (splicingMock) SplicesBodySpool() bool { return true }

var spoolBody = `{"model":"m","input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("QUJD", 75<<10) + `"}]}`

// TestUnconvertedExecutorIsFallbackPath: an executor that does not splice gets the full body
// within L, and the named 413 (no attempt) above L; a splicing executor keeps the skeleton.
func TestUnconvertedExecutorIsFallbackPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		splices  bool
		fallback int64
		wantErr  bool
	}{
		{"unconverted within L", false, 16 << 20, false},
		{"unconverted above L", false, 64 << 10, true},
		{"converted", true, 64 << 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			var got [][]byte
			base := mockCustomErrorExecutor{identifier: "spoolmock", executeFn: func(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				got = append(got, req.Payload)
				return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
			}}
			if tc.splices {
				m.RegisterExecutor(&splicingMock{base})
			} else {
				m.RegisterExecutor(&base)
			}
			registerSpoolAuths(t, m, "spoolmock", 1)
			ctx, sk := spoolRequestCtx(t, spoolBody, tc.fallback)
			_, err := m.Execute(ctx, []string{"spoolmock"}, cliproxyexecutor.Request{Model: "m", Payload: sk}, cliproxyexecutor.Options{OriginalRequest: sk})
			var e *bodyspool.Error
			switch {
			case tc.wantErr:
				if !errors.As(err, &e) || e.Status != 413 || e.Limit != tc.fallback || !strings.Contains(e.Message, "executor spoolmock") || len(got) != 0 {
					t.Fatalf("err=%v attempts=%d", err, len(got))
				}
			case tc.splices:
				if err != nil || len(got) != 1 || !bytes.Equal(got[0], sk) {
					t.Fatalf("converted executor: err=%v", err)
				}
			default:
				if err != nil || len(got) != 1 || string(got[0]) != spoolBody {
					t.Fatalf("unconverted executor did not get the full body: err=%v", err)
				}
			}
		})
	}
}

// TestProvider413IsRequestScoped: a provider 413 gets exactly one provider attempt, no
// credential rotation and no cooldown, and the provider's cause is relayed unchanged.
func TestProvider413IsRequestScoped(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 0, 3)
	attempts := 0
	cause := `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`
	m.RegisterExecutor(&splicingMock{mockCustomErrorExecutor{identifier: "spool413", executeFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		attempts++
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		return cliproxyexecutor.Response{}, customStatusError{code: 413, msg: cause}
	}}})
	registerSpoolAuths(t, m, "spool413", 3)
	ctx, sk := spoolRequestCtx(t, spoolBody, 16<<20)
	_, err := m.Execute(ctx, []string{"spool413"}, cliproxyexecutor.Request{Model: "m", Payload: sk}, cliproxyexecutor.Options{OriginalRequest: sk})
	if err == nil || statusCodeFromError(err) != 413 || err.Error() != cause {
		t.Fatalf("err=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1", attempts)
	}
	for _, a := range m.List() {
		if a.Unavailable || !a.NextRetryAfter.IsZero() {
			t.Fatalf("credential %s penalized after a provider 413", a.ID)
		}
	}
}
