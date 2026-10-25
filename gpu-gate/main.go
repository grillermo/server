// gpu-gate is a reverse proxy that serializes GPU use between two local model
// servers that share one GPU: the llama.cpp router and the Whisker (mema)
// voice-to-text server. Each server runs on a private loopback port; the gate
// owns the public port in front of it. While one server is answering a
// GPU-bound request, a GPU-bound request to the other is refused immediately
// with a hardcoded "GPU not available" 503, instead of both fighting over the
// card. Health, model-list and status endpoints are never gated, so probes and
// discovery keep working no matter which server holds the GPU.
//
// Holding the lock is not enough on a 12 GB card: an idle server keeps its
// model resident, and the other one then cannot load. So the gate remembers
// whose model is in VRAM, and when a GPU request comes for the other server it
// first frees it: llama's models through the router API (waiting until they
// are gone), or whisker's through its POST /v1/unload. The model stays loaded
// while requests keep coming to the same server, so back-to-back dictations or
// chats pay no loading cost; only a switch does. Requests to the same server
// that arrive during the switch wait for it instead of loading early.
//
//	phone ─▶ :8787 (gate) ─▶ 127.0.0.1:8788  mema/whisker
//	pi    ─▶ :8080 (gate) ─▶ 127.0.0.1:8081  llama.cpp
//
// Same-server concurrency is allowed (several llama requests, or mema's own
// serialized calls); only the *other* server is locked out.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// gpu is the single GPU owner. active counts in-flight gated requests for the
// current owner; the owner clears when the last one finishes. resident is the
// side whose model may be in VRAM, which outlives the requests.
type gpu struct {
	mu       sync.Mutex
	owner    string // "", "llama" or "mema"
	active   int
	resident string        // "" until the first switch, so it evicts whatever is there
	swap     chan struct{} // closed when the owner's switch finishes; nil when none runs
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

// ready makes the GPU's memory free for side, which must hold an acquire. The
// first request after the GPU changes hands runs evict to unload the other
// server's model; requests of the same side that arrive meanwhile wait for it,
// so none starts loading into memory that is still taken.
func (g *gpu) ready(side string, evict func(context.Context) error) error {
	g.mu.Lock()
	if ch := g.swap; ch != nil {
		g.mu.Unlock()
		<-ch
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.resident != side {
			return errors.New("the other server's model could not be unloaded")
		}
		return nil
	}
	if g.resident == side {
		g.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	g.swap = ch
	g.mu.Unlock()

	var err error
	if evict != nil {
		// Not the request's context: the requests waiting on this switch
		// should not fail because this one's client went away.
		err = evict(context.Background())
	}

	g.mu.Lock()
	if err == nil {
		g.resident = side
	}
	g.swap = nil
	close(ch)
	g.mu.Unlock()
	return err
}

// side is one gated server: its name, the upstream proxy, the predicate that
// decides which requests use the GPU, and the body to return when locked out.
type side struct {
	name    string
	proxy   *httputil.ReverseProxy
	isGPU   func(*http.Request) bool
	busyMsg string // JSON; names the *other* server

	// evictOther frees the other server's GPU memory before this side's model
	// loads. If it fails, the request is refused with evictMsg.
	evictOther func(context.Context) error
	evictMsg   string
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
				refuse(w, s.busyMsg)
				log.Printf("%s request refused: GPU held by %s", s.name, heldBy)
				return
			}
			defer g.release(s.name)
			if err := g.ready(s.name, s.evictOther); err != nil {
				refuse(w, s.evictMsg)
				log.Printf("%s request refused: %v", s.name, err)
				return
			}
		}
		s.proxy.ServeHTTP(w, r)
	}
}

func refuse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(body))
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

// memaGPU: only transcription uses the GPU (whisker's /v1/transcribe and the
// OpenAI-style /v1/audio/transcriptions); /v1/health, /v1/status and
// /v1/models do not.
func memaGPU(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "transcri")
}

func main() {
	llamaListen := flag.String("llama-listen", "127.0.0.1:8080", "public address for llama.cpp")
	llamaUpstream := flag.String("llama-upstream", "127.0.0.1:8081", "private llama.cpp address")
	memaListen := flag.String("mema-listen", "0.0.0.0:8787", "public address for Whisker/mema")
	memaUpstream := flag.String("mema-upstream", "127.0.0.1:8788", "private Whisker/mema address")
	llamaKeyEnv := flag.String("llama-key-env", "LLAMA_API_KEY", "env var holding llama-server's --api-key, used to unload its models")
	memaTokenEnv := flag.String("mema-token-env", "WHISKER_AUTH_TOKEN", "env var holding whisker's token, used to unload its models")
	flag.Parse()

	g := &gpu{}

	llamaKey := os.Getenv(*llamaKeyEnv)
	if llamaKey == "" {
		log.Printf("warning: $%s is empty; unloading llama models without auth", *llamaKeyEnv)
	}
	memaToken := os.Getenv(*memaTokenEnv)
	if memaToken == "" {
		log.Printf("warning: $%s is empty; whisker will refuse to unload", *memaTokenEnv)
	}
	llamaRouter := &llamaModels{
		base:    "http://" + *llamaUpstream,
		key:     llamaKey,
		client:  &http.Client{Timeout: 10 * time.Second},
		poll:    250 * time.Millisecond,
		timeout: 20 * time.Second,
	}
	whisker := &memaModels{
		base:   "http://" + *memaUpstream,
		token:  memaToken,
		client: &http.Client{Timeout: 30 * time.Second},
	}

	llama := side{
		name:    "llama",
		proxy:   mustProxy(*llamaUpstream),
		isGPU:   llamaGPU,
		busyMsg: `{"error":{"message":"GPU not available: the whisker voice-to-text server is using the GPU. Try again in a moment.","type":"gpu_unavailable","code":"gpu_busy"}}`,
		// A chat goes ahead even if whisker would not unload: llama's
		// `fit = on` sizes its offload to the memory that is free, while
		// whisker would fail to load into a full card.
		evictOther: func(ctx context.Context) error {
			if err := whisker.evict(ctx); err != nil {
				log.Printf("whisker unload failed, loading llama anyway: %v", err)
			}
			return nil
		},
	}
	mema := side{
		name:       "mema",
		proxy:      mustProxy(*memaUpstream),
		isGPU:      memaGPU,
		busyMsg:    `{"detail":"GPU not available: llama-server is using the GPU. Try again in a moment."}`,
		evictOther: llamaRouter.evict,
		evictMsg:   `{"detail":"GPU not available: could not free the GPU from llama-server. Try again in a moment."}`,
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
