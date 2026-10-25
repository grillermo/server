package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The lease holds the GPU for as long as its connection is open: other sides
// are refused meanwhile, and closing the response gives the GPU back.
func TestHoldKeepsTheGPUUntilTheClientCloses(t *testing.T) {
	g := &gpu{}
	var evicted []string
	srv := httptest.NewServer(g.holdHandler(side{
		name:    "enhance",
		busyMsg: `{"error":"GPU not available"}`,
		evictOther: func(context.Context) error {
			evicted = append(evicted, "others")
			return nil
		},
	}))
	defer srv.Close()

	hold := func() *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+"/gpu-gate/hold", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	g.acquire("llama")
	resp := hold()
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("hold while llama runs got %d, want 503", resp.StatusCode)
	}
	g.release("llama")

	resp = hold()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hold got %d, want 200", resp.StatusCode)
	}
	if ok, heldBy := g.acquire("llama"); ok || heldBy != "enhance" {
		t.Fatalf("llama during the hold: ok=%v heldBy=%q", ok, heldBy)
	}
	if strings.Join(evicted, ",") != "others" {
		t.Fatalf("evicted %v, want the others once", evicted)
	}

	resp.Body.Close()
	waitFor(t, func() bool {
		ok, _ := g.acquire("llama")
		if ok {
			g.release("llama")
		}
		return ok
	})
}

func TestHoldOnlyAnswersPostHold(t *testing.T) {
	g := &gpu{}
	h := g.holdHandler(side{name: "enhance"})
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/gpu-gate/health", http.StatusOK},
		{"GET", "/gpu-gate/hold", http.StatusMethodNotAllowed},
		{"POST", "/v1/unload", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s got %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	if g.active != 0 {
		t.Fatalf("active %d after non-hold requests", g.active)
	}
}
