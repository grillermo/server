package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

// swapGate wires both sides to fake upstreams. Every evict and every request
// that reaches an upstream is appended to steps; evictErr makes the llama
// eviction (done for mema) fail, and hold blocks it until closed.
type swapGate struct {
	mu       sync.Mutex
	steps    []string
	evictErr error
	hold     chan struct{}
	g        *gpu
	mema     http.HandlerFunc
	llama    http.HandlerFunc
}

func (f *swapGate) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s)
}

func (f *swapGate) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.steps, ",")
}

func newSwapGate(t *testing.T) *swapGate {
	t.Helper()
	f := &swapGate{g: &gpu{}}
	upstream := func(name string) *httputil.ReverseProxy {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f.record(name)
			_, _ = io.WriteString(w, name)
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		return httputil.NewSingleHostReverseProxy(u)
	}
	f.mema = f.g.handler(side{
		name:    "mema",
		proxy:   upstream("transcribe"),
		isGPU:   memaGPU,
		busyMsg: `{"detail":"GPU not available"}`,
		evictOther: func(context.Context) error {
			if f.hold != nil {
				<-f.hold
			}
			f.record("unload-llama")
			return f.evictErr
		},
		evictMsg: `{"detail":"could not free the GPU"}`,
	})
	f.llama = f.g.handler(side{
		name:    "llama",
		proxy:   upstream("chat"),
		isGPU:   llamaGPU,
		busyMsg: `{"error":"busy"}`,
		evictOther: func(context.Context) error {
			f.record("unload-mema")
			return nil
		},
	})
	return f
}

func (f *swapGate) transcribe() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mema(rec, httptest.NewRequest("POST", "/v1/transcribe", strings.NewReader("")))
	return rec
}

func (f *swapGate) chat() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.llama(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
	return rec
}

// A model stays loaded while requests keep coming to its own server; only a
// switch to the other server unloads it.
func TestHandlerUnloadsOnlyWhenTheGPUChangesHands(t *testing.T) {
	f := newSwapGate(t)
	for _, call := range []func() *httptest.ResponseRecorder{
		f.transcribe, f.transcribe, f.chat, f.chat, f.transcribe,
	} {
		if rec := call(); rec.Code != http.StatusOK {
			t.Fatalf("got %d %q", rec.Code, rec.Body.String())
		}
	}
	want := "unload-llama,transcribe,transcribe,unload-mema,chat,chat,unload-llama,transcribe"
	if got := f.got(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

func TestHandlerRefusesWhenEvictFailsAndRetriesNextTime(t *testing.T) {
	f := newSwapGate(t)
	f.evictErr = errors.New("still loaded")
	rec := f.transcribe()
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "could not free") {
		t.Fatalf("got %d %q, want 503 evictMsg", rec.Code, rec.Body.String())
	}
	if got := f.got(); got != "unload-llama" {
		t.Fatalf("steps %s, want only unload-llama", got)
	}

	f.evictErr = nil
	if rec := f.transcribe(); rec.Code != http.StatusOK {
		t.Fatalf("retry got %d", rec.Code)
	}
	if got := f.got(); got != "unload-llama,unload-llama,transcribe" {
		t.Fatalf("steps %s", got)
	}
}

// A transcription that arrives while another one is still unloading llama
// waits for it, rather than loading into a card that is still full; a chat
// meanwhile is refused as usual.
func TestHandlerSameSideWaitsForTheSwitch(t *testing.T) {
	f := newSwapGate(t)
	f.hold = make(chan struct{})

	done := make(chan int, 2)
	go func() { done <- f.transcribe().Code }()
	waitFor(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		return f.g.swap != nil
	})
	go func() { done <- f.transcribe().Code }()
	waitFor(t, func() bool {
		f.g.mu.Lock()
		defer f.g.mu.Unlock()
		return f.g.active == 2
	})

	if rec := f.chat(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat during the switch got %d, want 503", rec.Code)
	}
	if got := f.got(); got != "" {
		t.Fatalf("steps %s before the unload finished, want none", got)
	}

	close(f.hold)
	for i := 0; i < 2; i++ {
		if code := <-done; code != http.StatusOK {
			t.Fatalf("transcribe got %d", code)
		}
	}
	if got := f.got(); got != "unload-llama,transcribe,transcribe" {
		t.Fatalf("steps %s", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
