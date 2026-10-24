package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMemaEvictSendsTheToken(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"unloaded":["parakeet-onnx/parakeet-tdt-0.6b-v3"]}`))
	}))
	defer srv.Close()

	m := &memaModels{base: srv.URL, token: "whisker-token", client: srv.Client()}
	if err := m.evict(context.Background()); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/unload" || gotAuth != "Bearer whisker-token" {
		t.Fatalf("got %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
}

func TestMemaEvictReportsAFailedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	m := &memaModels{base: srv.URL, client: srv.Client()}
	if err := m.evict(context.Background()); err == nil {
		t.Fatal("a 401 should be an error")
	}
}

func TestMemaEvictSucceedsWhenWhiskerIsDown(t *testing.T) {
	m := &memaModels{base: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}
	if err := m.evict(context.Background()); err != nil {
		t.Fatalf("evict with whisker down: %v", err)
	}
}
