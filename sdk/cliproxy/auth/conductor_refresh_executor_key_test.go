package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type countingRefreshExecutor struct {
	id           string
	refreshCalls atomic.Int32
	refreshErr   error
	refreshFunc  func(context.Context, *Auth) (*Auth, error)
}

func (e *countingRefreshExecutor) Identifier() string { return e.id }

func (e *countingRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *countingRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *countingRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	if e.refreshFunc != nil {
		return e.refreshFunc(ctx, auth)
	}
	if e.refreshErr != nil {
		return nil, e.refreshErr
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "refreshed-token"
	return auth, nil
}

func (e *countingRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *countingRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type refreshStateStore struct {
	mu          sync.Mutex
	auths       map[string]*Auth
	saves       atomic.Int32
	saveErr     error
	rejectedPut chan struct{}
	rejectedGo  chan struct{}
}

func newRefreshStateStore(auth *Auth) *refreshStateStore {
	store := &refreshStateStore{auths: make(map[string]*Auth)}
	if auth != nil {
		store.auths[auth.ID] = auth.Clone()
	}
	return store
}

func (s *refreshStateStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auths := make([]*Auth, 0, len(s.auths))
	for _, auth := range s.auths {
		auths = append(auths, auth.Clone())
	}
	return auths, nil
}

func (s *refreshStateStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth.LastError != nil && auth.LastError.Code == ErrorCodeRefreshCredentialRejected && s.rejectedPut != nil {
		select {
		case s.rejectedPut <- struct{}{}:
		default:
		}
		if s.rejectedGo != nil {
			<-s.rejectedGo
		}
	}
	if s.saveErr != nil {
		return "", s.saveErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths[auth.ID] = auth.Clone()
	s.saves.Add(1)
	return auth.ID, nil
}

func (s *refreshStateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.auths, id)
	return nil
}

func TestRefreshAuthIfNeededSerializesReloginAfterRejectedPersistence(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-relogin-order", func() *time.Duration { return &lead })
	oldAuth := &Auth{
		ID:       "relogin-order-oauth",
		Provider: "refresh-relogin-order",
		Metadata: map[string]any{
			"access_token":  "stale-token",
			"refresh_token": "rejected-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	store := newRefreshStateStore(oldAuth)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	manager.RegisterExecutor(&countingRefreshExecutor{id: oldAuth.Provider, refreshFunc: func(context.Context, *Auth) (*Auth, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, NewRefreshCredentialRejectedError(http.StatusBadRequest)
	}})
	if errLoad := manager.Load(ctx); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}

	refreshDone := make(chan error, 1)
	go func() {
		_, errRefresh := manager.RefreshAuthIfNeeded(ctx, oldAuth.ID)
		refreshDone <- errRefresh
	}()
	<-started

	// A relogin landing while the doomed refresh is still in flight must win:
	// the refresh failure belongs to the previous credential and must not be
	// stamped onto the replacement.
	newAuth := oldAuth.Clone()
	newAuth.Metadata["access_token"] = "relogin-access-token"
	newAuth.Metadata["refresh_token"] = "relogin-refresh-token"
	newAuth.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	newAuth.LastError = nil
	newAuth.Unavailable = false
	newAuth.Status = StatusActive
	newAuth.StatusMessage = ""
	if _, errUpdate := manager.Update(ctx, newAuth); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	close(release)
	if errRefresh := <-refreshDone; !IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshAuthIfNeeded() error = %v, want rejected refresh credential", errRefresh)
	}

	reloaded := NewManager(store, &RoundRobinSelector{}, nil)
	if errLoad := reloaded.Load(ctx); errLoad != nil {
		t.Fatalf("reload manager: %v", errLoad)
	}
	current, ok := reloaded.GetByID(oldAuth.ID)
	if !ok || authAccessToken(current) != "relogin-access-token" || hasRejectedRefreshCredential(current) {
		t.Fatalf("reloaded auth = %#v, want relogin credentials without rejected state", current)
	}
}

func TestRefreshAuthIfNeededSurfacesRejectedPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-persist-failure", func() *time.Duration { return &lead })
	auth := &Auth{
		ID:       "persist-failure-oauth",
		Provider: "refresh-persist-failure",
		Metadata: map[string]any{
			"access_token":  "stale-token",
			"refresh_token": "rejected-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	store := newRefreshStateStore(auth)
	store.saveErr = errors.New("store unavailable")
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&countingRefreshExecutor{id: auth.Provider, refreshErr: NewRefreshCredentialRejectedError(http.StatusBadRequest)})
	if errLoad := manager.Load(ctx); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}

	_, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
	if errRefresh == nil || !strings.Contains(errRefresh.Error(), "persist rejected refresh credential") {
		t.Fatalf("RefreshAuthIfNeeded() error = %v, want surfaced persistence failure", errRefresh)
	}
	if !IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshAuthIfNeeded() error = %v, want rejection classification preserved", errRefresh)
	}
}

func TestRefreshAuthIfNeededPersistsRejectedCredentialAcrossReload(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-persist-rejected", func() *time.Duration { return &lead })
	auth := &Auth{
		ID:       "persist-rejected-oauth",
		Provider: "refresh-persist-rejected",
		Metadata: map[string]any{
			"access_token":  "stale-token",
			"refresh_token": "rejected-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	store := newRefreshStateStore(auth)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &countingRefreshExecutor{id: auth.Provider, refreshErr: NewRefreshCredentialRejectedError(http.StatusBadRequest)}
	manager.RegisterExecutor(executor)
	if errLoad := manager.Load(ctx); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}

	if _, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID); !IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshAuthIfNeeded() error = %v, want rejected refresh credential", errRefresh)
	}
	if got := store.saves.Load(); got != 1 {
		t.Fatalf("store saves = %d, want 1", got)
	}

	reloaded := NewManager(store, &RoundRobinSelector{}, nil)
	reloadedExecutor := &countingRefreshExecutor{id: auth.Provider}
	reloaded.RegisterExecutor(reloadedExecutor)
	if errLoad := reloaded.Load(ctx); errLoad != nil {
		t.Fatalf("reload manager: %v", errLoad)
	}
	if _, errRefresh := reloaded.RefreshAuthIfNeeded(ctx, auth.ID); !IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("reloaded RefreshAuthIfNeeded() error = %v, want durable rejected state", errRefresh)
	}
	if got := reloadedExecutor.refreshCalls.Load(); got != 0 {
		t.Fatalf("reloaded refresh calls = %d, want 0", got)
	}
}

func TestRefreshAuthIfNeededConcurrentRejectionRefreshesOnce(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-concurrent-rejected", func() *time.Duration { return &lead })
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	executor := &countingRefreshExecutor{id: "refresh-concurrent-rejected", refreshFunc: func(context.Context, *Auth) (*Auth, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, NewRefreshCredentialRejectedError(http.StatusBadRequest)
	}}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "concurrent-rejected-oauth",
		Provider: executor.id,
		Metadata: map[string]any{
			"access_token":  "stale-token",
			"refresh_token": "rejected-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	const callers = 2
	errs := make(chan error, callers)
	go func() {
		_, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
		errs <- errRefresh
	}()
	<-started
	go func() {
		_, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
		errs <- errRefresh
	}()
	close(release)

	for range callers {
		if errRefresh := <-errs; !IsRefreshCredentialRejected(errRefresh) {
			t.Fatalf("RefreshAuthIfNeeded() error = %v, want rejected refresh credential", errRefresh)
		}
	}
	if got := executor.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestRefreshAuthIfNeededSkipsFreshCredential(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-if-needed-fresh", func() *time.Duration { return &lead })
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &countingRefreshExecutor{id: "refresh-if-needed-fresh"}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "fresh-oauth",
		Provider: "refresh-if-needed-fresh",
		Metadata: map[string]any{
			"access_token":  "current-token",
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	current, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
	if errRefresh != nil {
		t.Fatalf("RefreshAuthIfNeeded() error = %v", errRefresh)
	}
	if executor.refreshCalls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", executor.refreshCalls.Load())
	}
	if current == nil || current.Metadata["access_token"] != "current-token" {
		t.Fatalf("current auth = %#v, want unchanged access_token", current)
	}
}

func TestRefreshAuthIfNeededRefreshesExpiredCredential(t *testing.T) {
	ctx := context.Background()
	lead := 5 * time.Minute
	setRefreshLeadFactory(t, "refresh-if-needed-expired", func() *time.Duration { return &lead })
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &countingRefreshExecutor{id: "refresh-if-needed-expired"}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "expired-oauth",
		Provider: "refresh-if-needed-expired",
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	refreshed, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
	if errRefresh != nil {
		t.Fatalf("RefreshAuthIfNeeded() error = %v", errRefresh)
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", executor.refreshCalls.Load())
	}
	if refreshed == nil || refreshed.Metadata["access_token"] != "refreshed-token" {
		t.Fatalf("refreshed auth = %#v, want updated access_token", refreshed)
	}
}

func TestRefreshAuthIfNeededBlocksRejectedCredential(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:       "rejected-oauth",
		Provider: "refresh-if-needed-rejected",
		Metadata: map[string]any{
			"access_token":  "stale-token",
			"refresh_token": "rejected-refresh-token",
		},
		LastError:     NewRefreshCredentialRejectedError(http.StatusBadRequest),
		Unavailable:   true,
		Status:        StatusError,
		StatusMessage: "unauthorized",
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	current, errRefresh := manager.RefreshAuthIfNeeded(ctx, auth.ID)
	if !IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshAuthIfNeeded() error = %v, want rejected refresh credential", errRefresh)
	}
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr == nil || authErr.Code != ErrorCodeRefreshCredentialRejected || authErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("RefreshAuthIfNeeded() error = %#v, want durable HTTP 400 rejection", errRefresh)
	}
	if current != nil {
		t.Fatalf("current auth = %#v, want nil", current)
	}
}

func TestRefreshAuthForRequest_UsesExecutorKeyFromAuth(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &countingRefreshExecutor{id: "openai-compatible-custom"}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "compat-oauth",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"compat_name":  "custom",
			"provider_key": "custom",
			"base_url":     "https://compat.example.com/v1",
		},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, "old-token")
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", executor.refreshCalls.Load())
	}
	if refreshed == nil || refreshed.Metadata["access_token"] != "refreshed-token" {
		t.Fatalf("refreshed auth = %#v, want updated access_token", refreshed)
	}
}
