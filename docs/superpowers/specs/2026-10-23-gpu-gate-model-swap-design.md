# gpu-gate: hand the GPU between llama-server and mema by unloading models

Repos touched: `~/c/server` (gpu-gate, after-reboot.linux.sh) and `~/c/mema`
(unload endpoint). Runs on PC-linux (RTX 4070, 12 GB).

## Problem

gpu-gate only serializes in-flight requests. llama-server keeps its model
resident while idle (10.3 of 12 GB), so mema's Parakeet model fails to load
(onnxruntime BFCArena "Failed to allocate memory") and `/v1/transcribe` returns
500. Clients such as the Obsidian Whisper plugin see nothing happen.

## Facts

- llama: llama.cpp router on 127.0.0.1:8081, `--models-max 1`, `--api-key`
  from `~/.config/llama-server/env` (`LLAMA_API_KEY`), presets in
  `~/.config/llama-server/models.ini`. `GET /models` lists every preset with
  `status.value` of `loaded`, `loading` or `unloaded`.
  `POST /models/unload {"model": "<id>"}` frees one. Autoload is on, so the next
  request reloads the model (~8 s from page cache).
- mema: FastAPI on 127.0.0.1:8788, auth by `WHISKER_AUTH_TOKEN` bearer token.
  `ParakeetOnnxEngine` lazy-loads on first transcribe, under
  `CUDA_INFERENCE_LOCK`, and never frees it. There is no unload endpoint.
- gpu-gate is already the only public entry point: :8080 → llama, :8787 →
  mema. Both upstreams listen on loopback only.

## Design

### Gate state (`gpu-gate/main.go`)

Keep the `owner`/`active` lock and the immediate 503 when the other side has a
request in flight. Add a swap step that runs when ownership changes hands.

### Transcription takes the GPU

1. `acquire(mema)`. If llama has a request in flight → existing 503.
2. If this request made mema the owner (`active` went 0 → 1): `GET
   :8081/models` with `Authorization: Bearer $LLAMA_API_KEY`. For each model
   whose status is `loaded` or `loading`, `POST /models/unload`. Then poll
   `/models` every 250 ms until none is `loaded`/`loading`, for at most 20 s.
3. Proxy the request to mema as today.
4. ~~When the last mema request finishes, unload mema~~ (superseded
   2026-10-24, see Revision below).

### Chat takes the GPU

~~No new behaviour~~ (superseded): if mema's model is resident, the gate calls
`POST :8788/v1/unload` first, then proxies; the router autoloads llama.

### Revision 2026-10-24: unload only on a switch

The user asked that a model be unloaded only when the *other* server needs the
GPU, so back-to-back requests to one server never pay the loading cost:

- The gate keeps `resident` (whose model may be in VRAM; `""` at startup, so
  the first request evicts whatever is there). The first GPU request whose side
  is not resident runs the eviction of the other side, then sets `resident`.
- Requests of the same side that arrive while that eviction runs wait for it
  (they hold the GPU, so the other side still gets the immediate 503). If the
  eviction fails, they are refused too.
- Nothing is unloaded after a request finishes.
- Unloading mema before a chat uses whisker's own token, from
  `-mema-token-env` (default `WHISKER_AUTH_TOKEN`); the boot script sources
  `~/c/mema/server/.env` for it. A chat goes ahead even if that unload fails,
  since llama's `fit = on` sizes itself to the free memory.
- mema's `/v1/unload` frees every CUDA engine that has `unload()` (Parakeet,
  faster-whisper, and later Qwen3-ASR).

### mema `POST /v1/unload` (`~/c/mema/server`)

- Requires the same auth as the other endpoints.
- For each built engine in `_ENGINES` that has an `unload()` method, call it
  off the event loop. Response: `{"unloaded": ["<engine/model>", ...]}`.
- `ParakeetOnnxEngine.unload()`: under `CUDA_INFERENCE_LOCK`, set
  `self._model = None` and run `gc.collect()`. It reports whether a model was
  loaded. Other engines get no `unload()` yet and are skipped.
- The process's CUDA context (a few hundred MB) stays allocated. llama's
  `fit = on` already keeps 1 GiB free.

### Errors

- llama unload request fails, or models are still loaded after 20 s: release
  the GPU and return 503 `{"detail":"GPU not available: could not free the GPU
  from llama-server. Try again in a moment."}` without forwarding.
- llama `/models` unreachable (connection refused or timeout): log and forward
  anyway, since a llama that is down holds no GPU memory.
- mema unload fails: log it and release the GPU anyway.

### Config

- New gate flag `-llama-key-env` (default `LLAMA_API_KEY`) names the env var
  that holds llama's key. If it is empty, the gate logs a warning at startup
  and sends unload requests without auth.
- `after-reboot.linux.sh` starts the gate with `set -a; . "$LLAMA_ENV"; set
  +a; exec "$GATE_BIN"`, the same way it starts llama. The header comments
  describe the swap.

## Testing

- Go (`main_test.go`, `httptest` fakes for both upstreams):
  - with llama idle and a model loaded, a transcription causes unload and then
    the transcribe call, in that order;
  - with llama in flight, a transcription still gets 503 and nothing is unloaded;
  - if llama unload fails or times out, the response is 503 and mema is not called;
  - when transcriptions overlap, mema unload runs once, after the last one;
  - a chat request during the mema unload gets 503, and succeeds after it;
  - llama `/models` unreachable → the request is forwarded.
- mema (pytest): `/v1/unload` requires auth, frees a loaded parakeet engine
  (`_model is None` afterwards), and is a no-op when nothing is loaded.
- Manual check on the PC: with llama loaded, dictate from Obsidian. `nvidia-smi`
  should show the llama child gone during the transcription and mema's memory
  back near its CUDA-context baseline after it. The text should land in the
  note, and the next chat should reload llama.

## Out of scope

- Path-prefix routing on a single port (clients keep their ports).
- Queueing or waiting instead of the 503.
- Reloading llama in the background after a dictation.

## Decisions

1. ~~After a transcription, mema frees its model.~~ Revised 2026-10-24: each
   model stays loaded until the other server needs the GPU.
   Rationale: user choice ("unload the model that is not needed for the
   current request … avoid the initial loading cost"); a switch is the only
   time anything loads.
2. Never force-unload llama while it is answering a request.
   Rationale: user requirement; a chat in progress must not be cut off.
3. A transcription that arrives while llama is answering gets the "GPU busy"
   503 immediately; it does not wait.
   Rationale: user choice; keeps the gate's current refusal behaviour.
4. A chat request that arrives while mema is transcribing gets the 503
   immediately; it does not wait.
   Rationale: user choice; same rule in the other direction.
5. The gate does the swapping in both directions: it unloads llama via the
   router API and calls a new mema `/v1/unload`.
   Rationale: user picked "Gate does both"; all swap logic lives in one place.
6. Keep two ports (8080 llama, 8787 mema), no `/llama` `/mema` path prefixes.
   Rationale: the gate already handles every request; prefixes would break
   the Whisker app (mDNS on 8787), the Obsidian plugin URL and pi's config.
