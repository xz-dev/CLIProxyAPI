package kimi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

type kimiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kimiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func resetKimiRefreshGroupForTest() {
	kimiRefreshGroup = singleflight.Group{}
}

func TestRefreshTokenClassifiesInvalidGrantAsRejectedCredential(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"refresh token is invalid"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	_, errRefresh := client.RefreshToken(context.Background(), "rejected-refresh-token")
	if !cliproxyauth.IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshToken() error = %v, want rejected refresh credential", errRefresh)
	}
	if strings.Contains(errRefresh.Error(), "refresh token is invalid") {
		t.Fatalf("RefreshToken() error exposed provider response: %v", errRefresh)
	}
	var authErr *cliproxyauth.Error
	if !errors.As(errRefresh, &authErr) || authErr == nil {
		t.Fatalf("RefreshToken() error = %T, want *auth.Error", errRefresh)
	}
	if authErr.Code != cliproxyauth.ErrorCodeRefreshCredentialRejected || authErr.HTTPStatus != http.StatusBadRequest || authErr.Retryable {
		t.Fatalf("RefreshToken() error = %+v, want non-retryable HTTP 400 rejection", authErr)
	}
}

func TestRefreshTokenLeavesOrdinaryBadRequestRetryable(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	_, errRefresh := client.RefreshToken(context.Background(), "retryable-refresh-token")
	if errRefresh == nil {
		t.Fatal("RefreshToken() error = nil, want ordinary bad request")
	}
	if cliproxyauth.IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshToken() error = %v, want retryable classification", errRefresh)
	}
}

func TestRefreshTokenLeavesNetworkErrorRetryable(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	transport := kimiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	_, errRefresh := client.RefreshToken(context.Background(), "retryable-refresh-token")
	if errRefresh == nil {
		t.Fatal("RefreshToken() error = nil, want network error")
	}
	if cliproxyauth.IsRefreshCredentialRejected(errRefresh) {
		t.Fatalf("RefreshToken() error = %v, want retryable classification", errRefresh)
	}
}

func TestRefreshTokenDoesNotClassifySuccessfulInvalidGrantField(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"error":"invalid_grant"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	tokenData, errRefresh := client.RefreshToken(context.Background(), "refresh-token")
	if errRefresh != nil {
		t.Fatalf("RefreshToken() error = %v, want success", errRefresh)
	}
	if tokenData == nil || tokenData.AccessToken != "new-access" {
		t.Fatalf("RefreshToken() token data = %#v, want new access token", tokenData)
	}
}

func TestRefreshToken_DeduplicatesConcurrentRefreshAcrossInstances(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"token_type":"Bearer",
				"expires_in":3600
			}`)),
			Header:  make(http.Header),
			Request: req,
		}, nil
	})
	clientA := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}
	clientB := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	results := make(chan *KimiTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func(client *DeviceFlowClient, launched chan<- struct{}) {
		if launched != nil {
			close(launched)
		}
		tokenData, errRefresh := client.RefreshToken(context.Background(), "shared-refresh-token")
		results <- tokenData
		errs <- errRefresh
	}

	go runRefresh(clientA, nil)
	<-started

	secondLaunched := make(chan struct{})
	go runRefresh(clientB, secondLaunched)
	<-secondLaunched
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if errRefresh := <-errs; errRefresh != nil {
			t.Fatalf("expected refresh to succeed, got %v", errRefresh)
		}
		tokenData := <-results
		if tokenData == nil || tokenData.AccessToken != "new-access" || tokenData.RefreshToken != "new-refresh" {
			t.Fatalf("unexpected token data: %#v", tokenData)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected both refresh callers to share a single upstream call, got %d", got)
	}
}
