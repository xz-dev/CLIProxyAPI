package executor

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

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	log "github.com/sirupsen/logrus"
)

type traceLogBuf struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines chan string
}

func newTraceLogBuf(t *testing.T) *traceLogBuf {
	t.Helper()
	b := &traceLogBuf{lines: make(chan string, 128)}
	prev, level, formatter := log.StandardLogger().Out, log.GetLevel(), log.StandardLogger().Formatter
	log.SetOutput(b)
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&logging.LogFormatter{})
	t.Cleanup(func() { log.SetOutput(prev); log.SetLevel(level); log.SetFormatter(formatter) })
	return b
}

func (b *traceLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, errWrite := b.buf.Write(p)
	if strings.Contains(string(p), "codex stream trace") {
		b.lines <- string(p)
	}
	return n, errWrite
}

func (b *traceLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *traceLogBuf) wait(t *testing.T, stage string) {
	t.Helper()
	guard := time.NewTimer(5 * time.Second)
	defer guard.Stop()
	for {
		select {
		case line := <-b.lines:
			if strings.Contains(line, "stage="+stage+" ") {
				return
			}
		case <-guard.C:
			t.Fatalf("missing stage %s:\n%s", stage, b.String())
		}
	}
}

func TestCodexStreamTraceCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, data, wait, absent string
		status                   int
	}{
		{name: "before_headers", wait: "request_written", absent: "upstream_status"},
		{name: "headers_no_body", status: 200, wait: "upstream_status", absent: "upstream_first_byte"},
		{name: "error_headers_no_body", status: 503, wait: "upstream_status", absent: "upstream_first_byte"},
		{name: "partial_invalid_data", status: 200, data: "data: {\"type\":\"response.created\",\"response\":{", wait: "upstream_first_byte", absent: "upstream_first_valid_json_data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := newTraceLogBuf(t)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.status != 0 {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.data)
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(logging.WithRequestID(context.Background(), "trace-cancel-request"))
			defer cancel()
			done := make(chan error, 1)
			req, opts := codexTestRequest()
			go func() {
				result, errExec := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTestAuth(server.URL), req, opts)
				if errExec == nil {
					_, errExec = drainChunks(result)
				}
				done <- errExec
			}()
			buf.wait(t, tc.wait)
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("execution did not cancel")
			}
			buf.wait(t, "canceled")
			out := buf.String()
			for _, absent := range []string{tc.absent, "first_downstream_application_write", "executor_completed"} {
				if strings.Contains(out, "stage="+absent) {
					t.Errorf("false milestone %s:\n%s", absent, out)
				}
			}
			if tc.data == "" && strings.Contains(out, "stage=first_executor_chunk") {
				t.Errorf("chunk produced without body bytes:\n%s", out)
			}
			if !strings.Contains(out, "body_bytes=") || !strings.Contains(out, "events_in=") || !strings.Contains(out, "chunks_out=") || !strings.Contains(out, "error=context_canceled") {
				t.Errorf("missing terminal metadata:\n%s", out)
			}
		})
	}
}

func TestCodexStreamTraceNormalAndDisabled(t *testing.T) {
	var payloads []string
	for _, enabled := range []bool{true, false} {
		buf := newTraceLogBuf(t)
		if !enabled {
			log.SetLevel(log.InfoLevel)
		}
		server := codexSSEServer(codexCreatedEvent, codexOutputDeltaEvent, codexCompletedEventBody)
		req, opts := codexTestRequest()
		result, errExec := NewCodexExecutor(&config.Config{}).ExecuteStream(logging.WithRequestID(context.Background(), "trace-normal-request"), codexTestAuth(server.URL), req, opts)
		if errExec != nil {
			server.Close()
			t.Fatal(errExec)
		}
		payload, errStream := drainChunks(result)
		server.Close()
		if errStream != nil {
			t.Fatal(errStream)
		}
		payloads = append(payloads, payload)
		if enabled {
			buf.wait(t, "executor_completed")
			out := buf.String()
			for _, stage := range []string{"start", "request_written", "upstream_status", "upstream_first_byte", "upstream_first_valid_json_data", "first_executor_chunk"} {
				if strings.Count(out, "stage="+stage+" ") != 1 {
					t.Errorf("stage %s missing or duplicated:\n%s", stage, out)
				}
			}
			if strings.Contains(out, "stage=first_downstream_application_write") {
				t.Error("executor fabricated downstream write")
			}
		} else if strings.Contains(buf.String(), "codex stream trace") {
			t.Error("trace enabled at info level")
		}
	}
	if payloads[0] != payloads[1] || !strings.Contains(payloads[0], "hello") {
		t.Fatalf("tracing changed payloads: %q", payloads)
	}
}
