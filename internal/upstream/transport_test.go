package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type panelTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	closed    int
}

func (transport *panelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.roundTrip(request)
}
func (transport *panelTransport) CloseIdleConnections() { transport.closed++ }

func TestPanelChatCancelAndClose(test *testing.T) {
	var requestContext context.Context
	transport := &panelTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		requestContext = request.Context()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}}
	client := &Client{HTTP: &http.Client{Transport: transport}}
	body, _, _, err := client.ChatStreamContext(context.Background(), &auth.Auth{UID: "test"}, []byte(`{}`), "")
	if err != nil {
		test.Fatal(err)
	}
	if err := body.Close(); err != nil {
		test.Fatal(err)
	}
	if !errors.Is(requestContext.Err(), context.Canceled) {
		test.Error("Close did not cancel with idle watchdog disabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := client.ChatStreamContext(ctx, &auth.Auth{}, []byte(`{}`), ""); !errors.Is(err, context.Canceled) {
		test.Errorf("error = %v", err)
	}
}

func TestPanelChatCancellationDuringHeaders(test *testing.T) {
	started := make(chan struct{})
	transport := &panelTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	client := &Client{HTTP: &http.Client{Transport: transport}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, _, _, err := client.ChatStreamContext(ctx, &auth.Auth{}, []byte(`{}`), ""); finished <- err }()
	<-started
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			test.Error(err)
		}
	case <-time.After(time.Second):
		test.Fatal("upstream request did not cancel")
	}
	if transport.closed != 0 {
		test.Error("client cancellation cleared other idle connections")
	}
}

func TestPanelTransportFailureClearsIdle(test *testing.T) {
	transport := &panelTransport{roundTrip: func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }}
	client := &Client{HTTP: &http.Client{Transport: transport}}
	if _, _, _, err := client.ChatStream(&auth.Auth{}, []byte(`{}`)); err == nil {
		test.Fatal("transport error swallowed")
	}
	if transport.closed != 1 {
		test.Fatal("idle connections not cleared")
	}
	defaults := newTransport()
	if defaults.DialContext == nil || defaults.TLSHandshakeTimeout != 10*time.Second || defaults.ResponseHeaderTimeout != 120*time.Second {
		test.Fatal("missing connection timeout protection")
	}
	if defaults.TLSNextProto != nil {
		test.Error("HTTP/2 forcibly disabled")
	}
}

func TestPanelHeadersReadConsistentCredentials(test *testing.T) {
	account := &auth.Auth{UID: "test", AccessToken: "before", RefreshToken: "refresh", Domain: "before"}
	client := &Client{}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for iteration := 0; iteration < 100; iteration++ {
			_ = account.Refresh(context.Background(), func(snapshot *auth.Auth) error { snapshot.AccessToken, snapshot.Domain = "after", "after"; return nil })
		}
	}()
	for iteration := 0; iteration < 100; iteration++ {
		request, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
		client.ChatHeaders(request, account)
		if request.Header.Get("Authorization") != "Bearer "+request.Header.Get("X-Domain") {
			test.Error("torn credential header pair")
		}
	}
	workers.Wait()
}

func TestPanelCacheKeyUsedOnWire(test *testing.T) {
	transport := &panelTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		object := decodeCompat(test, raw)
		if object["prompt_cache_key"] == nil || object["max_tokens"] != float64(123) {
			test.Error("compatibility not connected to outgoing request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}}
	client := &Client{HTTP: &http.Client{Transport: transport}, PromptCacheKey: true}
	body, _, _, err := client.ChatStreamContext(context.Background(), &auth.Auth{UID: "test"}, []byte(`{"max_completion_tokens":123}`), "header-conversation")
	if err != nil {
		test.Fatal(err)
	}
	body.Close()
}
