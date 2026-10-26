package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSMI writes an nvidia-smi stand-in that reports 12282 MiB of VRAM and
// the given compute processes ("pid, MiB" lines).
func fakeSMI(t *testing.T, apps string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvidia-smi")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
--query-gpu=*) echo "11158, 12282" ;;
--query-compute-apps=*) printf '%%s' '%s' ;;
esac
`, apps)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A process holding VRAM is attributed to the side whose upstream port it
// listens on, whoever started it, and the gate's record follows at startup.
func TestProbeAttributesVRAMByListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	p := newVRAMProbe(map[string]string{"llama": ln.Addr().String(), "mema": "127.0.0.1:1"})
	p.smi = fakeSMI(t, fmt.Sprintf("%d, 10556\n1, 8\n", os.Getpid()))
	p.models["llama"] = func(context.Context) ([]string, error) { return []string{"qwen"}, nil }

	snap, err := p.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.byside(); got["llama"] != 10556 || got[""] != 8 {
		t.Fatalf("by side %v, want llama 10556 and 8 unattributed", got)
	}
	if hs := snap.holders(); len(hs) != 1 || hs[0] != "llama" {
		t.Fatalf("holders %v, want [llama]", hs)
	}

	g := &gpu{probe: p}
	g.recordResident(context.Background())
	if g.resident != "llama" {
		t.Fatalf("resident %q after startup, want llama", g.resident)
	}

	got := g.report(g.born, &snap, nil)
	for _, want := range []string{
		"VRAM:           11158 of 12282 MiB used (90%)",
		fmt.Sprintf("llama     10556 MiB  gpu-gate.test (pid %d), qwen", os.Getpid()),
		"other         8 MiB",
		"unlisted    594 MiB",
		"Gate's record:  llama's model is loaded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report lacks %q:\n%s", want, got)
		}
	}
}

// With nothing loaded, or the models of several sides, the record stays
// empty, so the next GPU request unloads the others first.
func TestRecordResidentOnlyTrustsASingleHolder(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	for _, apps := range []string{"", fmt.Sprintf("%d, 512\n", os.Getpid())} {
		p := newVRAMProbe(map[string]string{"llama": ln.Addr().String()})
		p.smi = fakeSMI(t, apps)
		g := &gpu{probe: p}
		g.recordResident(context.Background())
		if g.resident != "" {
			t.Fatalf("apps %q: resident %q, want none", apps, g.resident)
		}
	}
}

// whisker serves its own /status; on its port the gate's page is only at
// /gpu-gate/status.
func TestUpstreamKeepsItsOwnStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("whisker status"))
	}))
	defer upstream.Close()
	g := &gpu{}
	h := g.handler(side{name: "mema", proxy: mustProxy(strings.TrimPrefix(upstream.URL, "http://")), isGPU: memaGPU, upstreamStatus: true})

	for path, want := range map[string]string{"/status": "whisker status", "/gpu-gate/status": "GPU:"} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s got %q, want %q", path, rec.Body.String(), want)
		}
	}
}
