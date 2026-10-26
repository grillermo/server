package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func status(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/status got %d", rec.Code)
	}
	return rec.Body.String()
}

// /status says who holds the GPU and what the last GPU request got, and is
// answered by the gate itself rather than the upstream.
func TestStatusReportsHolderAndLastRequest(t *testing.T) {
	f := newSwapGate(t)

	if got := status(t, f.mema); !strings.Contains(got, "GPU:            free") ||
		!strings.Contains(got, "none since the gate started") {
		t.Fatalf("fresh status:\n%s", got)
	}

	f.transcribe()
	got := status(t, f.llama)
	for _, want := range []string{"GPU:            free", "Gate's record:  mema's model is loaded", "mema  POST /v1/transcribe", "-> 200 OK"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status after a transcription lacks %q:\n%s", want, got)
		}
	}

	f.g.acquire("llama")
	f.transcribe()
	got = status(t, f.mema)
	for _, want := range []string{"held by llama, 1 request in flight", "-> 503 Service Unavailable (GPU held by llama)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status while llama holds lacks %q:\n%s", want, got)
		}
	}
	if steps := f.got(); strings.Contains(steps, "status") {
		t.Fatalf("/status reached an upstream: %s", steps)
	}
}
