package logging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
)

func captureCodexTrace(t *testing.T, level log.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, oldLevel, formatter := log.StandardLogger().Out, log.GetLevel(), log.StandardLogger().Formatter
	log.SetOutput(&buf)
	log.SetLevel(level)
	log.SetFormatter(&LogFormatter{})
	t.Cleanup(func() { log.SetOutput(prev); log.SetLevel(oldLevel); log.SetFormatter(formatter) })
	return &buf
}

func TestCodexStreamTracerLifecycle(t *testing.T) {
	buf := captureCodexTrace(t, log.DebugLevel)
	ctx := WithCodexStreamTrace(WithRequestID(context.Background(), "trace-request"))
	tracer := NewCodexStreamTracer(ctx)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	tracer.TraceHTTPRequest(request)
	hook := httptrace.ContextClientTrace(request.Context()).WroteRequest
	hook(httptrace.WroteRequestInfo{Err: errors.New("credential-secret")})
	if strings.Contains(buf.String(), "stage=request_written") {
		t.Fatal("failed write recorded as successful")
	}
	hook(httptrace.WroteRequestInfo{})
	tracer.MarkResponseStatus(200)
	_, _ = io.Copy(io.Discard, tracer.ObserveBody(io.NopCloser(strings.NewReader("secret-body"))))
	tracer.ObserveUpstreamEvent([]byte(`{"type":`))
	tracer.ObserveUpstreamEvent([]byte(`[DONE]`))
	tracer.ObserveUpstreamEvent([]byte(`{"type":"keepalive"}`))
	if strings.Contains(buf.String(), "upstream_first_valid_json_data") {
		t.Fatal("invalid data or DONE recorded as JSON progress")
	}
	tracer.ObserveUpstreamEvent([]byte(`{"type":"secret-output"}`))
	tracer.ObserveExecutorChunk([]byte("secret-output"))
	tracer.Finish(nil)
	// Async delivery survives Finish. An older retry finishing cannot clear a newer one.
	MarkCodexDownstreamWrite(ctx, 4)
	second := NewCodexStreamTracer(ctx)
	tracer.Finish(errors.New("secret-error"))
	MarkCodexDownstreamWrite(ctx, 8)
	second.Finish(errors.New("secret-error"))
	out := buf.String()
	if strings.Count(out, "stage=first_downstream_application_write") != 2 {
		t.Fatalf("lost async/retry write:\n%s", out)
	}
	if strings.Count(out, "stage=request_written") != 1 || !strings.Contains(out, "body_bytes=11 events_in=4 chunks_out=1") {
		t.Fatalf("wrong milestones/counts:\n%s", out)
	}
	firstID, secondID := tracer.entry.Data["execution_id"].(string), second.entry.Data["execution_id"].(string)
	if firstID == secondID || strings.Count(out, "execution_id="+firstID+" ")+strings.Count(out, "execution_id="+firstID+"\n") < 7 {
		t.Fatalf("execution identity absent or reused:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("payload/credential/error leaked:\n%s", out)
	}
	before := buf.String()
	hook(httptrace.WroteRequestInfo{})
	tracer.MarkResponseStatus(500)
	tracer.ObserveUpstreamEvent([]byte(`{}`))
	tracer.ObserveExecutorChunk([]byte("late"))
	if before != buf.String() {
		t.Fatal("upstream milestone logged after Finish")
	}
}

func TestCodexStreamTracerConcurrentFinish(t *testing.T) {
	buf := captureCodexTrace(t, log.DebugLevel)
	ctx := WithCodexStreamTrace(context.Background())
	tracer := NewCodexStreamTracer(ctx)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	tracer.TraceHTTPRequest(request)
	hook := httptrace.ContextClientTrace(request.Context()).WroteRequest
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook(httptrace.WroteRequestInfo{})
			tracer.ObserveUpstreamEvent([]byte(`{}`))
			tracer.ObserveExecutorChunk([]byte("x"))
			tracer.Finish(context.Canceled)
			MarkCodexDownstreamWrite(ctx, 1)
		}()
	}
	wg.Wait()
	if strings.Count(buf.String(), "stage=canceled") != 1 || strings.Count(buf.String(), "stage=first_downstream_application_write") != 1 {
		t.Fatalf("duplicate milestones:\n%s", buf.String())
	}
}

func TestCodexStreamTracerDisabled(t *testing.T) {
	buf := captureCodexTrace(t, log.InfoLevel)
	ctx := context.Background()
	if WithCodexStreamTrace(ctx) != ctx {
		t.Fatal("disabled trace changed context")
	}
	tracer := NewCodexStreamTracer(ctx)
	if tracer != nil {
		t.Fatal("expected disabled tracer")
	}
	tracer.TraceHTTPRequest(nil)
	tracer.MarkResponseStatus(200)
	tracer.ObserveUpstreamEvent(nil)
	tracer.ObserveExecutorChunk(nil)
	body := io.NopCloser(strings.NewReader("x"))
	if tracer.ObserveBody(body) != body {
		t.Fatal("disabled trace wrapped body")
	}
	tracer.Finish(nil)
	MarkCodexDownstreamWrite(ctx, 1)
	MarkCodexDownstreamWrite(nil, 1)
	if buf.Len() != 0 {
		t.Fatal("disabled trace emitted logs")
	}
}

func TestLogFormatterCodexStreamSafeFields(t *testing.T) {
	fields := log.Fields{"request_id": "trace-12345678", "execution_id": "42", "stage": "upstream_first_valid_json_data", "elapsed_ms": 12, "status": 200, "body_bytes": 42, "events_in": 1, "chunks_out": 1, "data_lines": 3, "line_bytes": 9, "lines_valid": false, "data_bytes": 4, "json_error_offset": 2,
		"headers": "HEADER-SENTINEL", "body": "BODY-SENTINEL", "token": "TOKEN-SENTINEL", "prompt": "PROMPT-SENTINEL"}
	out, errFormat := (&LogFormatter{}).Format(&log.Entry{Data: fields, Level: log.DebugLevel, Message: "codex stream trace"})
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	for key := range fields {
		switch key {
		case "headers", "body", "token", "prompt", "request_id":
			continue
		}
		if !strings.Contains(string(out), key+"=") {
			t.Errorf("formatter dropped %s: %s", key, out)
		}
	}
	if strings.Contains(string(out), "SENTINEL") {
		t.Fatalf("unsafe fields rendered: %s", out)
	}
}
