package auth

import (
	"bytes"
	"context"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bodyspool"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// BodySpoolSplicer is implemented by executors whose send points splice body-spool skeletons
// (codex HTTP and WS, claude, openai-compat). Every other executor is a fallback path (D2).
type BodySpoolSplicer interface {
	SplicesBodySpool() bool
}

func splicesBodySpool(executor ProviderExecutor) bool {
	s, ok := executor.(BodySpoolSplicer)
	return ok && s.SplicesBodySpool()
}

// guardBodySpool hands an executor or plugin hook that is not converted the full body within the
// fallback limit L, and refuses above L with the named 413 that names the path. A converted
// executor without a body-reading hook keeps the skeleton, which it splices at its send point.
func guardBodySpool(ctx context.Context, executor ProviderExecutor, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, hookActive bool) (cliproxyexecutor.Request, cliproxyexecutor.Options, error) {
	sr := bodyspool.FromContext(ctx)
	sp := sr.Spool()
	if !sp.Has(req.Payload) && !sp.Has(opts.OriginalRequest) {
		return req, opts, nil
	}
	if splicesBodySpool(executor) && !hookActive {
		return req, opts, nil
	}
	path := "executor " + provider
	if hookActive {
		path = "plugin request hook"
	}
	payload, err := sr.Fallback(req.Payload, path)
	if err != nil {
		return req, opts, err
	}
	original := payload
	if !bytes.Equal(opts.OriginalRequest, req.Payload) {
		if original, err = sr.Fallback(opts.OriginalRequest, path); err != nil {
			return req, opts, err
		}
	}
	req.Payload, opts.OriginalRequest = payload, original
	return req, opts, nil
}

// isBodySpoolError reports a local body-spool refusal (507 insufficient_storage, 503
// insufficient_memory, 500 marker_unresolved, 413 limits). It belongs to the request, not to the
// credential: the conductor stops without MarkResult, cooldown or credential rotation.
func isBodySpoolError(err error) bool {
	var e *bodyspool.Error
	return errors.As(err, &e)
}
