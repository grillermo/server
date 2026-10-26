package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// exchange is one GPU request as /status reports it: the latest one to take
// the GPU, or the last one to finish.
type exchange struct {
	side     string
	method   string
	path     string
	started  time.Time
	finished time.Time
	status   int    // 0 if no response was written
	note     string // why it was refused, if it was
}

// statusWriter remembers the status code the proxy answered with. Unwrap
// keeps http.ResponseController's Flush working, so SSE still streams.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// took notes that a request of side now holds the GPU.
func (g *gpu) took(side string, r *http.Request, started time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.latest = exchange{side: side, method: r.Method, path: r.URL.Path, started: started}
}

// finished records the response a GPU request of side got.
func (g *gpu) finished(side string, r *http.Request, started time.Time, status int, note string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = exchange{
		side: side, method: r.Method, path: r.URL.Path,
		started: started, finished: time.Now(), status: status, note: note,
	}
}

// serveOwn answers the gate's own endpoints, on every port, so they never
// depend on an upstream being up. It reports whether it answered. A side whose
// upstream has its own /status (whisker) keeps it; the gate's page is then at
// /gpu-gate/status only.
func (g *gpu) serveOwn(w http.ResponseWriter, r *http.Request, s side) bool {
	switch {
	case r.URL.Path == "/gpu-gate/health":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	case r.URL.Path == "/gpu-gate/status", r.URL.Path == "/status" && !s.upstreamStatus:
		var snap *vramSnap
		var err error
		if g.probe != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			var v vramSnap
			v, err = g.probe.snapshot(ctx)
			cancel()
			snap = &v
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(g.report(time.Now(), snap, err)))
	default:
		return false
	}
	return true
}

// report is the human-readable status page. snap is what the driver says is
// in VRAM, nil when the gate has no probe.
func (g *gpu) report(now time.Time, snap *vramSnap, probeErr error) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder

	if g.active == 0 {
		b.WriteString("GPU:            free (no GPU request in flight)\n")
	} else {
		fmt.Fprintf(&b, "GPU:            held by %s, %s in flight, for %s\n",
			g.owner, plural(g.active, "request"), span(now.Sub(g.since)))
		if l := g.latest; l.side == g.owner && !l.started.IsZero() {
			fmt.Fprintf(&b, "                latest: %s %s, started %s ago\n", l.method, l.path, span(now.Sub(l.started)))
		}
	}
	if g.swap != nil {
		fmt.Fprintf(&b, "Switching:      unloading the others for %s\n", g.owner)
	}

	switch {
	case probeErr != nil:
		fmt.Fprintf(&b, "VRAM:           unknown (%v)\n", probeErr)
	case snap != nil:
		writeVRAM(&b, *snap)
	}
	if g.resident == "" {
		b.WriteString("Gate's record:  none; the next GPU request unloads every other server first\n")
	} else {
		fmt.Fprintf(&b, "Gate's record:  %s's model is loaded\n", g.resident)
	}

	b.WriteString("\nLast GPU request:\n")
	l := g.last
	if l.finished.IsZero() {
		fmt.Fprintf(&b, "  none since the gate started, %s ago\n", span(now.Sub(g.born)))
		return b.String()
	}
	fmt.Fprintf(&b, "  %s  %s %s\n", l.side, l.method, l.path)
	switch {
	case l.note != "":
		fmt.Fprintf(&b, "  -> %d %s (%s)\n", l.status, http.StatusText(l.status), l.note)
	case l.status == 0:
		b.WriteString("  -> no response (the client went away)\n")
	default:
		fmt.Fprintf(&b, "  -> %d %s\n", l.status, http.StatusText(l.status))
	}
	fmt.Fprintf(&b, "  took %s, finished %s ago (%s)\n",
		span(l.finished.Sub(l.started)), span(now.Sub(l.finished)), l.finished.Format("Jan 2 15:04:05"))
	return b.String()
}

// writeVRAM lists who holds the VRAM, biggest first, one line per process.
func writeVRAM(b *strings.Builder, s vramSnap) {
	pct := 0
	if s.totalMiB > 0 {
		pct = s.usedMiB * 100 / s.totalMiB
	}
	fmt.Fprintf(b, "VRAM:           %d of %d MiB used (%d%%)\n", s.usedMiB, s.totalMiB, pct)
	procs := append([]vramUse(nil), s.procs...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].mib > procs[j].mib })
	listed := 0
	for _, u := range procs {
		who := u.side
		if who == "" {
			who = "other"
		}
		fmt.Fprintf(b, "  %-8s %6d MiB  %s (pid %d)", who, u.mib, u.comm, u.pid)
		if ids := s.models[u.side]; len(ids) > 0 && u.side != "" {
			fmt.Fprintf(b, ", %s", strings.Join(ids, ", "))
		}
		b.WriteString("\n")
		listed += u.mib
	}
	if rest := s.usedMiB - listed; rest > 0 {
		fmt.Fprintf(b, "  %-8s %6d MiB  driver, desktop and processes nvidia-smi does not list\n", "unlisted", rest)
	}
}

// span rounds a duration to what a person cares about: 340ms, 4.2s, 3m12s, 2h5m.
func span(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// recordResident sets the gate's record of whose model is loaded from what is
// really in VRAM, so a gate started while a model is loaded neither unloads
// it needlessly nor lets another server load on top of it. With no model, or
// models of several sides, the record stays empty and the next GPU request
// unloads the others first.
func (g *gpu) recordResident(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snap, err := g.probe.snapshot(ctx)
	if err != nil {
		log.Printf("cannot tell whose model is in VRAM: %v", err)
		return
	}
	hs := snap.holders()
	if len(hs) != 1 {
		log.Printf("models in VRAM at startup: %v; the next GPU request unloads the others", hs)
		return
	}
	g.mu.Lock()
	g.resident = hs[0]
	g.mu.Unlock()
	log.Printf("%s's model is already in VRAM (%d MiB)", hs[0], snap.byside()[hs[0]])
}
