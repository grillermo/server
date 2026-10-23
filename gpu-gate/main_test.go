package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestAcquireLocksOutOtherSide(t *testing.T) {
	g := &gpu{}

	if ok, _ := g.acquire("llama"); !ok {
		t.Fatal("first llama acquire should succeed")
	}
	// Same side may run concurrently.
	if ok, _ := g.acquire("llama"); !ok {
		t.Fatal("second llama acquire should succeed")
	}
	// The other side is locked out while llama holds it.
	if ok, heldBy := g.acquire("mema"); ok || heldBy != "llama" {
		t.Fatalf("mema should be refused, got ok=%v heldBy=%q", ok, heldBy)
	}

	// Still held after releasing only one of the two llama holders.
	g.release("llama")
	if ok, _ := g.acquire("mema"); ok {
		t.Fatal("mema should still be refused while one llama request is in flight")
	}
	// Last llama holder releases -> GPU is free for mema.
	g.release("llama")
	if ok, _ := g.acquire("mema"); !ok {
		t.Fatal("mema should acquire once llama is fully released")
	}
}

func TestGPUPredicates(t *testing.T) {
	req := func(method, path string) *http.Request {
		return httptest.NewRequest(method, path, nil)
	}
	cases := []struct {
		name string
		fn   func(*http.Request) bool
		r    *http.Request
		want bool
	}{
		{"llama chat", llamaGPU, req("POST", "/v1/chat/completions"), true},
		{"llama embeddings", llamaGPU, req("POST", "/v1/embeddings"), true},
		{"llama health ungated", llamaGPU, req("GET", "/health"), false},
		{"llama models ungated", llamaGPU, req("GET", "/v1/models"), false},
		{"mema transcribe", memaGPU, req("POST", "/v1/transcribe"), true},
		{"mema health ungated", memaGPU, req("GET", "/v1/health"), false},
		{"mema status ungated", memaGPU, req("GET", "/v1/status"), false},
	}
	for _, c := range cases {
		if got := c.fn(c.r); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// A GPU request to the busy side gets the hardcoded 503; an ungated request
// (health) still reaches the upstream.
func TestHandlerRefusesWhenOtherSideBusy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	g := &gpu{}
	s := side{
		name:    "mema",
		proxy:   httputil.NewSingleHostReverseProxy(u),
		isGPU:   memaGPU,
		busyMsg: `{"detail":"GPU not available"}`,
	}
	h := g.handler(s)

	// llama holds the GPU.
	g.acquire("llama")

	// A transcription is refused with the hardcoded body.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/v1/transcribe", strings.NewReader("")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("transcribe got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GPU not available") {
		t.Fatalf("transcribe body = %q", rec.Body.String())
	}

	// Health still passes through to the upstream even while llama holds it.
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
		t.Fatalf("health got %d %q, want 200 upstream-ok", rec.Code, rec.Body.String())
	}
}
