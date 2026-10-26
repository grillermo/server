package main

import (
	"fmt"
	"net/http"
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
// depend on an upstream being up. It reports whether it answered.
func (g *gpu) serveOwn(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/gpu-gate/health":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	case "/status", "/gpu-gate/status":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(g.report(time.Now())))
	default:
		return false
	}
	return true
}

// report is the human-readable status page.
func (g *gpu) report(now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder

	if g.active == 0 {
		b.WriteString("GPU:            free\n")
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
	if g.resident == "" {
		b.WriteString("Model in VRAM:  unknown (no switch since the gate started)\n")
	} else {
		fmt.Fprintf(&b, "Model in VRAM:  %s\n", g.resident)
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
