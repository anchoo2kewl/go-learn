"""M0 voice bake-off: render one script with several engines and voices, time it, and check it.

Run with the sidecar's Python: ../../sidecar/.venv/bin/python bakeoff.py ...

    python bakeoff.py refs                 design the four reference voices (Qwen3-TTS VoiceDesign)
    python bakeoff.py render --set excerpt [--engine E] [--voice V]
    python bakeoff.py render --set full    [--engine E] [--voice V]
    python bakeoff.py check  --set full    transcribe renders with parakeet-mlx and score WER
    python bakeoff.py page                 build the blind listening page from the excerpt renders

Text is rendered paragraph by paragraph (as go-learn will in production), joined with a short pause,
loudness-normalised to -16 LUFS and encoded to mono AAC at 64 kbps.
"""

import argparse
import gc
import json
import random
import re
import subprocess
import sys
import time
from pathlib import Path

import numpy as np
import soundfile as sf

HERE = Path(__file__).resolve().parent
OUT = HERE / "out"
REFS = OUT / "refs"
RESULTS = OUT / "results.json"

# The excerpt: the congestion-window and slow-start paragraphs (dense with technical terms).
EXCERPT = slice(5, 8)
PAUSE_S = 0.35

VOICES = {
    "us_f": {
        "kokoro": "af_heart",
        "design": "A warm, clear American woman in her thirties, a calm and friendly teacher, "
        "natural pace, standard American accent, studio quality recording.",
    },
    "us_m": {
        "kokoro": "am_michael",
        "design": "A warm, clear American man in his thirties, a calm and engaging teacher, "
        "natural pace, standard American accent, studio quality recording.",
    },
    "uk_f": {
        "kokoro": "bf_emma",
        "design": "A clear, articulate British woman in her thirties with a southern English accent, "
        "a calm and friendly lecturer, natural pace, studio quality recording.",
    },
    "uk_m": {
        "kokoro": "bm_george",
        "design": "A clear, articulate British man in his forties with a southern English accent, "
        "a calm and engaging lecturer, natural pace, studio quality recording.",
    },
}

REF_TEXT = (
    "Hello, and welcome back. Today we're going to look at how computers share a network fairly, "
    "and why a few simple rules turn out to be surprisingly powerful."
)

# Chosen in M0 (blind): Qwen3-TTS American male and Kokoro American female/male. Breeze TTS 2 (7.6 GB) was
# tested and not picked.
# Best sound per gigabyte of disk, at full bf16 precision. Left out on size or rank: Fish Audio Drama 3
# (arena #1 but 23.8 GB at 4-bit, only ~22 Elo above Breeze 2), Fish S2 Pro (11 GB), Voxtral 4B (8 GB),
# VoxCPM2 (unranked, 1.2x realtime).
ENGINES = {
    "qwen3": "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-bf16",  # 4.5 GB, Apache-2.0
    "kokoro": "mlx-community/Kokoro-82M-bf16",  # 0.3 GB, baseline
}
ROUND1_VOICES = ["us_f", "us_m"]
DESIGN_MODEL = "mlx-community/Qwen3-TTS-12Hz-1.7B-VoiceDesign-8bit"
STT_MODEL = "mlx-community/parakeet-tdt-0.6b-v3"


def paragraphs():
    text = (HERE / "script.md").read_text()
    return [p.strip() for p in text.split("\n\n") if p.strip()]


def load(repo):
    from mlx_audio.tts.utils import load_model

    return load_model(repo)


def synth(model, engine, voice, text):
    """One paragraph -> (float32 mono audio, sample rate)."""
    kw = {}
    if engine == "kokoro":
        kw = {"voice": VOICES[voice]["kokoro"], "lang_code": "b" if voice.startswith("uk") else "a"}
    else:
        kw = {"ref_audio": str(REFS / f"{voice}.wav"), "ref_text": REF_TEXT}
    chunks, sr = [], None
    for r in model.generate(text=text, **kw):
        chunks.append(np.asarray(r.audio, dtype=np.float32).reshape(-1))
        sr = r.sample_rate
    return np.concatenate(chunks), sr


def encode(wav, m4a):
    subprocess.run(
        ["ffmpeg", "-y", "-loglevel", "error", "-i", str(wav), "-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
         "-ac", "1", "-c:a", "aac", "-b:a", "64k", str(m4a)],
        check=True,
    )


def load_results():
    return json.loads(RESULTS.read_text()) if RESULTS.exists() else {}


def save_results(res):
    RESULTS.write_text(json.dumps(res, indent=2, sort_keys=True))


def cmd_refs(_):
    REFS.mkdir(parents=True, exist_ok=True)
    model = load(DESIGN_MODEL)
    for v, spec in VOICES.items():
        audio, sr = [], None
        for r in model.generate(text=REF_TEXT, instruct=spec["design"]):
            audio.append(np.asarray(r.audio, dtype=np.float32).reshape(-1))
            sr = r.sample_rate
        sf.write(REFS / f"{v}.wav", np.concatenate(audio), sr)
        print(f"ref {v}: {len(np.concatenate(audio)) / sr:.1f}s", flush=True)


def cmd_render(a):
    paras = paragraphs()
    texts = paras[EXCERPT] if a.set == "excerpt" else paras
    engines = [a.engine] if a.engine else list(ENGINES)
    voices = [a.voice] if a.voice else (ROUND1_VOICES if a.set == "excerpt" else list(VOICES))
    res = load_results()
    for e in engines:
        t0 = time.time()
        try:
            model = load(ENGINES[e])
        except Exception as ex:  # a model that won't load is a result, not a crash
            print(f"{e}: load failed: {ex}", flush=True)
            res.setdefault(a.set, {})[f"{e}/load"] = {"error": str(ex)[:500]}
            save_results(res)
            continue
        load_s = time.time() - t0
        for v in voices:
            key = f"{e}/{v}"
            if key in res.get(a.set, {}) and "error" not in res[a.set][key] and not a.force:
                print(f"{key}: done already", flush=True)
                continue
            d = OUT / a.set
            d.mkdir(parents=True, exist_ok=True)
            pieces, sr, gen_s = [], None, 0.0
            try:
                for t in texts:
                    t1 = time.time()
                    audio, sr = synth(model, e, v, t)
                    gen_s += time.time() - t1
                    pieces += [audio, np.zeros(int(PAUSE_S * sr), dtype=np.float32)]
            except Exception as ex:
                print(f"{key}: failed: {ex}", flush=True)
                res.setdefault(a.set, {})[key] = {"error": str(ex)[:500]}
                save_results(res)
                continue
            audio = np.concatenate(pieces)
            wav, m4a = d / f"{e}_{v}.wav", d / f"{e}_{v}.m4a"
            sf.write(wav, audio, sr)
            encode(wav, m4a)
            dur = len(audio) / sr
            res.setdefault(a.set, {})[key] = {
                "audio_s": round(dur, 1),
                "gen_s": round(gen_s, 1),
                "x_realtime": round(dur / gen_s, 2),
                "load_s": round(load_s, 1),
                "sample_rate": sr,
                "m4a_bytes": m4a.stat().st_size,
            }
            save_results(res)
            print(f"{key}: {dur:.0f}s audio in {gen_s:.0f}s ({dur / gen_s:.1f}x realtime)", flush=True)
        del model
        gc.collect()


def words(s):
    s = s.lower().replace("c-wnd", "cwnd").replace("r-t-t", "rtt").replace("e-c-n", "ecn")
    return re.findall(r"[a-z0-9']+", s)


def wer(ref, hyp):
    r, h = words(ref), words(hyp)
    prev = list(range(len(h) + 1))
    for i in range(1, len(r) + 1):
        cur = [i] + [0] * len(h)
        for j in range(1, len(h) + 1):
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r[i - 1] != h[j - 1]))
        prev = cur
    return prev[-1] / max(1, len(r))


def norm(w):
    return re.sub(r"[^a-z0-9]", "", w.lower())


def heard_words(result):
    """parakeet tokens -> [(word, start, end)]; a leading space starts a new word."""
    out = []
    for t in (t for s in result.sentences for t in s.tokens):
        if out and not t.text.startswith(" "):
            w, s, _ = out[-1]
            out[-1] = (w + t.text, s, t.end)
        else:
            out.append((t.text.strip(), t.start, t.end))
    return [(w, s, e) for w, s, e in out if norm(w)]


def align(script_words, heard):
    """Edit-distance alignment of script words to heard words.

    Returns per-script-word (start, end) and the share of script words heard exactly. Matched and
    substituted words take the heard timing; words the voice skipped are interpolated between neighbours.
    """
    a, b = [norm(w) for w in script_words], [norm(w) for w, _, _ in heard]
    n, m = len(a), len(b)
    d = [[0] * (m + 1) for _ in range(n + 1)]
    for i in range(n + 1):
        d[i][0] = i
    for j in range(m + 1):
        d[0][j] = j
    for i in range(1, n + 1):
        for j in range(1, m + 1):
            d[i][j] = min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + (a[i - 1] != b[j - 1]))
    times, i, j, exact = [None] * n, n, m, 0
    while i > 0 and j > 0:
        if d[i][j] == d[i - 1][j - 1] + (a[i - 1] != b[j - 1]):
            times[i - 1] = heard[j - 1][1:]
            exact += a[i - 1] == b[j - 1]
            i, j = i - 1, j - 1
        elif d[i][j] == d[i - 1][j] + 1:
            i -= 1
        else:
            j -= 1
    # Interpolate gaps between known neighbours.
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


def cmd_check(a):
    """Transcribe each render: WER against the script, and word timings for read-along."""
    from parakeet_mlx import from_pretrained

    stt = from_pretrained(STT_MODEL)
    paras = paragraphs()
    paras = paras[EXCERPT] if a.set == "excerpt" else paras
    script = [(p, w) for p, para in enumerate(paras) for w in para.split()]
    ref = " ".join(paras)
    res = load_results()
    for key, r in sorted(res.get(a.set, {}).items()):
        if "error" in r or ("align_score" in r and not a.force):
            continue
        base = OUT / a.set / key.replace("/", "_")
        result = stt.transcribe(str(base) + ".wav", chunk_duration=120.0, overlap_duration=15.0)
        hyp = result.text
        r["wer"] = round(wer(ref, hyp), 4)
        hw, rw = words(hyp), words(ref)
        n = len(rw) // 3
        # By thirds, so drift late in a long render shows up separately.
        r["wer_thirds"] = [round(wer(" ".join(rw[i * n:(i + 1) * n]), " ".join(hw[i * len(hw) // 3:(i + 1) * len(hw) // 3])), 4) for i in range(3)]
        times, score = align([w for _, w in script], heard_words(result))
        r["align_score"] = round(score, 4)
        Path(str(base) + ".words.json").write_text(json.dumps(
            [{"p": p, "w": w, "s": round(s, 3), "e": round(e, 3)} for (p, w), (s, e) in zip(script, times)]))
        Path(str(base) + ".txt").write_text(hyp)
        print(f"{key}: WER {r['wer']:.3f} thirds {r['wer_thirds']} align {score:.3f}", flush=True)
        save_results(res)


def cmd_page(_):
    """Copy excerpt renders into page/ under shuffled letters; the key stays in out/ (not published)."""
    import shutil

    res = load_results().get("excerpt", {})
    clips = sorted(k for k, r in res.items() if "error" not in r and "align_score" in r)
    random.SystemRandom().shuffle(clips)
    page = HERE / "page"
    for f in page.glob("*.m4a"):
        f.unlink()
    for f in page.glob("*.words.json"):
        f.unlink()
    key, listing = {}, []
    for label, k in zip("ABCDEFGHIJKLMNOPQRSTUVWXYZ", clips):
        src = OUT / "excerpt" / k.replace("/", "_")
        shutil.copy(str(src) + ".m4a", page / f"{label}.m4a")
        shutil.copy(str(src) + ".words.json", page / f"{label}.words.json")
        key[label] = k
        listing.append({"label": label, "audio": f"{label}.m4a", "words": f"{label}.words.json"})
    (page / "clips.json").write_text(json.dumps(listing))
    (OUT / "blind_key.json").write_text(json.dumps(key, indent=2))
    print(f"{len(listing)} clips; key in out/blind_key.json")


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("refs")
    for name in ("render", "check"):
        s = sub.add_parser(name)
        s.add_argument("--set", choices=["excerpt", "full"], required=True)
        s.add_argument("--force", action="store_true")
        if name == "render":
            s.add_argument("--engine", choices=list(ENGINES))
            s.add_argument("--voice", choices=list(VOICES))
    sub.add_parser("page")
    a = p.parse_args()
    {"refs": cmd_refs, "render": cmd_render, "check": cmd_check, "page": cmd_page}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
