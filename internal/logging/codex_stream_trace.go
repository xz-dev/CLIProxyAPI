package logging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// CodexStreamTracer records bounded metadata-only milestones. JSON data observation
// does not imply an SSE frame delimiter, model output, or downstream delivery.
// Elapsed times use Go's monotonic clock; execution IDs are process-local unique.
type CodexStreamTracer struct {
	mu                            sync.Mutex
	entry                         *log.Entry
	ctx                           context.Context
	start                         time.Time
	seen                          map[string]bool
	finished                      bool
	bodyBytes, dataEvents, chunks int64
}

type codexStreamTraceKey struct{}
type codexStreamTraceSlot struct {
	sync.Mutex
	current *CodexStreamTracer
}

var codexStreamExecutionID atomic.Uint64

// WithCodexStreamTrace installs a request-local slot before execution. Only the
// latest attempt is retained, including after executor completion for async writes.
func WithCodexStreamTrace(ctx context.Context) context.Context {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return ctx
	}
	return context.WithValue(ctx, codexStreamTraceKey{}, &codexStreamTraceSlot{})
}

// ClearCodexStreamTrace detaches the previous attempt before any provider starts.
// A non-Codex fallback must not inherit a failed Codex attempt's downstream marker.
func ClearCodexStreamTrace(ctx context.Context) {
	if ctx == nil {
		return
	}
	if slot, ok := ctx.Value(codexStreamTraceKey{}).(*codexStreamTraceSlot); ok {
		slot.Lock()
		slot.current = nil
		slot.Unlock()
	}
}

// NewCodexStreamTracer starts a fresh execution; nil methods are safe when disabled.
func NewCodexStreamTracer(ctx context.Context) *CodexStreamTracer {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return nil
	}
	t := &CodexStreamTracer{
		entry: log.WithFields(log.Fields{"provider": "codex", "request_id": GetRequestID(ctx),
			"execution_id": strconv.FormatUint(codexStreamExecutionID.Add(1), 16)}),
		ctx: ctx, start: time.Now(), seen: make(map[string]bool),
	}
	if slot, ok := ctx.Value(codexStreamTraceKey{}).(*codexStreamTraceSlot); ok {
		slot.Lock()
		slot.current = t
		slot.Unlock()
	}
	t.emit("start", nil)
	return t
}

func (t *CodexStreamTracer) emit(stage string, fields log.Fields) {
	if fields == nil {
		fields = log.Fields{}
	}
	fields["stage"] = stage
	fields["elapsed_ms"] = time.Since(t.start).Milliseconds()
	t.entry.WithFields(fields).Debug("codex stream trace")
}

func (t *CodexStreamTracer) mark(stage string, fields log.Fields) {
	if !t.seen[stage] {
		t.seen[stage] = true
		t.emit(stage, fields)
	}
}

// TraceHTTPRequest preserves existing hooks and ignores failed or late writes.
func (t *CodexStreamTracer) TraceHTTPRequest(req *http.Request) {
	if t == nil || req == nil {
		return
	}
	*req = *req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.finished && info.Err == nil {
				t.mark("request_written", nil)
			}
		},
	}))
}

// MarkResponseStatus records header arrival without recording header values.
func (t *CodexStreamTracer) MarkResponseStatus(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.mark("upstream_status", log.Fields{"status": status})
	}
}

func (t *CodexStreamTracer) ObserveBody(body io.ReadCloser) io.ReadCloser {
	if t == nil || body == nil {
		return body
	}
	return &codexTraceReadCloser{ReadCloser: body, tracer: t}
}

type codexTraceReadCloser struct {
	io.ReadCloser
	tracer *CodexStreamTracer
}

func (r *codexTraceReadCloser) Read(p []byte) (int, error) {
	n, errRead := r.ReadCloser.Read(p)
	if n > 0 {
		t := r.tracer
		t.mu.Lock()
		if !t.finished {
			t.bodyBytes += int64(n)
			t.mark("upstream_first_byte", nil)
		}
		t.mu.Unlock()
	}
	return n, errRead
}

// ObserveUpstreamEvent observes scanner data values, not complete SSE frames.
// [DONE], keepalive messages and invalid fragments never count as JSON progress.
func (t *CodexStreamTracer) ObserveUpstreamEvent(data []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.dataEvents++
	if !t.seen["upstream_first_valid_json_data"] && json.Valid(data) && gjson.GetBytes(data, "type").String() != "keepalive" {
		t.mark("upstream_first_valid_json_data", nil)
	}
}

func (t *CodexStreamTracer) ObserveExecutorChunk(payload []byte) {
	if t == nil || len(payload) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.chunks++
	t.mark("first_executor_chunk", nil)
}

// MarkCodexDownstreamWrite records application bytes accepted by HTTP Writer,
// not peer receipt or flush success. Call only for successful, non-heartbeat writes.
func MarkCodexDownstreamWrite(ctx context.Context, n int) {
	if ctx == nil || n <= 0 {
		return
	}
	slot, ok := ctx.Value(codexStreamTraceKey{}).(*codexStreamTraceSlot)
	if !ok {
		return
	}
	slot.Lock()
	t := slot.current
	slot.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Executor completion can precede the handler draining its output channel.
	t.mark("first_downstream_application_write", nil)
}

// Finish records executor outcome, not downstream delivery success.
func (t *CodexStreamTracer) Finish(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	stage := "executor_completed"
	fields := log.Fields{"body_bytes": t.bodyBytes, "events_in": t.dataEvents, "chunks_out": t.chunks}
	if err != nil {
		stage = "failed"
		fields["error"] = "upstream_error"
	}
	// Successful terminal observation is not undone by the handler's later cleanup cancel.
	var ctxErr error
	if err != nil {
		ctxErr = t.ctx.Err()
	}
	if ctxErr == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		ctxErr = err
	}
	if ctxErr != nil {
		stage = "canceled"
		fields["error"] = "context_canceled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			fields["error"] = "context_deadline_exceeded"
		}
	}
	t.emit(stage, fields)
}
