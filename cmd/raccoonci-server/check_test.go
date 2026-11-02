package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthCheckFollowsTheStatusCode(t *testing.T) {
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
	defer srv.Close()
	if err := healthCheck(srv.URL); err != nil {
		t.Errorf("ready server: %v", err)
	}
	code = 503
	if err := healthCheck(srv.URL); err == nil {
		t.Error("a 503 passed the check")
	}
	srv.Close()
	if err := healthCheck(srv.URL); err == nil {
		t.Error("a server that is gone passed the check")
	}
}
