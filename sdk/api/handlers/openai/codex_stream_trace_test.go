package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

type responsesTraceLog struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	completed chan struct{}
	once      sync.Once
}

func (b *responsesTraceLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if strings.Contains(string(p), "codex stream trace") {
		_, _ = b.buf.Write(p)
		if strings.Contains(string(p), "stage=executor_completed") {
			b.once.Do(func() { close(b.completed) })
		}
	}
	return len(p), nil
}

func (b *responsesTraceLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureResponsesTrace(t *testing.T) *responsesTraceLog {
	t.Helper()
	buf := &responsesTraceLog{completed: make(chan struct{})}
	prev, level, formatter := log.StandardLogger().Out, log.GetLevel(), log.StandardLogger().Formatter
	log.SetOutput(buf)
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&logging.LogFormatter{})
	t.Cleanup(func() { log.SetOutput(prev); log.SetLevel(level); log.SetFormatter(formatter) })
	return buf
}

type responsesTraceWriter struct {
	*httptest.ResponseRecorder
	beforeWrite func()
	fail        bool
}

func (w *responsesTraceWriter) Write(p []byte) (int, error) {
	if w.beforeWrite != nil {
		w.beforeWrite()
		w.beforeWrite = nil
	}
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func TestResponsesCodexStreamTraceEndToEnd(t *testing.T) {
	const model = "gpt-5.6-terra"
	var payloads []string
	for _, enabled := range []bool{true, false} {
		buf := captureResponsesTrace(t)
		if !enabled {
			log.SetLevel(log.InfoLevel)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Private", "HEADER-SENTINEL")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_trace\",\"output\":[]}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_trace\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_trace\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OUTPUT-SENTINEL\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		}))
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{}))
		auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Attributes: map[string]string{"api_key": "CREDENTIAL-SENTINEL", "base_url": server.URL}}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			server.Close()
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
		router := gin.New()
		router.POST("/v1/responses", h.Responses)
		writer := &responsesTraceWriter{ResponseRecorder: httptest.NewRecorder()}
		if enabled {
			writer.beforeWrite = func() {
				// Force executor Finish before accepting the first application byte.
				select {
				case <-buf.completed:
				case <-time.After(5 * time.Second):
					t.Error("executor did not finish before downstream write")
				}
				if strings.Contains(buf.String(), "first_downstream_application_write") {
					t.Error("downstream milestone before Writer accepted data")
				}
			}
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-terra","input":"PROMPT-SENTINEL","stream":true}`))
		request = request.WithContext(logging.WithRequestID(request.Context(), "e2e-12345678"))
		router.ServeHTTP(writer, request)
		server.Close()
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		payloads = append(payloads, writer.Body.String())
		if writer.Code != http.StatusOK || !strings.Contains(writer.Body.String(), "OUTPUT-SENTINEL") {
			t.Fatalf("unexpected response %d: %s", writer.Code, writer.Body.String())
		}
		out := buf.String()
		if strings.Contains(out, "SENTINEL") {
			t.Fatalf("new diagnostics leaked private data:\n%s", out)
		}
		if enabled {
			for _, stage := range []string{"start", "request_written", "upstream_status", "upstream_first_byte", "upstream_first_valid_json_data", "first_executor_chunk", "executor_completed", "first_downstream_application_write"} {
				if strings.Count(out, "stage="+stage+" ") != 1 {
					t.Errorf("missing/duplicate %s:\n%s", stage, out)
				}
			}
			if !strings.Contains(out, "12345678") {
				t.Errorf("request ID lost:\n%s", out)
			}
		} else if out != "" {
			t.Errorf("disabled diagnostics emitted logs: %s", out)
		}
	}
	if payloads[0] != payloads[1] {
		t.Fatalf("tracing changed response:\nenabled: %s\ndisabled: %s", payloads[0], payloads[1])
	}
}

func TestResponsesCodexStreamTraceOnlyAcceptedApplicationBytes(t *testing.T) {
	for _, tc := range []struct {
		name, chunk string
		fail, want  bool
	}{
		{name: "buffered", chunk: "data: {\"type\":\"response.created\","},
		{name: "heartbeat", chunk: ": keep-alive\n\n"},
		{name: "json_heartbeat", chunk: "data: {\"type\":\"keepalive\"}\n\n"},
		{name: "event_heartbeat", chunk: "event: keepalive\ndata: {}\n\n"},
		{name: "done_marker", chunk: "data: [DONE]\n\n"},
		{name: "write_failure", chunk: "data: {\"type\":\"response.created\"}\n\n", fail: true},
		{name: "application", chunk: "data: {\"type\":\"response.created\"}\n\n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureResponsesTrace(t)
			ctx := logging.WithCodexStreamTrace(context.Background())
			trace := logging.NewCodexStreamTracer(ctx)
			defer trace.Finish(nil)
			writer := &responsesTraceWriter{ResponseRecorder: httptest.NewRecorder(), fail: tc.fail}
			framer := &responsesSSEFramer{}
			framer.WriteChunk(responsesApplicationWriter{Writer: writer, ctx: ctx}, []byte(tc.chunk))
			got := strings.Contains(buf.String(), "stage=first_downstream_application_write")
			if got != tc.want {
				t.Errorf("milestone = %v, want %v:\n%s", got, tc.want, buf.String())
			}
			if tc.fail && framer.writeErr != io.ErrClosedPipe {
				t.Fatalf("write error changed: %v", framer.writeErr)
			}
			if tc.name == "buffered" && writer.Body.Len() != 0 {
				t.Fatal("framer wrote buffered input")
			}
		})
	}
}
