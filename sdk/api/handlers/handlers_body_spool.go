package handlers

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

type responseInterceptorDetector interface {
	HasResponseInterceptors() bool
}

// bodyReadingHooksActive reports whether a plugin hook receives the request body: request,
// stream-chunk, response interceptors or WebSocket response observers.
func (h *BaseAPIHandler) bodyReadingHooksActive() bool {
	host := h.interceptorHost()
	if host == nil {
		return false
	}
	if requestInterceptorsEnabled(host) || streamInterceptorsEnabled(host) || webSocketResponseObserversEnabled(host) {
		return true
	}
	if d, ok := host.(responseInterceptorDetector); ok {
		return d.HasResponseInterceptors()
	}
	return true
}

// bodySpoolHookGuard expands a spooled skeleton for body-reading plugin hooks and plugin executors,
// which are fallback paths (D2): within L the full body is materialized (charged at k_path), above
// L the request gets the named 413. Without hooks the skeleton is returned unchanged.
func (h *BaseAPIHandler) bodySpoolHookGuard(ctx context.Context, rawJSON []byte, pluginExecutor bool) ([]byte, *interfaces.ErrorMessage) {
	sr := bodyspool.FromContext(ctx)
	if sr == nil || !sr.Spool().Has(rawJSON) {
		return rawJSON, nil
	}
	path := ""
	switch {
	case pluginExecutor:
		path = "plugin executor"
	case h.bodyReadingHooksActive():
		path = "plugin body hook"
	default:
		return rawJSON, nil
	}
	full, err := sr.Fallback(rawJSON, path)
	if err != nil {
		return nil, &interfaces.ErrorMessage{StatusCode: clienterror.HTTPStatusFromErrorOr(err, 500), Error: err}
	}
	return full, nil
}
