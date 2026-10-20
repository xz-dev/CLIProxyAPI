package auth

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type conductorTraceLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *conductorTraceLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *conductorTraceLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestExecuteStreamTraceDetachesPreviousProvider(t *testing.T) {
	for _, nextProvider := range []string{"other-provider", "codex"} {
		t.Run(nextProvider, func(t *testing.T) {
			var buf conductorTraceLog
			prev, level, formatter := log.StandardLogger().Out, log.GetLevel(), log.StandardLogger().Formatter
			log.SetOutput(&buf)
			log.SetLevel(log.DebugLevel)
			log.SetFormatter(&logging.LogFormatter{})
			t.Cleanup(func() { log.SetOutput(prev); log.SetLevel(level); log.SetFormatter(formatter) })
			ctx := logging.WithCodexStreamTrace(context.Background())
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(0, 0, 1)
			register := func(id, provider string) {
				t.Helper()
				auth := &Auth{ID: id, Provider: provider, Status: StatusActive}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: "trace-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := m.Register(ctx, auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			register(t.Name()+"-first", "codex")
			var previousTrace *logging.CodexStreamTracer
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				previousTrace = logging.NewCodexStreamTracer(ctx)
				errStream := overloadStatusError()
				previousTrace.Finish(errStream)
				return nil, errStream
			}})
			req := cliproxyexecutor.Request{Model: "trace-model"}
			if _, errStream := m.ExecuteStream(ctx, []string{"codex"}, req, cliproxyexecutor.Options{}); errStream == nil {
				t.Fatal("expected failed Codex attempt")
			}
			register(t.Name()+"-next", nextProvider)
			m.RegisterExecutor(&customStreamMockExecutor{identifier: nextProvider, streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				if nextProvider == "codex" {
					trace := logging.NewCodexStreamTracer(ctx)
					trace.ObserveExecutorChunk([]byte("application"))
					trace.Finish(nil)
				}
				previousTrace.Finish(nil) // An older completion must not clear the new attempt.
				return successStreamResult(), nil
			}})
			result, errStream := m.ExecuteStream(ctx, []string{nextProvider}, req, cliproxyexecutor.Options{})
			if errStream != nil {
				t.Fatal(errStream)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				logging.MarkCodexDownstreamWrite(ctx, len(chunk.Payload))
			}
			out := buf.String()
			wantWrites := 0
			if nextProvider == "codex" {
				wantWrites = 1
			}
			if strings.Count(out, "stage=first_downstream_application_write") != wantWrites {
				t.Fatalf("stale or missing downstream attribution:\n%s", out)
			}
			if nextProvider == "codex" {
				ids := map[string]bool{}
				for _, line := range strings.Split(out, "\n") {
					if !strings.Contains(line, "stage=start ") {
						continue
					}
					for _, field := range strings.Fields(line) {
						if strings.HasPrefix(field, "execution_id=") {
							ids[field] = true
						}
					}
				}
				if len(ids) != 2 {
					t.Fatalf("retry execution identity reused:\n%s", out)
				}
			}
		})
	}
}
