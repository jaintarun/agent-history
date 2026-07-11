package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStartRejectsNonLoopbackBind(t *testing.T) {
	_, err := Start(context.Background(), "0.0.0.0:0", Config{})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Start public bind error = %v, want loopback error", err)
	}
}

func TestServerUsesTokenAndGracefullyDrainsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-releaseRequest
		response.WriteHeader(http.StatusNoContent)
	})

	server, err := startHandler(ctx, "127.0.0.1:0", "test-token", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	if got := server.URL(); !strings.HasPrefix(got, "http://127.0.0.1:") || !strings.HasSuffix(got, "/test-token/") {
		t.Fatalf("server URL = %q", got)
	}

	responseDone := make(chan error, 1)
	go func() {
		response, err := http.Get(server.URL())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			err = response.Body.Close()
		}
		responseDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach handler")
	}

	cancel()
	select {
	case err := <-server.Done():
		t.Fatalf("server stopped before in-flight request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRequest)
	if err := <-responseDone; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
	select {
	case err := <-server.Done():
		if err != nil {
			t.Fatalf("server stopped with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after request drained")
	}
}
