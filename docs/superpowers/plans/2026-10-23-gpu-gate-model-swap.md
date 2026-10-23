# gpu-gate model swap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before a transcription, gpu-gate unloads llama-server's model. After the last transcription, it has mema free its model. That way the 12 GB GPU is actually free for whichever server is working.

**Architecture:** gpu-gate (Go stdlib reverse proxy in `~/c/server/gpu-gate`) keeps its owner/active lock and its immediate 503s. When mema takes a free GPU, the gate first evicts llama's models through the llama.cpp router API (`GET /models`, `POST /models/unload`, poll). When mema's last in-flight request finishes, a goroutine calls a new mema `POST /v1/unload` with the client's `Authorization` header, then releases the GPU. mema (`~/c/mema/server`, FastAPI) gets that endpoint and a `ParakeetOnnxEngine.unload()`.

**Tech Stack:** Go 1.27 stdlib (`net/http`, `httptest`), Python 3 + FastAPI + `unittest`.

**Spec:** `~/c/server/docs/superpowers/specs/2026-10-23-gpu-gate-model-swap-design.md`

**Repos:** both are git-synced to PC-linux (192.168.1.3, SSH alias `pc`), where they run. A commit to `~/c/mema` restarts mema on the PC automatically (its `./activate`). The gate has to be rebuilt and restarted by hand (Task 8).

---

## File structure

| File | Repo | Responsibility |
|---|---|---|
| `server/engines/parakeet_onnx.py` | mema | add `unload()` |
| `server/app.py` | mema | add `POST /v1/unload` |
| `server/tests/test_cuda_engines.py` | mema | tests for `unload()` |
| `server/tests/test_app_http.py` | mema | tests for `/v1/unload` |
| `gpu-gate/main.go` | server | lock reports `first`/`isLast`; handler runs evict/unload hooks; flags and wiring |
| `gpu-gate/llama.go` (new) | server | `llamaModels`: list, unload and wait on the router's models |
| `gpu-gate/mema.go` (new) | server | `memaUnload`: call whisker's `/v1/unload` |
| `gpu-gate/main_test.go` | server | lock and handler tests |
| `gpu-gate/llama_test.go` (new) | server | `llamaModels` tests against a fake router |
| `gpu-gate/mema_test.go` (new) | server | `memaUnload` tests |
| `after-reboot.linux.sh` | server | start the gate with `LLAMA_API_KEY` in its env |

## How to run tests

- Gate: `cd ~/c/server/gpu-gate && go test ./...` (Go downloads its 1.27 toolchain by itself).
- mema, one-time setup of a throwaway venv outside the repo:

```bash
python3 -m venv /tmp/mema-test-venv && /tmp/mema-test-venv/bin/pip -q install fastapi python-multipart httpx anyio numpy
```

  Then: `cd ~/c/mema && /tmp/mema-test-venv/bin/python -m unittest server.tests.test_app_http server.tests.test_cuda_engines -v`

---

### Task 1: mema — `ParakeetOnnxEngine.unload()`

**Files:**
- Modify: `~/c/mema/server/engines/parakeet_onnx.py`
- Test: `~/c/mema/server/tests/test_cuda_engines.py` (class `ParakeetOnnxEngineTests`)

- [ ] **Step 1: Write the failing tests.** Append these methods to `ParakeetOnnxEngineTests`, after `test_transcribe_strips_the_recognized_text`:

```python
    def test_unload_drops_a_loaded_model(self):
        engine = AvailableParakeetOnnxEngine("nemo-parakeet-tdt-0.6b-v3", Path("/tmp/models"))
        engine._model = FakeParakeetModel()

        self.assertTrue(engine.unload())
        self.assertIsNone(engine._model)

    def test_unload_without_a_model_is_a_no_op(self):
        engine = AvailableParakeetOnnxEngine("nemo-parakeet-tdt-0.6b-v3", Path("/tmp/models"))

        self.assertFalse(engine.unload())
        self.assertIsNone(engine._model)
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/c/mema && /tmp/mema-test-venv/bin/python -m unittest server.tests.test_cuda_engines -v`
Expected: the 2 new tests ERROR with `AttributeError: 'AvailableParakeetOnnxEngine' object has no attribute 'unload'`.

- [ ] **Step 3: Implement.** In `server/engines/parakeet_onnx.py`, add `import gc` as the first import line, then add this method to `ParakeetOnnxEngine` right after `transcribe`:

```python
    def unload(self) -> bool:
        """Free the model so its GPU memory goes back to other servers.

        gpu-gate calls this (through POST /v1/unload) after the last
        transcription, so llama-server can load into the card again. The next
        transcribe loads the model again. Returns whether a model was loaded.
        """
        with CUDA_INFERENCE_LOCK:
            if self._model is None:
                return False
            self._model = None
        # The onnxruntime session frees its CUDA arena when it is collected.
        gc.collect()
        return True
```

- [ ] **Step 4: Run the tests and see them pass**

Run: `cd ~/c/mema && /tmp/mema-test-venv/bin/python -m unittest server.tests.test_cuda_engines -v`
Expected: all OK.

- [ ] **Step 5: Hold the commit.** Commit Tasks 1 and 2 together at the end of Task 2: every mema commit restarts the PC's server, and the endpoint is what makes this useful.

---

### Task 2: mema — `POST /v1/unload`

**Files:**
- Modify: `~/c/mema/server/app.py` (add the route after `transcribe`, before `_work_dir`)
- Test: `~/c/mema/server/tests/test_app_http.py`

- [ ] **Step 1: Write the failing tests.** Append this to `test_app_http.py`, before `if __name__ == "__main__":` if there is one, otherwise at the end:

```python
class _UnloadableEngine(_StubEngine):
    def __init__(self, loaded: bool) -> None:
        super().__init__()
        self.loaded = loaded

    def unload(self) -> bool:
        was_loaded, self.loaded = self.loaded, False
        return was_loaded


class UnloadHttpTests(unittest.TestCase):
    def setUp(self) -> None:
        self.client = TestClient(app_module.app)

    def test_unload_requires_token(self):
        self.assertEqual(self.client.post("/v1/unload").status_code, 401)

    def test_unload_frees_loaded_engines_and_skips_the_rest(self):
        loaded = _UnloadableEngine(loaded=True)
        idle = _UnloadableEngine(loaded=False)
        plain = _StubEngine()  # no unload() at all
        engines = {"loaded": loaded, "idle": idle, "plain": plain}
        with mock.patch.dict(app_module._ENGINES, engines, clear=True):
            response = self.client.post("/v1/unload", headers=AUTH)

        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"unloaded": ["stub-engine/stub-model"]})
        self.assertFalse(loaded.loaded)

    def test_unload_with_nothing_built_returns_empty(self):
        with mock.patch.dict(app_module._ENGINES, {}, clear=True):
            response = self.client.post("/v1/unload", headers=AUTH)

        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"unloaded": []})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/c/mema && /tmp/mema-test-venv/bin/python -m unittest server.tests.test_app_http.UnloadHttpTests -v`
Expected: FAIL. The route doesn't exist yet, so you get 404 (or 405) where the tests expect 401/200.

- [ ] **Step 3: Implement.** In `server/app.py`, add this right after the `transcribe` function and before `def _work_dir`:

```python
@app.post("/v1/unload", dependencies=[Depends(authorize)])
async def unload() -> dict:
    """Free every loaded model so another server can use the GPU.

    gpu-gate calls this after the last transcription, so llama-server can
    load into the card again. Engines without unload() are skipped, and
    the next transcription loads the model again.
    """
    unloaded = []
    for engine in list(_ENGINES.values()):
        free = getattr(engine, "unload", None)
        if free is not None and await anyio.to_thread.run_sync(free):
            unloaded.append(f"{engine.name}/{engine.model}")
    logger.info("unload freed=%s", unloaded)
    return {"unloaded": unloaded}
```

- [ ] **Step 4: Run the whole mema suite and check it passes**

Run: `cd ~/c/mema && /tmp/mema-test-venv/bin/python -m unittest discover server/tests -v 2>&1 | tail -5`
Expected: `OK`. If a test module that needs macOS-only or GPU packages (MLX, onnx) fails to import in the throwaway venv, run the two modules from Task 1 and this task instead. Report which modules were skipped.

- [ ] **Step 5: Commit** (this restarts mema on the PC through git-sync's `./activate`):

```bash
cd ~/c/mema && git add server/engines/parakeet_onnx.py server/app.py server/tests/test_cuda_engines.py server/tests/test_app_http.py && git commit -m "Add POST /v1/unload so gpu-gate can free the ASR model's GPU memory

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: gate — lock reports `first` and `isLast`

**Files:**
- Modify: `~/c/server/gpu-gate/main.go` (`acquire`, add `isLast`)
- Test: `~/c/server/gpu-gate/main_test.go`

- [ ] **Step 1: Write the failing test** and update the existing callers. In `main_test.go`, change every `if ok, _ := g.acquire(` to `if ok, _, _ := g.acquire(`, and the line `if ok, heldBy := g.acquire("mema"); ok || heldBy != "llama" {` to `if ok, heldBy, _ := g.acquire("mema"); ok || heldBy != "llama" {`. Then add:

```go
// first marks the request that took a free GPU (when the other server must be
// evicted); isLast marks the last in-flight request of the owner.
func TestAcquireReportsFirstAndLast(t *testing.T) {
	g := &gpu{}

	if _, _, first := g.acquire("mema"); !first {
		t.Fatal("taking a free GPU should be first")
	}
	if _, _, first := g.acquire("mema"); first {
		t.Fatal("joining mema's own in-flight request is not first")
	}
	if g.isLast("mema") {
		t.Fatal("two mema requests in flight: not last")
	}
	g.release("mema")
	if !g.isLast("mema") {
		t.Fatal("one mema request left: should be last")
	}
	if g.isLast("llama") {
		t.Fatal("llama does not own the GPU")
	}
	g.release("mema")
	if _, _, first := g.acquire("llama"); !first {
		t.Fatal("llama taking the freed GPU should be first")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: build failure, `assignment mismatch: 3 variables but g.acquire returns 2 values` and `g.isLast undefined`.

- [ ] **Step 3: Implement.** In `main.go`, replace `acquire` and add `isLast` right after `release`:

```go
// acquire reserves the GPU for side, or reports who already holds it. It
// succeeds when the GPU is free or already held by this same side. first is
// true when this call took a free GPU: the moment the other server's model
// has to leave VRAM.
func (g *gpu) acquire(side string) (ok bool, heldBy string, first bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == 0 {
		g.owner = side
		first = true
	} else if g.owner != side {
		return false, g.owner, false
	}
	g.active++
	return true, "", first
}
```

```go
// isLast reports whether the caller holds side's only in-flight request, so
// the side can free its model before the GPU is released.
func (g *gpu) isLast(side string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owner == side && g.active == 1
}
```

In `handler`, change `ok, heldBy := g.acquire(s.name)` to `ok, heldBy, _ := g.acquire(s.name)` so it compiles (Task 6 rewrites it).

- [ ] **Step 4: Run the tests and check they pass**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: `ok  	gpu-gate`

- [ ] **Step 5: Commit**

```bash
cd ~/c/server && git add gpu-gate/main.go gpu-gate/main_test.go && git commit -m "gpu-gate: report the first and last request of a GPU owner

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: gate — evict llama's models (`llama.go`)

**Files:**
- Create: `~/c/server/gpu-gate/llama.go`
- Test: `~/c/server/gpu-gate/llama_test.go`

- [ ] **Step 1: Write the failing tests.** Create `llama_test.go`:

```go
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
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: build failure, `undefined: modelList`, `undefined: routerModel`, `undefined: llamaModels`.

- [ ] **Step 3: Implement.** Create `llama.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// llamaModels drives the llama.cpp router's model API, so the gate can push
// llama's model out of VRAM before a transcription. The router loads it again
// by itself on the next chat request (autoload is on).
type llamaModels struct {
	base    string // "http://127.0.0.1:8081"
	key     string // the router's --api-key; empty sends no auth
	client  *http.Client
	poll    time.Duration // how often to re-check /models after unloading
	timeout time.Duration // give up on the whole eviction after this long
}

type routerModel struct {
	ID     string `json:"id"`
	Status struct {
		Value string `json:"value"` // "loaded", "loading" or "unloaded"
	} `json:"status"`
}

type modelList struct {
	Data []routerModel `json:"data"`
}

// errUnreachable means nothing answered at all: llama is down, so it holds no
// GPU memory.
var errUnreachable = errors.New("llama-server unreachable")

// evict unloads every resident model and waits until the router reports none,
// so the GPU memory is free when it returns nil. A router that does not answer
// at all holds nothing, so that also returns nil.
func (l *llamaModels) evict(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	ids, err := l.resident(ctx)
	if errors.Is(err, errUnreachable) {
		log.Printf("%v; forwarding the transcription anyway", err)
		return nil
	}
	if err != nil || len(ids) == 0 {
		return err
	}
	log.Printf("unloading llama models %v for whisker", ids)
	for _, id := range ids {
		if err := l.unload(ctx, id); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("llama models %v still loaded after %s", ids, l.timeout)
		case <-time.After(l.poll):
		}
		if ids, err = l.resident(ctx); err != nil || len(ids) == 0 {
			return err
		}
	}
}

// resident lists the models that are loaded or loading.
func (l *llamaModels) resident(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/models", nil)
	if err != nil {
		return nil, err
	}
	l.auth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models: status %d", resp.StatusCode)
	}
	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("GET /models: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		if m.Status.Value == "loaded" || m.Status.Value == "loading" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (l *llamaModels) unload(ctx context.Context, id string) error {
	body, _ := json.Marshal(map[string]string{"model": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/models/unload", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	l.auth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("unload %s: %v", id, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload %s: status %d", id, resp.StatusCode)
	}
	return nil
}

func (l *llamaModels) auth(req *http.Request) {
	if l.key != "" {
		req.Header.Set("Authorization", "Bearer "+l.key)
	}
}
```

- [ ] **Step 4: Run the tests and check they pass**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: `ok  	gpu-gate`

- [ ] **Step 5: Commit**

```bash
cd ~/c/server && git add gpu-gate/llama.go gpu-gate/llama_test.go && git commit -m "gpu-gate: evict llama's models through the router API

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: gate — ask mema to unload (`mema.go`)

**Files:**
- Create: `~/c/server/gpu-gate/mema.go`
- Test: `~/c/server/gpu-gate/mema_test.go`

- [ ] **Step 1: Write the failing tests.** Create `mema_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMemaUnloadPassesTheClientsAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"unloaded":["parakeet-onnx/parakeet-tdt-0.6b-v3"]}`))
	}))
	defer srv.Close()

	if err := memaUnload(srv.Client(), srv.URL, "Bearer client-token"); err != nil {
		t.Fatalf("memaUnload: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/unload" || gotAuth != "Bearer client-token" {
		t.Fatalf("got %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
}

func TestMemaUnloadReportsAFailedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if err := memaUnload(srv.Client(), srv.URL, ""); err == nil {
		t.Fatal("a 401 should be an error")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: build failure, `undefined: memaUnload`.

- [ ] **Step 3: Implement.** Create `mema.go`:

```go
package main

import (
	"fmt"
	"net/http"
)

// memaUnload asks whisker to free its ASR model once its last transcription
// is done. It passes along the client's own Authorization header, so the gate
// never needs whisker's token.
func memaUnload(client *http.Client, base, authorization string) error {
	req, err := http.NewRequest(http.MethodPost, base+"/v1/unload", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /v1/unload: status %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests and check they pass**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: `ok  	gpu-gate`

- [ ] **Step 5: Commit**

```bash
cd ~/c/server && git add gpu-gate/mema.go gpu-gate/mema_test.go && git commit -m "gpu-gate: client for whisker's /v1/unload

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: gate — handler runs the evict and unload hooks

**Files:**
- Modify: `~/c/server/gpu-gate/main.go` (`side`, `handler`, new `refuse`)
- Test: `~/c/server/gpu-gate/main_test.go`

- [ ] **Step 1: Write the failing tests.** In `main_test.go`, add `"context"`, `"errors"`, `"sync"` and `"time"` to the imports. Then append:

```go
// waitFree waits for the background unload to release the GPU.
func waitFree(t *testing.T, g *gpu) {
	t.Helper()
	for i := 0; i < 200; i++ {
		g.mu.Lock()
		free := g.active == 0
		g.mu.Unlock()
		if free {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("GPU was never released")
}

// swapSide builds a mema side whose upstream and hooks append to steps.
func swapSide(t *testing.T, steps *[]string, mu *sync.Mutex, evictErr error, unloadGate chan struct{}) side {
	t.Helper()
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		*steps = append(*steps, s)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		record("transcribe")
		_, _ = io.WriteString(w, "text")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	return side{
		name:     "mema",
		proxy:    httputil.NewSingleHostReverseProxy(u),
		isGPU:    memaGPU,
		busyMsg:  `{"detail":"GPU not available"}`,
		evictMsg: `{"detail":"could not free the GPU"}`,
		evictOther: func(context.Context) error {
			record("evict")
			return evictErr
		},
		unloadSelf: func(auth string) error {
			if unloadGate != nil {
				<-unloadGate
			}
			record("unload " + auth)
			return nil
		},
	}
}

func transcribeReq() *http.Request {
	r := httptest.NewRequest("POST", "/v1/transcribe", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer client")
	return r
}

func TestHandlerEvictsThenTranscribesThenUnloads(t *testing.T) {
	var steps []string
	var mu sync.Mutex
	g := &gpu{}
	h := g.handler(swapSide(t, &steps, &mu, nil, nil))

	rec := httptest.NewRecorder()
	h(rec, transcribeReq())
	if rec.Code != http.StatusOK || rec.Body.String() != "text" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	waitFree(t, g)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"evict", "transcribe", "unload Bearer client"}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %v, want %v", steps, want)
	}
}

func TestHandlerRefusesWhenEvictFails(t *testing.T) {
	var steps []string
	var mu sync.Mutex
	g := &gpu{}
	h := g.handler(swapSide(t, &steps, &mu, errors.New("still loaded"), nil))

	rec := httptest.NewRecorder()
	h(rec, transcribeReq())
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "could not free") {
		t.Fatalf("got %d %q, want 503 evictMsg", rec.Code, rec.Body.String())
	}
	if ok, _, _ := g.acquire("llama"); !ok {
		t.Fatal("GPU should be free after a failed eviction")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(steps, ",") != "evict" {
		t.Fatalf("steps %v, want only evict", steps)
	}
}

// A request that joins mema's own in-flight one neither evicts nor unloads;
// the request that finishes last does the unloading.
func TestHandlerJoiningRequestSkipsEvictAndUnload(t *testing.T) {
	var steps []string
	var mu sync.Mutex
	g := &gpu{}
	h := g.handler(swapSide(t, &steps, &mu, nil, nil))

	g.acquire("mema") // another transcription is in flight
	rec := httptest.NewRecorder()
	h(rec, transcribeReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	mu.Lock()
	got := strings.Join(steps, ",")
	mu.Unlock()
	if got != "transcribe" {
		t.Fatalf("steps %v, want only transcribe", got)
	}
	if !g.isLast("mema") {
		t.Fatal("the other mema request should still hold the GPU")
	}
}

// While mema is still freeing its model, llama's GPU requests are refused;
// once the unload finishes they go through.
func TestHandlerHoldsGPUUntilUnloadFinishes(t *testing.T) {
	var steps []string
	var mu sync.Mutex
	unloadGate := make(chan struct{})
	g := &gpu{}
	h := g.handler(swapSide(t, &steps, &mu, nil, unloadGate))

	llamaUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "answer")
	}))
	defer llamaUp.Close()
	lu, _ := url.Parse(llamaUp.URL)
	llama := g.handler(side{
		name:    "llama",
		proxy:   httputil.NewSingleHostReverseProxy(lu),
		isGPU:   llamaGPU,
		busyMsg: `{"error":"busy"}`,
	})
	chat := func() int {
		rec := httptest.NewRecorder()
		llama(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
		return rec.Code
	}

	rec := httptest.NewRecorder()
	h(rec, transcribeReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("transcribe got %d", rec.Code)
	}
	if code := chat(); code != http.StatusServiceUnavailable {
		t.Fatalf("chat during unload got %d, want 503", code)
	}
	close(unloadGate)
	waitFree(t, g)
	if code := chat(); code != http.StatusOK {
		t.Fatalf("chat after unload got %d, want 200", code)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/c/server/gpu-gate && go test ./...`
Expected: build failure, `unknown field evictMsg in struct literal`, `unknown field evictOther`, `unknown field unloadSelf`.

- [ ] **Step 3: Implement.** In `main.go`, add `"context"` to the imports. Replace the `side` struct:

```go
// side is one gated server: its name, the upstream proxy, the predicate that
// decides which requests use the GPU, and the body to return when locked out.
type side struct {
	name    string
	proxy   *httputil.ReverseProxy
	isGPU   func(*http.Request) bool
	busyMsg string // JSON; names the *other* server

	// evictOther, when set, frees the other server's GPU memory before the
	// request that takes a free GPU runs. If it fails, the request is refused
	// with evictMsg.
	evictOther func(context.Context) error
	evictMsg   string
	// unloadSelf, when set, frees this server's model after its last in-flight
	// request, before the GPU is released. It gets that request's
	// Authorization header.
	unloadSelf func(authorization string) error
}
```

Replace `handler` and add `refuse` after it:

```go
func (g *gpu) handler(s side) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Gate itself answers its own health, so a probe never depends on the
		// upstream being up.
		if r.URL.Path == "/gpu-gate/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		if !s.isGPU(r) {
			s.proxy.ServeHTTP(w, r)
			return
		}
		ok, heldBy, first := g.acquire(s.name)
		if !ok {
			refuse(w, s.busyMsg)
			log.Printf("%s request refused: GPU held by %s", s.name, heldBy)
			return
		}
		if first && s.evictOther != nil {
			if err := s.evictOther(r.Context()); err != nil {
				g.release(s.name)
				refuse(w, s.evictMsg)
				log.Printf("%s request refused: %v", s.name, err)
				return
			}
		}
		// Deferred so a proxy panic (http.ErrAbortHandler) still releases.
		defer func() {
			if s.unloadSelf == nil || !g.isLast(s.name) {
				g.release(s.name)
				return
			}
			// Off the request, since the client already has its answer. The
			// GPU stays held until the model is gone, so the other server
			// cannot start loading into memory that is still being freed.
			auth := r.Header.Get("Authorization")
			go func() {
				if err := s.unloadSelf(auth); err != nil {
					log.Printf("%s unload failed: %v", s.name, err)
				}
				g.release(s.name)
			}()
		}()
		s.proxy.ServeHTTP(w, r)
	}
}

func refuse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(body))
}
```

- [ ] **Step 4: Run the tests, with the race detector, and check they pass**

Run: `cd ~/c/server/gpu-gate && go test -race -count=3 ./...`
Expected: `ok  	gpu-gate`, with no `DATA RACE` reports. The existing `TestHandlerRefusesWhenOtherSideBusy` must still pass: its side has no hooks.

- [ ] **Step 5: Commit**

```bash
cd ~/c/server && git add gpu-gate/main.go gpu-gate/main_test.go && git commit -m "gpu-gate: evict the other server before taking the GPU, unload after

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: gate — wire it up in `main()` and the boot script

**Files:**
- Modify: `~/c/server/gpu-gate/main.go` (package doc comment, `main`)
- Modify: `~/c/server/after-reboot.linux.sh` (gate section and header comment)

- [ ] **Step 1: Update the package doc comment.** In `main.go`, replace the paragraph that starts `// owns the public port in front of it. While one server is answering a` and ends `// discovery keep working no matter which server holds the GPU.` with:

```go
// owns the public port in front of it. While one server is answering a
// GPU-bound request, a GPU-bound request to the other is refused immediately
// with a hardcoded "GPU not available" 503, instead of both fighting over the
// card. Health, model-list and status endpoints are never gated, so probes and
// discovery keep working no matter which server holds the GPU.
//
// Holding the lock is not enough on a 12 GB card: an idle llama keeps its
// model resident and whisker cannot load. So when a transcription takes a
// free GPU, the gate first unloads llama's models through the router API and
// waits until they are gone. When the last transcription finishes, it asks
// whisker to free its model (POST /v1/unload) before releasing the GPU. llama
// reloads on its next chat request.
```

- [ ] **Step 2: Wire `main()`.** Add `"os"` and `"time"` to the imports. In `main`, add this flag after `memaUpstream`:

```go
	llamaKeyEnv := flag.String("llama-key-env", "LLAMA_API_KEY", "env var holding llama-server's --api-key, used to unload its models")
```

After `g := &gpu{}`, add:

```go
	llamaKey := os.Getenv(*llamaKeyEnv)
	if llamaKey == "" {
		log.Printf("warning: $%s is empty; unloading llama models without auth", *llamaKeyEnv)
	}
	models := &llamaModels{
		base:    "http://" + *llamaUpstream,
		key:     llamaKey,
		client:  &http.Client{Timeout: 10 * time.Second},
		poll:    250 * time.Millisecond,
		timeout: 20 * time.Second,
	}
	memaBase := "http://" + *memaUpstream
	memaClient := &http.Client{Timeout: 30 * time.Second}
```

Replace the `mema := side{...}` literal with:

```go
	mema := side{
		name:       "mema",
		proxy:      mustProxy(*memaUpstream),
		isGPU:      memaGPU,
		busyMsg:    `{"detail":"GPU not available: llama-server is using the GPU. Try again in a moment."}`,
		evictOther: models.evict,
		evictMsg:   `{"detail":"GPU not available: could not free the GPU from llama-server. Try again in a moment."}`,
		unloadSelf: func(auth string) error { return memaUnload(memaClient, memaBase, auth) },
	}
```

- [ ] **Step 3: Build, vet and test**

Run: `cd ~/c/server/gpu-gate && go vet ./... && go test -race ./... && go build -o /tmp/gpu-gate-check . && rm /tmp/gpu-gate-check`
Expected: no vet output, `ok  	gpu-gate`, and the build succeeds.

- [ ] **Step 4: Give the gate llama's key at boot.** In `after-reboot.linux.sh`, replace the line

```bash
  tmux respawn-pane -k -t '=gpu-gate:' "exec '$GATE_BIN'"
```

with

```bash
  # LLAMA_API_KEY lets the gate unload llama's models before a transcription.
  tmux respawn-pane -k -t '=gpu-gate:' \
    "set -a; . '$LLAMA_ENV'; set +a; exec '$GATE_BIN'"
```

In the header comment, replace these lines:

```bash
#   - gpu-gate : reverse proxy owning the public ports (8080 for llama, 8787 for
#                mema). It serializes the GPU: while one server answers a
#                GPU request the other gets a hardcoded "GPU not available" 503.
#                See gpu-gate/main.go.
```

with

```bash
#   - gpu-gate : reverse proxy owning the public ports (8080 for llama, 8787 for
#                mema). It serializes the GPU: while one server answers a
#                GPU request the other gets a hardcoded "GPU not available" 503.
#                Before a transcription it unloads llama's model (llama reloads
#                on its next chat), and after it has mema free its own.
#                See gpu-gate/main.go.
```

- [ ] **Step 5: Lint the script**

Run: `shellcheck ~/c/server/after-reboot.linux.sh`
Expected: no new warnings compared with `git stash; shellcheck ~/c/server/after-reboot.linux.sh; git stash pop`.

- [ ] **Step 6: Commit**

```bash
cd ~/c/server && git add gpu-gate/main.go after-reboot.linux.sh && git commit -m "gpu-gate: wire model swapping, pass LLAMA_API_KEY at boot

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Deploy on the PC and check it end to end

Restarting gpu-gate on the PC is a remote, service-affecting action. **Ask the user before Step 3.**

- [ ] **Step 1: Confirm both commits reached the PC**

Run: `command ssh -o BatchMode=yes -o ConnectTimeout=10 pc 'git -C ~/c/mema log -1 --oneline; git -C ~/c/server log -1 --oneline'`
Expected: the commits from Task 2 and Task 7. If not, wait a minute for git-sync and re-check; don't push from the PC.

- [ ] **Step 2: Confirm mema restarted with the endpoint.** Asking without a token proves the route exists (401 rather than 404):

Run: `command ssh -o BatchMode=yes -o ConnectTimeout=10 pc 'curl -s -o /dev/null -w "%{http_code}\n" -X POST 127.0.0.1:8788/v1/unload'`
Expected: `401`. If it's `404`, mema didn't restart. Run `cd ~/c/mema && ./serve` on the PC, after asking.

- [ ] **Step 3: Rebuild and restart the gate (after the user agrees)**

```bash
command ssh -o BatchMode=yes -o ConnectTimeout=10 pc 'cd ~/c/server/gpu-gate && go build -o gpu-gate . && tmux respawn-pane -k -t "=gpu-gate:" "set -a; . ~/.config/llama-server/env; set +a; exec ~/c/server/gpu-gate/gpu-gate" && sleep 1 && curl -s 127.0.0.1:8080/gpu-gate/health && tmux capture-pane -p -t gpu-gate | tail -5'
```

Expected: `ok`, and the pane shows `llama gate ...` and `mema gate ...` with no `warning: $LLAMA_API_KEY is empty`.

- [ ] **Step 4: Make sure llama is loaded, then dictate.** Load llama with one tiny chat through the gate:

```bash
command ssh -o BatchMode=yes -o ConnectTimeout=10 pc 'set -a; . ~/.config/llama-server/env; set +a; curl -s -m 60 -H "Authorization: Bearer $LLAMA_API_KEY" -H "Content-Type: application/json" 127.0.0.1:8080/v1/chat/completions -d "{\"model\":\"Qwen3.6-35B-A3B-abliterated-i1-Q4_K_M\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":4}" >/dev/null; nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv'
```

Expected: a `llama-server` child using ~10 GB.

Then dictate from Obsidian on immunemac (open a note, Record, speak, Stop). Expected: "Transcribing…", then the text appears at the cursor.

- [ ] **Step 5: Check the swap happened**

Run: `command ssh -o BatchMode=yes -o ConnectTimeout=10 pc 'tmux capture-pane -p -J -t gpu-gate -S -20 | tail -8; tmux capture-pane -p -J -t mema -S -40 | grep -E "transcribe|unload" | tail -4; nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv'`
Expected:
- the gate log shows `unloading llama models [...] for whisker`;
- mema shows `POST /v1/transcribe ... 200` and `unload freed=['parakeet-onnx/parakeet-tdt-0.6b-v3']`;
- `nvidia-smi` has no llama child, and mema's process is back near its CUDA-context baseline (a few hundred MB). Write down the actual figure.

If mema's memory stays high (>1.5 GB) after unload, the onnxruntime session didn't hand its arena back. Report that to the user rather than working around it.

- [ ] **Step 6: Check llama comes back.** Run the chat command from Step 4 again. Expected: it answers after ~8 s, and `nvidia-smi` shows the llama child again.

- [ ] **Step 7: Report.** Tell the user what each check showed, including mema's post-unload memory figure, and that the Obsidian plugin change in the notes vault is still uncommitted.
