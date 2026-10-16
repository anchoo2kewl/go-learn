"""go-learn speech sidecar: text-to-speech with word timings, and speech-to-text, on Apple Silicon (MLX).

    python sidecar.py [--port 8791] [--voices voices/catalogue.json]

Listens on 127.0.0.1 only. One request at a time (MLX models aren't thread-safe, and the Mac does one
render at a time anyway). Models load on first use and are dropped after a quiet spell to give the
memory back.

  GET  /health
  GET  /voices                                     the stock catalogue
  POST /render     {voice_id, display[], speech[], out}   -> {duration_s, words[], align_score}
  POST /transcribe {path}                          -> {text}
  POST /preview    {voice_id, out}                 a short sample of the voice

`display` is the script as shown (paragraphs); `speech` is the same paragraphs as they should be said
(pronunciation fixes applied). Speech is rendered paragraph by paragraph with the voice's fixed reference
clip or preset, so it can't drift; the result is transcribed with word timestamps and aligned back to the
display words, which is what the read-along highlight uses.
"""

import argparse
import gc
import json
import logging
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import numpy as np
import soundfile as sf

HERE = Path(__file__).resolve().parent
STT_MODEL = "mlx-community/parakeet-tdt-0.6b-v3"
PAUSE_S = 0.35
IDLE_UNLOAD_S = 15 * 60
PREVIEW_TEXT = (
    "Hi, I'll be your teacher for this course. We'll start from what you already know, "
    "and go as deep as you want. Ask me anything, at any point."
)

log = logging.getLogger("sidecar")
lock = threading.Lock()
models = {}  # repo -> (model, last_used)
catalogue = {}


def model(repo, kind="tts"):
    hit = models.get(repo)
    if hit:
        models[repo] = (hit[0], time.time())
        return hit[0]
    log.info("loading %s", repo)
    if kind == "stt":
        from parakeet_mlx import from_pretrained

        m = from_pretrained(repo)
    else:
        from mlx_audio.tts.utils import load_model

        m = load_model(repo)
    models[repo] = (m, time.time())
    return m


def unloader():
    while True:
        time.sleep(60)
        with lock:
            for repo, (_, used) in list(models.items()):
                if time.time() - used > IDLE_UNLOAD_S:
                    log.info("unloading %s (idle)", repo)
                    del models[repo]
                    gc.collect()
                    try:
                        import mlx.core as mx

                        mx.clear_cache()
                    except Exception:
                        pass


def synth(voice, text):
    m = model(voice["model"])
    if voice["engine"] == "kokoro":
        kw = {"voice": voice["preset"], "lang_code": voice.get("lang_code", "a")}
    else:
        kw = {"ref_audio": str(HERE / "voices" / voice["ref_audio"]), "ref_text": voice["ref_text"]}
    chunks, sr = [], None
    for r in m.generate(text=text, **kw):
        chunks.append(np.asarray(r.audio, dtype=np.float32).reshape(-1))
        sr = r.sample_rate
    return np.concatenate(chunks), sr


def norm(w):
    return re.sub(r"[^a-z0-9]", "", w.lower())


def heard_words(result):
    out = []
    for t in (t for s in result.sentences for t in s.tokens):
        if out and not t.text.startswith(" "):
            w, s, _ = out[-1]
            out[-1] = (w + t.text, s, t.end)
        else:
            out.append((t.text.strip(), t.start, t.end))
    return [(w, s, e) for w, s, e in out if norm(w)]


def align(script_words, heard):
    """Edit-distance alignment; matched/substituted words take heard times, skipped ones are interpolated."""
    a, b = [norm(w) for w in script_words], [norm(w) for w, _, _ in heard]
    n, m = len(a), len(b)
    d = np.zeros((n + 1, m + 1), dtype=np.int32)
    d[:, 0] = np.arange(n + 1)
    d[0, :] = np.arange(m + 1)
    for i in range(1, n + 1):
        ai = a[i - 1]
        for j in range(1, m + 1):
            d[i, j] = min(d[i - 1, j] + 1, d[i, j - 1] + 1, d[i - 1, j - 1] + (ai != b[j - 1]))
    times, i, j, exact = [None] * n, n, m, 0
    while i > 0 and j > 0:
        if d[i, j] == d[i - 1, j - 1] + (a[i - 1] != b[j - 1]):
            times[i - 1] = heard[j - 1][1:]
            exact += a[i - 1] == b[j - 1]
            i, j = i - 1, j - 1
        elif d[i, j] == d[i - 1, j] + 1:
            i -= 1
        else:
            j -= 1
    known = [k for k, t in enumerate(times) if t]
    for k in range(n):
        if times[k]:
            continue
        lo = max((x for x in known if x < k), default=None)
        hi = min((x for x in known if x > k), default=None)
        s = times[lo][1] if lo is not None else 0.0
        e = times[hi][0] if hi is not None else s + 0.3
        span = (hi if hi is not None else n) - (lo if lo is not None else -1)
        step = (e - s) / max(1, span)
        off = k - (lo if lo is not None else -1)
        times[k] = (s + step * (off - 1), s + step * off)
    return times, exact / max(1, n)


def render(req):
    voice = catalogue[req["voice_id"]]
    display, speech = req["display"], req.get("speech") or req["display"]
    pieces, sr = [], None
    for para in speech:
        if not para.strip():
            continue
        audio, sr = synth(voice, para)
        pieces += [audio, np.zeros(int(PAUSE_S * sr), dtype=np.float32)]
    audio = np.concatenate(pieces)
    sf.write(req["out"], audio, sr)
    script = [(p, w) for p, para in enumerate(display) for w in para.split()]
    stt = model(STT_MODEL, "stt")
    result = stt.transcribe(req["out"], chunk_duration=120.0, overlap_duration=15.0)
    times, score = align([w for _, w in script], heard_words(result))
    words = [{"p": p, "w": w, "s": round(s, 3), "e": round(e, 3)} for (p, w), (s, e) in zip(script, times)]
    return {"duration_s": round(len(audio) / sr, 3), "words": words, "align_score": round(score, 4)}


def transcribe(req):
    stt = model(STT_MODEL, "stt")
    return {"text": stt.transcribe(req["path"]).text.strip()}


def preview(req):
    voice = catalogue[req["voice_id"]]
    audio, sr = synth(voice, PREVIEW_TEXT)
    sf.write(req["out"], audio, sr)
    return {"ok": True}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        log.debug(fmt, *args)

    def reply(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ok": True, "loaded": list(models)})
        if self.path == "/voices":
            return self.reply(200, [{k: v[k] for k in ("id", "engine", "label", "accent", "gender")} for v in catalogue.values()])
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        fn = {"/render": render, "/transcribe": transcribe, "/preview": preview}.get(self.path)
        if not fn:
            return self.reply(404, {"error": "not found"})
        try:
            req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            t0 = time.time()
            with lock:
                out = fn(req)
            log.info("%s done in %.1fs", self.path, time.time() - t0)
            self.reply(200, out)
        except KeyError as e:
            self.reply(400, {"error": f"unknown or missing {e}"})
        except Exception as e:  # report, don't die
            log.exception("%s failed", self.path)
            self.reply(500, {"error": str(e)[:500]})


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, default=8791)
    p.add_argument("--voices", default=str(HERE / "voices" / "catalogue.json"))
    a = p.parse_args()
    logging.basicConfig(level=logging.INFO, format='{"level":"%(levelname)s","component":"sidecar","msg":"%(message)s"}')
    for v in json.loads(Path(a.voices).read_text()):
        catalogue[v["id"]] = v
    threading.Thread(target=unloader, daemon=True).start()
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    log.info("listening on 127.0.0.1:%d with %d voices", a.port, len(catalogue))
    srv.serve_forever()


if __name__ == "__main__":
    main()
