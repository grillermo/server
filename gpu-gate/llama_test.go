package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeRouter mimics the llama.cpp router's model API: a model reports
// "loaded" until it is unloaded.
type fakeRouter struct {
	mu         sync.Mutex
	loaded     map[string]bool
	unloads    []string
	stuck      bool // unload is accepted but the model never leaves
	failUnload bool
	gotAuth    string
}

func (f *fakeRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/models":
		var list modelList
		for id, on := range f.loaded {
			m := routerModel{ID: id}
			m.Status.Value = "unloaded"
			if on {
				m.Status.Value = "loaded"
			}
			list.Data = append(list.Data, m)
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPost && r.URL.Path == "/models/unload":
		if f.failUnload {
			http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.unloads = append(f.unloads, body.Model)
		if !f.stuck {
			f.loaded[body.Model] = false
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	default:
		http.NotFound(w, r)
	}
}

func newModels(t *testing.T, f *fakeRouter) *llamaModels {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &llamaModels{
		base:    srv.URL,
		key:     "k",
		client:  srv.Client(),
		poll:    5 * time.Millisecond,
		timeout: 200 * time.Millisecond,
	}
}

func TestEvictUnloadsResidentModels(t *testing.T) {
	f := &fakeRouter{loaded: map[string]bool{"qwen": true, "other": false}}
	if err := newModels(t, f).evict(context.Background()); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if !reflect.DeepEqual(f.unloads, []string{"qwen"}) {
		t.Fatalf("unloaded %v, want [qwen]", f.unloads)
	}
	if f.gotAuth != "Bearer k" {
		t.Fatalf("auth header %q, want Bearer k", f.gotAuth)
	}
}

func TestEvictWithNothingLoadedUnloadsNothing(t *testing.T) {
	f := &fakeRouter{loaded: map[string]bool{"qwen": false}}
	if err := newModels(t, f).evict(context.Background()); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if len(f.unloads) != 0 {
		t.Fatalf("unloaded %v, want none", f.unloads)
	}
}

func TestEvictGivesUpWhenModelNeverLeaves(t *testing.T) {
	f := &fakeRouter{loaded: map[string]bool{"qwen": true}, stuck: true}
	if err := newModels(t, f).evict(context.Background()); err == nil {
		t.Fatal("evict should fail when the model stays loaded")
	}
}

func TestEvictFailsWhenUnloadIsRejected(t *testing.T) {
	f := &fakeRouter{loaded: map[string]bool{"qwen": true}, failUnload: true}
	if err := newModels(t, f).evict(context.Background()); err == nil {
		t.Fatal("evict should fail when /models/unload errors")
	}
}

// A router that does not answer is down and holds no GPU memory, so the
// transcription may go ahead.
func TestEvictSucceedsWhenLlamaIsDown(t *testing.T) {
	l := &llamaModels{
		base:    "http://127.0.0.1:1",
		client:  &http.Client{Timeout: time.Second},
		poll:    5 * time.Millisecond,
		timeout: 200 * time.Millisecond,
	}
	if err := l.evict(context.Background()); err != nil {
		t.Fatalf("evict with llama down: %v", err)
	}
}
