package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuntimeTransportKeepsActiveStream(t *testing.T) {
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-finish
		w.Write([]byte("last"))
	}))
	defer server.Close()
	old := New()
	response, err := old.ChatHTTP.Get(server.URL)
	if err != nil {
		close(finish)
		t.Fatal(err)
	}
	next := old.WithRuntime(RuntimeOptions{Timeout: time.Second, HeaderTimeout: 2 * time.Second, IdleTimeout: 3 * time.Second, PromptCacheKey: false})
	old.CloseIdleConnections()
	close(finish)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "firstlast" {
		t.Fatalf("active stream interrupted: %q %v", body, err)
	}
	if next.HTTP.Timeout != time.Second || old.HTTP.Timeout == time.Second || next.ChatHTTP.Transport == old.ChatHTTP.Transport || !old.PromptCacheKey || next.PromptCacheKey {
		t.Fatal("runtime mutated old snapshot")
	}
	next.CloseIdleConnections()
}
