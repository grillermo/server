"""Text-to-speech server for the PC's GPU: Qwen3-TTS 1.7B Base, cloning a voice
from a short reference recording.

Runs behind gpu-gate (public :8790 -> private 127.0.0.1:8791), which serializes
the GPU with llama.cpp and mema. The model loads on the first /v1/audio/speech
and stays resident until the gate asks for it back through POST /v1/unload, so
a boot costs no VRAM.

Voices are files in voices/: <name>.wav (10-30 s of one person speaking) and
<name>.txt (exactly what they say in it). They are personal recordings, so
they stay out of git.

    POST /v1/audio/speech  {"input": "...", "voice": "guillermo", "language": "Spanish", "seed": 7}
                           -> audio/wav (24 kHz mono)
    POST /v1/unload        frees the GPU; the next speech request loads again
    GET  /v1/health        {"ok": true, "loaded": false, "voices": [...]}

Every endpoint but health wants `Authorization: Bearer $TTS_AUTH_TOKEN`.
"""
import gc
import io
import os
import threading
from pathlib import Path

import soundfile as sf
import torch
from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.responses import Response
from pydantic import BaseModel

MODEL_ID = os.environ.get("TTS_MODEL", "Qwen/Qwen3-TTS-12Hz-1.7B-Base")
TOKEN = os.environ.get("TTS_AUTH_TOKEN", "")
VOICES = Path(__file__).resolve().parent / "voices"

app = FastAPI()
# One generation at a time: the model is not safe to share across threads, and
# the gate already keeps the other servers off the card while we run.
lock = threading.Lock()
model = None
prompts = {}  # voice name -> (mtime, voice clone prompt), rebuilt when the files change


def authorized(authorization: str = Header("")):
    if not TOKEN or authorization != f"Bearer {TOKEN}":
        raise HTTPException(401, "bad or missing token")


def load():
    global model
    if model is None:
        from qwen_tts import Qwen3TTSModel

        model = Qwen3TTSModel.from_pretrained(
            MODEL_ID, device_map="cuda:0", dtype=torch.bfloat16, attn_implementation="sdpa"
        )
    return model


def voice_prompt(name: str):
    wav, txt = VOICES / f"{name}.wav", VOICES / f"{name}.txt"
    if "/" in name or not wav.exists() or not txt.exists():
        raise HTTPException(404, f"no voice {name!r}; put {name}.wav and {name}.txt in voices/")
    mtime = max(wav.stat().st_mtime, txt.stat().st_mtime)
    cached = prompts.get(name)
    if cached is None or cached[0] != mtime:
        prompt = load().create_voice_clone_prompt(ref_audio=str(wav), ref_text=txt.read_text().strip())
        prompts[name] = cached = (mtime, prompt)
    return cached[1]


class Speech(BaseModel):
    input: str
    voice: str
    language: str = "Spanish"
    seed: int | None = None


@app.post("/v1/audio/speech", dependencies=[Depends(authorized)])
def speech(req: Speech):
    if not req.input.strip():
        raise HTTPException(400, "empty input")
    with lock:
        m = load()
        prompt = voice_prompt(req.voice)
        if req.seed is not None:
            torch.manual_seed(req.seed)
        wavs, sr = m.generate_voice_clone(text=req.input, language=req.language, voice_clone_prompt=prompt)
    buf = io.BytesIO()
    sf.write(buf, wavs[0], sr, format="WAV")
    return Response(buf.getvalue(), media_type="audio/wav")


@app.post("/v1/unload", dependencies=[Depends(authorized)])
def unload():
    global model
    with lock:
        was = model is not None
        model = None
        prompts.clear()
        gc.collect()
        torch.cuda.empty_cache()
    return {"unloaded": [MODEL_ID] if was else []}


@app.get("/v1/health")
def health():
    return {"ok": True, "loaded": model is not None, "voices": sorted(p.stem for p in VOICES.glob("*.wav"))}
