import os
import time
import re
import json
import requests
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

app = FastAPI()

# Gemma (llama.cpp) base URL, e.g. http://gemma-http:8080
GEMMA_BASE_URL = os.getenv("GEMMA_BASE_URL", "http://host.docker.internal:8001").rstrip("/")
MODEL_ID = os.getenv("MODEL_ID", "").strip()

# Networking / performance knobs
TIMEOUT_SECONDS = float(os.getenv("TIMEOUT_SECONDS", "60"))
MAX_SNIPPETS = int(os.getenv("MAX_SNIPPETS", "5"))
MAX_TOKENS = int(os.getenv("MAX_TOKENS", "200"))
TEMPERATURE = float(os.getenv("TEMPERATURE", "0"))

MODEL_CACHE = {"id": None}


def _system_prompt() -> str:
    return f"""You are an HTTP security analyst.
Return ONLY one-line JSON with this exact schema:
{{"verdict":"Normal|Anomalous|Uncertain","snippets":["..."]}}

Rules:
- verdict must be exactly one of: Normal, Anomalous, Uncertain
- snippets: 1..{MAX_SNIPPETS} items if verdict is Anomalous or Uncertain; otherwise [] for Normal
- Each snippet must be copied verbatim from the raw HTTP text (examples: request line, Host, User-Agent, query string, suspicious payload like ' OR 1=1 --).
- Prefer snippets that justify the verdict (e.g. "User-Agent: sqlmap/1.7", "username=' OR 1=1 --").
- Do NOT invent any text that is not present in the raw HTTP.
"""


def _get_model_id() -> str:
    # Prefer explicit MODEL_ID first
    if MODEL_ID:
        return MODEL_ID

    # Cache model id to avoid calling /v1/models on every request
    if MODEL_CACHE["id"]:
        return MODEL_CACHE["id"]

    url = f"{GEMMA_BASE_URL}/v1/models"
    r = requests.get(url, timeout=TIMEOUT_SECONDS)
    r.raise_for_status()
    data = r.json()

    # llama.cpp sometimes returns both "data" and "models"
    mid = None
    if isinstance(data, dict):
        if isinstance(data.get("data"), list) and data["data"]:
            mid = data["data"][0].get("id")
        if not mid and isinstance(data.get("models"), list) and data["models"]:
            mid = data["models"][0].get("name") or data["models"][0].get("model")

    if not mid:
        raise RuntimeError(f"Cannot find model id from {url}: {data}")

    MODEL_CACHE["id"] = mid
    return mid


def _extract_first_json_object(text: str):
    # Extract the first {...} JSON block from model output
    m = re.search(r"\{.*\}", text, flags=re.DOTALL)
    if not m:
        return None
    blob = m.group(0).strip()
    try:
        return json.loads(blob)
    except Exception:
        return None


def _normalize_result(obj, raw_text: str):
    # Ensure schema: {"verdict": "...", "snippets": [...]}
    verdict = obj.get("verdict", "")
    snippets = obj.get("snippets", [])

    if verdict not in ("Normal", "Anomalous", "Uncertain"):
        v = str(verdict).lower()
        if "anom" in v or "susp" in v or "mal" in v:
            verdict = "Anomalous"
        elif "unc" in v or "maybe" in v:
            verdict = "Uncertain"
        else:
            verdict = "Normal"

    if not isinstance(snippets, list):
        snippets = []

    # Keep snippets grounded: must appear in raw_text
    cleaned = []
    for s in snippets[:MAX_SNIPPETS]:
        if not isinstance(s, str):
            continue
        s2 = s.strip()
        if not s2:
            continue
        if s2 in raw_text:
            cleaned.append(s2)

    return {"verdict": verdict, "snippets": cleaned}


def _fallback_snippets(raw_text: str):
    lines = raw_text.splitlines()
    out = []

    def add(s: str):
        s = s.strip()
        if not s or s in out:
            return
        if s in raw_text:
            out.append(s)

    # Request line
    for ln in lines[:10]:
        if re.match(r"^(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD)\s+\S+\s+HTTP/\d", ln.strip()):
            add(ln.strip())
            break

    # Common headers
    for key in ("User-Agent:", "Host:", "Referer:", "X-Forwarded-For:", "Cookie:"):
        for ln in lines:
            if ln.startswith(key):
                add(ln.strip())
                break

    suspicious_patterns = [
        r"(?i)\bsqlmap\b",
        r"(?i)\bnikto\b",
        r"(?i)\bunion\s+select\b",
        r"(?i)\bor\s+1=1\b",
        r"(?i)'\s*or\s+1=1",
        r"(?i)\bbenchmark\s*\(",
        r"(?i)\bsleep\s*\(",
        r"(?i)\binformation_schema\b",
        r"(?i)\.\./",
        r"(?i)<script",
        r"(?i)\bselect\b.+\bfrom\b",
        r"(?i)\binto\s+outfile\b",
        r"(?i)\bload_file\s*\(",
    ]

    for ln in lines:
        lns = ln.strip()
        if not lns:
            continue
        for pat in suspicious_patterns:
            if re.search(pat, lns):
                add(lns)
                break

    return out[:MAX_SNIPPETS]


def _ensure_http_shape(raw: str) -> str:
    raw = raw or ""
    raw = raw.replace("\r\n", "\n")

    # Allow formats like "Raw HTTP:\n\n<request...>"
    if raw.lstrip().lower().startswith("raw http:"):
        raw = re.sub(r"(?i)^raw http:\s*", "", raw.lstrip(), count=1)

    first_line = raw.splitlines()[0].strip() if raw.strip() else ""
    if not re.match(r"^(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD)\s+\S+\s+HTTP/\d", first_line):
        raw = "POST / HTTP/1.1\n" + raw

    return raw


def analyze_raw_http(raw_http: str) -> dict:
    raw = _ensure_http_shape(raw_http)

    model_id = _get_model_id()

    payload = {
        "model": model_id,
        "messages": [
            {"role": "system", "content": _system_prompt()},
            {"role": "user", "content": raw},
        ],
        "temperature": TEMPERATURE,
        "max_tokens": MAX_TOKENS,
    }

    r = requests.post(
        f"{GEMMA_BASE_URL}/v1/chat/completions",
        json=payload,
        timeout=TIMEOUT_SECONDS,
        headers={"Content-Type": "application/json"},
    )
    r.raise_for_status()
    data = r.json()

    content = data["choices"][0]["message"]["content"]

    obj = None
    if isinstance(content, str):
        obj = _extract_first_json_object(content)
        if obj is None:
            label = content.strip().lower()
            if label in ("anomalous", "suspicious", "malicious"):
                obj = {"verdict": "Anomalous", "snippets": []}
            elif label in ("normal", "benign"):
                obj = {"verdict": "Normal", "snippets": []}
            else:
                obj = {"verdict": "Uncertain", "snippets": []}
    elif isinstance(content, dict):
        obj = content
    else:
        obj = {"verdict": "Uncertain", "snippets": []}

    result = _normalize_result(obj, raw)

    if result["verdict"] in ("Anomalous", "Uncertain") and not result["snippets"]:
        result["snippets"] = _fallback_snippets(raw)

    return result


def _extract_raw_from_chat_payload(payload: dict) -> str:
    messages = payload.get("messages") or []
    if not isinstance(messages, list) or not messages:
        return ""
    last = messages[-1] or {}
    content = last.get("content", "")
    if not isinstance(content, str):
        return ""
    return content


@app.get("/healthz")
def healthz():
    mid = MODEL_CACHE["id"] or (MODEL_ID if MODEL_ID else None)
    return {"ok": True, "gemma_base_url": GEMMA_BASE_URL, "model_id": mid}


@app.get("/v1/models")
def v1_models():
    try:
        mid = _get_model_id()
    except Exception as e:
        return JSONResponse({"error": f"model_id_error: {e}"}, status_code=502)
    return {"object": "list", "data": [{"id": mid, "object": "model"}]}


@app.post("/ingest")
async def ingest(req: Request):
    body = await req.body()
    text = body.decode("utf-8", "replace").strip()

    # If http.ingest accidentally posts JSON chat payload to /ingest, handle it gracefully
    raw = text
    if text.startswith("{"):
        try:
            payload = json.loads(text)
            if isinstance(payload, dict) and "messages" in payload:
                raw = _extract_raw_from_chat_payload(payload)
        except Exception:
            raw = text

    try:
        result = analyze_raw_http(raw)
        return JSONResponse(result, status_code=200)
    except Exception as e:
        return JSONResponse({"error": f"gemma_call_error: {e}"}, status_code=502)


@app.post("/v1/chat/completions")
async def chat_completions(req: Request):
    payload = await req.json()
    raw = _extract_raw_from_chat_payload(payload)

    try:
        result = analyze_raw_http(raw)
    except Exception as e:
        return JSONResponse({"error": f"gemma_call_error: {e}"}, status_code=502)

    model = payload.get("model") or (MODEL_CACHE["id"] or MODEL_ID or "model.gguf")
    now = int(time.time())

    # Return OpenAI-like response, with message.content as a JSON string
    return {
        "id": f"chatcmpl-{now}",
        "object": "chat.completion",
        "created": now,
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": json.dumps(result, ensure_ascii=False),
                },
                "finish_reason": "stop",
            }
        ],
    }
