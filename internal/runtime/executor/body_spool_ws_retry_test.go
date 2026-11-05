package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestCodexWebsocketStreamRetryAfterSendErrorSplices: when the first turn frame fails on a dead
// reused connection, the retry on a fresh connection still carries the spliced body: no marker
// leaves the hop and the blob bytes are sent (B1).
func TestCodexWebsocketStreamRetryAfterSendErrorSplices(t *testing.T) {
	body := spoolResponsesBody()
	frames := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(64 << 20)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frames <- msg
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	dead, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "spool-retry-session"
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = dead
	sess.connCloser = newWebsocketConnectionCloser(dead)
	sess.authID = "auth-a"
	sess.wsURL = wsURL
	sess.readerConn = dead // no reader: only the write path sees the failure
	sess.connMu.Unlock()
	_ = dead.Close() // the reused connection is dead: the first send fails

	ctx, sk := spoolCtx(t, body)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	auth := &cliproxyauth.Auth{ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "k", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	res, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: sk}, opts)
	if err != nil {
		t.Fatalf("retry after send error: %v", err)
	}
	go func() {
		for range res.Chunks {
		}
	}()
	select {
	case msg := <-frames:
		if bytes.Contains(msg, []byte(bodyspool.MarkerPrefix)) {
			t.Fatalf("retry frame carries a spool marker (%d bytes)", len(msg))
		}
		if !bytes.Contains(msg, []byte(spoolImageOf(body))) {
			t.Fatalf("retry frame lacks the blob bytes (%d bytes)", len(msg))
		}
	case <-ctx.Done():
		t.Fatal("no retry frame")
	}
}

// spoolImageOf returns the first data URL of a spoolResponsesBody body.
func spoolImageOf(body []byte) string {
	i := bytes.Index(body, []byte(`"data:`))
	j := bytes.IndexByte(body[i+1:], '"')
	return string(body[i+1 : i+1+j])
}

// TestSendPathsRefuseUnresolvedMarkers: a body that still carries a live spool marker the
// request's spool does not resolve is refused with the explicit 500 marker_unresolved, on the
// HTTP send helper and on both codex WS writers; nothing reaches the wire (fail closed, B1).
func TestSendPathsRefuseUnresolvedMarkers(t *testing.T) {
	_, sk := spoolCtx(t, spoolResponsesBody())
	frames := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(64 << 20)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- msg
		}
	}))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	unresolved := func(name string, err error) {
		t.Helper()
		var se *bodyspool.Error
		if !errors.As(err, &se) || se.Status != http.StatusInternalServerError || se.Code != "marker_unresolved" {
			t.Fatalf("%s: err = %v, want 500 marker_unresolved", name, err)
		}
	}
	// No spool on this context (lost context), but the markers belong to a live spool.
	_, err = bodyspool.NewHTTPRequest(context.Background(), http.MethodPost, srv.URL, sk)
	unresolved("NewHTTPRequest", err)
	unresolved("writeCodexWebsocketMessage", writeCodexWebsocketMessage(&codexWebsocketSession{}, conn, sk))
	unresolved("writeCodexWebsocketMessageCtx", writeCodexWebsocketMessageCtx(context.Background(), &codexWebsocketSession{}, conn, sk))
	// Another request's spool does not resolve these markers either.
	other, _ := spoolCtx(t, spoolResponsesBody())
	_, err = bodyspool.NewHTTPRequest(other, http.MethodPost, srv.URL, sk)
	unresolved("NewHTTPRequest(other spool)", err)

	if err := writeCodexWebsocketMessage(&codexWebsocketSession{}, conn, []byte(`{"text":"~lrcblob: is fine"}`)); err != nil {
		t.Fatalf("plain body refused: %v", err)
	}
	if msg := <-frames; !bytes.Equal(msg, []byte(`{"text":"~lrcblob: is fine"}`)) {
		t.Fatalf("first frame on the wire = %q, want only the plain body", msg)
	}
}
