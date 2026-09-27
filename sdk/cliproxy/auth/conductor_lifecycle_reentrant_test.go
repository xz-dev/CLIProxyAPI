package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type reentrantRegisterHook struct {
	NoopHook
	manager *Manager
	called  atomic.Bool
	done    chan error
}

func (h *reentrantRegisterHook) OnAuthRegistered(ctx context.Context, auth *Auth) {
	if h == nil || auth == nil || !h.called.CompareAndSwap(false, true) {
		return
	}
	nested := auth.Clone()
	if nested.Metadata == nil {
		nested.Metadata = make(map[string]any)
	}
	nested.Metadata["reentrant_hook"] = "registered"
	_, err := h.manager.Update(ctx, nested)
	h.done <- err
}

type reentrantUpdateHook struct {
	NoopHook
	manager *Manager
	called  atomic.Bool
	done    chan error
}

func (h *reentrantUpdateHook) OnAuthUpdated(ctx context.Context, auth *Auth) {
	if h == nil || auth == nil || !h.called.CompareAndSwap(false, true) {
		return
	}
	nested := auth.Clone()
	if nested.Metadata == nil {
		nested.Metadata = make(map[string]any)
	}
	nested.Metadata["reentrant_hook"] = "updated"
	_, err := h.manager.Update(ctx, nested)
	h.done <- err
}

func TestManager_RegisterHookCanSynchronouslyUpdateSameAuth(t *testing.T) {
	ctx := context.Background()
	hook := &reentrantRegisterHook{done: make(chan error, 1)}
	manager := NewManager(nil, nil, hook)
	hook.manager = manager

	registerDone := make(chan error, 1)
	go func() {
		_, err := manager.Register(ctx, &Auth{
			ID:       "reentrant-register-auth",
			Provider: "claude",
			Metadata: map[string]any{"source": "test"},
		})
		registerDone <- err
	}()

	select {
	case err := <-hook.done:
		if err != nil {
			t.Fatalf("nested Update() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAuthRegistered did not complete synchronous same-ID Update")
	}
	select {
	case err := <-registerDone:
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Register() did not complete")
	}

	current, ok := manager.GetByID("reentrant-register-auth")
	if !ok || current == nil || current.Metadata["reentrant_hook"] != "registered" {
		t.Fatalf("final auth = %#v, want nested registered update", current)
	}
}

func TestManager_UpdateHookCanSynchronouslyUpdateSameAuth(t *testing.T) {
	ctx := context.Background()
	hook := &reentrantUpdateHook{done: make(chan error, 1)}
	manager := NewManager(nil, nil, hook)
	hook.manager = manager

	const authID = "reentrant-update-auth"
	if _, err := manager.Register(ctx, &Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{"source": "test"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	updateDone := make(chan error, 1)
	go func() {
		_, err := manager.Update(ctx, &Auth{
			ID:       authID,
			Provider: "claude",
			Metadata: map[string]any{"source": "outer-update"},
		})
		updateDone <- err
	}()

	select {
	case err := <-hook.done:
		if err != nil {
			t.Fatalf("nested Update() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAuthUpdated did not complete synchronous same-ID Update")
	}
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Update() did not complete")
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Metadata["reentrant_hook"] != "updated" {
		t.Fatalf("final auth = %#v, want nested updated state", current)
	}
}
