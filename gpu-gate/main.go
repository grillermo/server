// gpu-gate is a reverse proxy that serializes GPU use between two local model
// servers that share one GPU: the llama.cpp router and the Whisker (mema)
// voice-to-text server. Each server runs on a private loopback port; the gate
// owns the public port in front of it. While one server is answering a
// GPU-bound request, a GPU-bound request to the other is refused immediately
// with a hardcoded "GPU not available" 503, instead of both fighting over the
// card. Health, model-list and status endpoints are never gated, so probes and
// discovery keep working no matter which server holds the GPU.
//
//	phone ─▶ :8787 (gate) ─▶ 127.0.0.1:8788  mema/whisker
//	pi    ─▶ :8080 (gate) ─▶ 127.0.0.1:8081  llama.cpp
//
// Same-server concurrency is allowed (several llama requests, or mema's own
// serialized calls); only the *other* server is locked out.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// gpu is the single GPU owner. active counts in-flight gated requests for the
// current owner; the owner clears when the last one finishes.
type gpu struct {
	mu     sync.Mutex
	owner  string // "", "llama" or "mema"
	active int
}

// acquire reserves the GPU for side, or reports who already holds it. It
// succeeds when the GPU is free or already held by this same side.
func (g *gpu) acquire(side string) (ok bool, heldBy string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == 0 {
		g.owner = side
	} else if g.owner != side {
		return false, g.owner
	}
	g.active++
	return true, ""
}

func (g *gpu) release(side string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner == side && g.active > 0 {
		g.active--
		if g.active == 0 {
			g.owner = ""
		}
	}
}

// side is one gated server: its name, the upstream proxy, the predicate that
// decides which requests use the GPU, and the body to return when locked out.
type side struct {
	name    string
	proxy   *httputil.ReverseProxy
	isGPU   func(*http.Request) bool
	busyMsg string // JSON; names the *other* server
}

func mustProxy(upstream string) *httputil.ReverseProxy {
	u, err := url.Parse("http://" + upstream)
	if err != nil {
		log.Fatalf("bad upstream %q: %v", upstream, err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1 // stream SSE (llama) and chunked bodies through at once
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
		log.Printf("upstream error: %v", err)
	}
	return p
}

func (g *gpu) handler(s side) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Gate itself answers its own health, so a probe never depends on the
		// upstream being up.
		if r.URL.Path == "/gpu-gate/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		if s.isGPU(r) {
			ok, heldBy := g.acquire(s.name)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(s.busyMsg))
				log.Printf("%s request refused: GPU held by %s", s.name, heldBy)
				return
			}
			defer g.release(s.name)
		}
		s.proxy.ServeHTTP(w, r)
	}
}

// llamaGPU: the llama.cpp endpoints that actually run the model. Everything
// else (/health, /v1/models, /props, /slots, /metrics) passes ungated.
func llamaGPU(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	return strings.Contains(p, "completion") ||
		strings.Contains(p, "embedding") ||
		strings.Contains(p, "infill") ||
		strings.Contains(p, "rerank")
}

// memaGPU: only transcription uses the GPU; /v1/health and /v1/status do not.
func memaGPU(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "transcribe")
}

func main() {
	llamaListen := flag.String("llama-listen", "127.0.0.1:8080", "public address for llama.cpp")
	llamaUpstream := flag.String("llama-upstream", "127.0.0.1:8081", "private llama.cpp address")
	memaListen := flag.String("mema-listen", "0.0.0.0:8787", "public address for Whisker/mema")
	memaUpstream := flag.String("mema-upstream", "127.0.0.1:8788", "private Whisker/mema address")
	flag.Parse()

	g := &gpu{}

	llama := side{
		name:    "llama",
		proxy:   mustProxy(*llamaUpstream),
		isGPU:   llamaGPU,
		busyMsg: `{"error":{"message":"GPU not available: the whisker voice-to-text server is using the GPU. Try again in a moment.","type":"gpu_unavailable","code":"gpu_busy"}}`,
	}
	mema := side{
		name:    "mema",
		proxy:   mustProxy(*memaUpstream),
		isGPU:   memaGPU,
		busyMsg: `{"detail":"GPU not available: llama-server is using the GPU. Try again in a moment."}`,
	}

	llamaSrv := &http.Server{Addr: *llamaListen, Handler: g.handler(llama)}
	memaSrv := &http.Server{Addr: *memaListen, Handler: g.handler(mema)}

	errs := make(chan error, 2)
	go func() {
		log.Printf("llama gate %s -> %s", *llamaListen, *llamaUpstream)
		errs <- llamaSrv.ListenAndServe()
	}()
	go func() {
		log.Printf("mema gate %s -> %s", *memaListen, *memaUpstream)
		errs <- memaSrv.ListenAndServe()
	}()
	log.Fatal(<-errs)
}
