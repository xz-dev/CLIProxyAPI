package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool/bodyspooltest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"golang.org/x/net/http2"
)

func spoolDataURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

func spoolResponsesBody() []byte {
	a := spoolDataURL(300 << 10)
	return []byte(fmt.Sprintf(`{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[`+
		`{"type":"input_text","text":"look \u00e9"},{"type":"input_image","image_url":%q}]},`+
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, a, a))
}

// spoolCtx ingests body and returns a context carrying the spool request plus the skeleton.
func spoolCtx(t *testing.T, body []byte) (context.Context, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := bodyspool.NewRuntime(config.BodySpoolConfig{Enabled: true, CgroupDir: bodyspooltest.CgroupDir(t), Dir: dir, DiskReserveBytes: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sk, sp, err := bodyspool.Ingest(bytes.NewReader(body), bodyspool.Options{Dir: rt.Cfg.Dir, Threshold: rt.Cfg.ThresholdBytes, Reserve: 1, Hop: "cpa"})
	if err != nil || sp.Blobs() == 0 {
		t.Fatalf("ingest: %v blobs=%d", err, sp.Blobs())
	}
	sr := rt.NewRequest("", true)
	sr.SetSpool(sp)
	t.Cleanup(sr.Close)
	return bodyspool.WithRequest(t.Context(), sr), sk
}

type capture struct {
	mu     sync.Mutex
	bodies [][]byte
	lens   []int64
}

func (c *capture) server(t *testing.T, reply func(w http.ResponseWriter)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies, c.lens = append(c.bodies, b), append(c.lens, r.ContentLength)
		c.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSendPointsSpliceIdenticalBytes: legacy(body) and splice(skeleton) reach the upstream with
// identical bytes and Content-Length at the codex, claude and openai-compat send points.
func TestSendPointsSpliceIdenticalBytes(t *testing.T) {
	body := spoolResponsesBody()
	cases := map[string]struct {
		exec  cliproxyauth.ProviderExecutor
		auth  func(url string) *cliproxyauth.Auth
		reply func(w http.ResponseWriter)
	}{
		"codex": {NewCodexExecutor(&config.Config{}), codexAPIKeyTestAuth, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"m\",\"output\":[]}}\n\n")
		}},
		"claude": {NewClaudeExecutor(&config.Config{}), func(url string) *cliproxyauth.Auth {
			return &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-test", "base_url": url}}
		}, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}},
		"openai-compat": {NewOpenAICompatExecutor("openai-compatibility", &config.Config{}), func(url string) *cliproxyauth.Auth {
			return &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{"base_url": url + "/v1", "api_key": "k"}}
		}, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c capture
			srv := c.server(t, tc.reply)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body}
			_, _ = tc.exec.Execute(t.Context(), tc.auth(srv.URL), cliproxyexecutor.Request{Model: "m", Payload: body}, opts)
			ctx, sk := spoolCtx(t, body)
			opts.OriginalRequest = sk
			_, _ = tc.exec.Execute(ctx, tc.auth(srv.URL), cliproxyexecutor.Request{Model: "m", Payload: sk}, opts)
			if len(c.bodies) != 2 {
				t.Fatalf("upstream saw %d requests", len(c.bodies))
			}
			if !bytes.Equal(c.bodies[0], c.bodies[1]) || c.lens[0] != c.lens[1] || c.lens[1] != int64(len(c.bodies[1])) {
				t.Fatalf("legacy %d bytes (CL %d) vs spliced %d bytes (CL %d)", len(c.bodies[0]), c.lens[0], len(c.bodies[1]), c.lens[1])
			}
			if bytes.Contains(c.bodies[1], []byte(bodyspool.MarkerPrefix)) || len(c.bodies[1]) < 600<<10 {
				t.Fatalf("marker leaked or blob missing (%d bytes)", len(c.bodies[1]))
			}
		})
	}
}

// TestCodexCompressionRunsOverSplice: codex zstd compression compresses the spliced body into
// the spool; the decoded wire body equals the legacy upstream body.
func TestCodexCompressionRunsOverSplice(t *testing.T) {
	executor := NewCodexExecutor(codexCompressionConfig())
	target := "https://chatgpt.com/backend-api/codex/responses"
	body := spoolResponsesBody()
	_, legacy, err := executor.cacheHelper(t.Context(), sdktranslator.FormatOpenAIResponse, target, cliproxyexecutor.Request{Payload: body}, body, codexCompressionAuth())
	if err != nil {
		t.Fatal(err)
	}
	ctx, sk := spoolCtx(t, body)
	req, upstreamSk, err := executor.cacheHelper(ctx, sdktranslator.FormatOpenAIResponse, target, cliproxyexecutor.Request{Payload: sk}, sk, codexCompressionAuth())
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := io.ReadAll(req.Body)
	if req.Header.Get("Content-Encoding") != "zstd" || req.ContentLength != int64(len(wire)) {
		t.Fatalf("Content-Encoding=%q CL=%d wire=%d", req.Header.Get("Content-Encoding"), req.ContentLength, len(wire))
	}
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	decoded, err := dec.DecodeAll(wire, nil)
	if err != nil || !bytes.Equal(decoded, legacy) {
		t.Fatalf("decoded spliced compression differs from legacy body: %v (%d vs %d)", err, len(decoded), len(legacy))
	}
	if again, _ := req.GetBody(); again == nil {
		t.Fatal("no replayable body")
	} else if b, _ := io.ReadAll(again); !bytes.Equal(b, wire) {
		t.Fatal("replayed compressed body differs")
	}
	if bytes.Contains(upstreamSk, []byte(bodyspool.MarkerPrefix)) != true {
		t.Fatal("returned upstream body should stay the skeleton (logged redacted)")
	}
}

// TestCCHSigningHashesSplice: the CCH of a skeleton equals the CCH of the full body.
func TestCCHSigningHashesSplice(t *testing.T) {
	body := []byte(`{"model":"claude-x","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=00000;"}],` +
		`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("QUJD", 40<<10) + `"}},{"type":"text","text":"hi"}]}]}`)
	want, err := signAnthropicMessagesBody(body)
	if err != nil || bytes.Equal(want, body) {
		t.Fatalf("legacy signing did not sign: %v", err)
	}
	ctx, sk := spoolCtx(t, body)
	sp := bodyspool.SpoolFrom(ctx)
	signed, err := signAnthropicMessagesBodySpliced(sp, sk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sp.Materialize(signed, 0, "test")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("spliced CCH differs from legacy: %v", err)
	}
}

// TestCodexWebsocketSendSplices: the codex WS turn frame carries the spliced body.
func TestCodexWebsocketSendSplices(t *testing.T) {
	body := spoolResponsesBody()
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(64 << 20)
		_, msg, _ := conn.ReadMessage()
		got <- msg
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, sk := spoolCtx(t, body)
	if err := writeCodexWebsocketMessageCtx(ctx, &codexWebsocketSession{}, conn, sk); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !bytes.Equal(msg, body) {
		t.Fatalf("ws frame %d bytes, want %d identical bytes", len(msg), len(body))
	}
}

// TestProvider413SingleAttemptThroughManager: a codex provider 413 on a spooled body is uploaded
// once, never rotated to the second credential, never cools a credential, and is relayed as a 413.
// A payload-size 413 keeps the provider's body unchanged; only a context-length 413 becomes
// context_too_large.
func TestProvider413SingleAttemptThroughManager(t *testing.T) {
	for _, tc := range []struct {
		name, cause, want string
	}{
		{"size", `{"error":{"type":"invalid_request_error","code":"request_too_large","message":"Request exceeds the maximum size"}}`, ""},
		{"size-unknown-code", `{"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`, ""},
		{"context", `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Request exceeds the maximum context length"}}`, "context_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) { provider413ThroughManager(t, tc.name, tc.cause, tc.want) })
	}
}

func provider413ThroughManager(t *testing.T, name, cause, wantCode string) {
	for _, stream := range []bool{false, true} {
		var c capture
		srv := c.server(t, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, cause)
		})
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.SetRetryConfig(3, 0, 3)
		manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))
		reg := registry.GetGlobalRegistry()
		for i := 0; i < 2; i++ {
			auth := codexAPIKeyTestAuth(srv.URL)
			auth.ID = fmt.Sprintf("spool413-%s-%v-%d", name, stream, i)
			reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "m"}})
			id := auth.ID
			t.Cleanup(func() { reg.UnregisterClient(id) })
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
		}
		body := spoolResponsesBody()
		ctx, sk := spoolCtx(t, body)
		req := cliproxyexecutor.Request{Model: "m", Payload: sk}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: sk, Stream: stream}
		var err error
		if stream {
			var res *cliproxyexecutor.StreamResult
			if res, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts); err == nil {
				for chunk := range res.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
		} else {
			_, err = manager.Execute(ctx, []string{"codex"}, req, opts)
		}
		if err == nil || !strings.Contains(err.Error(), "Request exceeds the maximum") || statusFrom(err) != http.StatusRequestEntityTooLarge {
			t.Fatalf("stream=%v: err=%v", stream, err)
		}
		if len(c.bodies) != 1 || len(c.bodies[0]) < 600<<10 {
			t.Fatalf("stream=%v: provider attempts=%d, want exactly 1 full upload", stream, len(c.bodies))
		}
		if wantCode == "" && err.Error() != cause {
			t.Fatalf("stream=%v: body %s, want the provider body unchanged %s", stream, err.Error(), cause)
		}
		if wantCode != "" {
			assertCodexErrorCode(t, err.Error(), "invalid_request_error", wantCode)
		}
		for i := 0; i < 2; i++ {
			auth, ok := manager.GetByID(fmt.Sprintf("spool413-%s-%v-%d", name, stream, i))
			if !ok || auth.Unavailable || auth.Quota.Exceeded || (auth.ModelStates["m"] != nil && auth.ModelStates["m"].Unavailable) {
				t.Fatalf("stream=%v: credential %d cooled down after a provider 413", stream, i)
			}
		}
	}
}

func statusFrom(err error) int {
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	return 0
}

type resetOnceTransport struct {
	mu     sync.Mutex
	bodies [][]byte
}

// RoundTrip reads part of the first upload and fails it with a pre-header HTTP/2 PROTOCOL_ERROR,
// then answers every later attempt (each attempt re-reads the spool).
func (rt *resetOnceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	first := len(rt.bodies) == 0
	rt.mu.Unlock()
	if first {
		_, _ = io.CopyN(io.Discard, req.Body, 4096)
		rt.mu.Lock()
		rt.bodies = append(rt.bodies, nil)
		rt.mu.Unlock()
		return nil, http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol, Cause: errors.New("received from peer")}
	}
	b, _ := io.ReadAll(req.Body)
	rt.mu.Lock()
	rt.bodies = append(rt.bodies, b)
	rt.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: req,
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"m\",\"output\":[]}}\n\n"))}, nil
}

type fixedRT struct{ rt http.RoundTripper }

func (f fixedRT) RoundTripperFor(*cliproxyauth.Auth) http.RoundTripper { return f.rt }

// TestH2ResetRetryReSplicesSpool: a pre-header H2 reset is retried within the request-retry budget,
// the retry re-reads the spool, and the delivered body equals the legacy body.
func TestH2ResetRetryReSplicesSpool(t *testing.T) {
	body := spoolResponsesBody()
	run := func(ctx context.Context, payload []byte, id string) *resetOnceTransport {
		rt := &resetOnceTransport{}
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.SetRetryConfig(1, 0, 0)
		manager.SetRoundTripperProvider(fixedRT{rt})
		manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))
		auth := codexAPIKeyTestAuth("https://upstream.invalid")
		auth.ID = id
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "m"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
		if _, err := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "m", Payload: payload}, opts); err != nil {
			t.Fatal(err)
		}
		return rt
	}
	legacy := run(context.Background(), body, "h2-legacy")
	ctx, sk := spoolCtx(t, body)
	spooled := run(ctx, sk, "h2-spooled")
	if len(spooled.bodies) != 2 || len(legacy.bodies) != 2 || !bytes.Equal(spooled.bodies[1], legacy.bodies[1]) || len(spooled.bodies[1]) < 600<<10 {
		t.Fatalf("attempts spooled=%d legacy=%d; retried bodies differ", len(spooled.bodies), len(legacy.bodies))
	}
}

type repeatReader struct{ n int64 }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := int(min(int64(len(p)), r.n))
	for i := range p[:k] {
		p[i] = 'A'
	}
	r.n -= int64(k)
	return k, nil
}

// TestClaudeFirstPartyCeiling: a body above Anthropic's 128,000,000-byte ceiling gets the named
// 413 (limit and route) and nothing is sent; other claude routes keep the 256 MiB cap.
func TestClaudeFirstPartyCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 128 MB spool")
	}
	head := `{"model":"claude-x","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`
	src := io.MultiReader(strings.NewReader(head), &repeatReader{n: claudeFirstPartyCeiling}, strings.NewReader(`"}}]}]}`))
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	rt, err := bodyspool.NewRuntime(config.BodySpoolConfig{Enabled: true, CgroupDir: bodyspooltest.CgroupDir(t), Dir: dir, DiskReserveBytes: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sk, sp, err := bodyspool.Ingest(src, bodyspool.Options{Dir: dir, Threshold: 64 << 10, Reserve: 1, Hop: "cpa"})
	if err != nil {
		t.Fatal(err)
	}
	sr := rt.NewRequest("", true)
	sr.SetSpool(sp)
	defer sr.Close()
	ctx := bodyspool.WithRequest(t.Context(), sr)
	var e *bodyspool.Error
	if err := claudeProviderCeiling(ctx, "https://api.anthropic.com", sk); !errors.As(err, &e) || e.Status != 413 || e.Limit != claudeFirstPartyCeiling || !strings.Contains(e.Message, "Anthropic first-party") {
		t.Fatalf("first-party: %v", err)
	}
	if err := claudeProviderCeiling(ctx, "https://claude-relay.example", sk); err != nil {
		t.Fatalf("other claude route must keep the cap: %v", err)
	}
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-test", "base_url": "https://api.anthropic.com"}}
	_, err = NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-x", Payload: sk}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: sk})
	if !errors.As(err, &e) || e.Status != 413 {
		t.Fatalf("executor did not refuse before sending: %v", err)
	}
}
