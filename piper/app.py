"""Tiny local Piper TTS HTTP sidecar for the HomeForge assistant.
POST /tts  (body = text)  -> audio/wav  (Piper neural voice, CPU, no cloud).
Voice is set by $PIPER_VOICE (default en_GB-alan-medium — British male, "Jarvis"-ish) and
downloaded once from the public rhasspy/piper-voices repo into a persistent volume.
Python (not Go) only because Piper's runtime is a Python/onnx package with no Go equivalent —
same "bridge" pattern as the vesync/kidde/hubspace sidecars.
"""
import os
import subprocess
import tempfile
import urllib.request

from flask import Flask, request, Response

VOICE = os.environ.get("PIPER_VOICE", "en_GB-alan-medium")
MODELS = "/models"
os.makedirs(MODELS, exist_ok=True)


def onnx_path(v):
    return os.path.join(MODELS, v + ".onnx")


def ensure_voice(v):
    """Download <voice>.onnx and .onnx.json from HuggingFace if not already present."""
    onnx = onnx_path(v)
    if os.path.exists(onnx) and os.path.exists(onnx + ".json"):
        return
    lang, speaker, quality = v.split("-")          # en_GB, alan, medium
    family = lang.split("_")[0]                     # en
    base = ("https://huggingface.co/rhasspy/piper-voices/resolve/main/"
            f"{family}/{lang}/{speaker}/{quality}/{v}")
    for suffix, dest in ((".onnx", onnx), (".onnx.json", onnx + ".json")):
        print("piper: downloading", base + suffix, flush=True)
        urllib.request.urlretrieve(base + suffix, dest)
    print("piper: voice ready:", v, flush=True)


ensure_voice(VOICE)
app = Flask(__name__)


@app.post("/tts")
def tts():
    text = request.get_data(as_text=True).strip()
    if not text:
        return ("empty", 400)
    if len(text) > 2000:
        text = text[:2000]
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
        path = tf.name
    try:
        r = subprocess.run(
            ["piper", "--model", onnx_path(VOICE), "--output_file", path],
            input=text.encode("utf-8"), capture_output=True)
        if r.returncode != 0:
            return (r.stderr.decode("utf-8", "ignore") or "piper failed", 500)
        with open(path, "rb") as fh:
            data = fh.read()
    finally:
        try:
            os.unlink(path)
        except OSError:
            pass
    return Response(data, mimetype="audio/wav")


@app.get("/health")
def health():
    return "ok"


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5050)
