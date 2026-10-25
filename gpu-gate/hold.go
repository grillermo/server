package main

import (
	"log"
	"net/http"
)

// holdHandler serves a side whose GPU work is not one HTTP request but a
// background job, like simple-enhance's minutes-long enhancements. Instead of
// proxying, it hands out a lease: POST /gpu-gate/hold acquires the GPU for
// s.name (unloading the others first), answers 200 at once, and keeps the
// response open; the GPU is released when the client closes the connection,
// so a client that crashes or is killed cannot keep it. While another server
// holds the GPU the lease is refused with s.busyMsg, and the client retries.
func (g *gpu) holdHandler(s side) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gpu-gate/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		case r.URL.Path != "/gpu-gate/hold":
			http.NotFound(w, r)
			return
		case r.Method != http.MethodPost:
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ok, heldBy := g.acquire(s.name)
		if !ok {
			refuse(w, s.busyMsg)
			log.Printf("%s hold refused: GPU held by %s", s.name, heldBy)
			return
		}
		defer g.release(s.name)
		if err := g.ready(s.name, s.evictOther); err != nil {
			refuse(w, s.evictMsg)
			log.Printf("%s hold refused: %v", s.name, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			log.Printf("%s hold: %v", s.name, err)
			return
		}
		log.Printf("%s holds the GPU", s.name)
		<-r.Context().Done()
		log.Printf("%s released the GPU", s.name)
	}
}
