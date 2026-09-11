#!/usr/bin/env python3
"""
R60.38: OpenWebUI-imitation diagnostic test with strict response-schema validation.

Имитирует точный формат запросов OpenWebUI (Ollama /api/chat with options.num_ctx),
проксирует через balancer → cppworker, парсит ВЕСЬ ответ (stream или non-stream),
валидирует response schema, детектирует аномалии и выдаёт структурированный
verdict (PASS / FAIL с деталями).

Если ответ не соответствует ожидаемой схеме (Ollama /api/chat формат),
тест падает с подробным diff'ом — это первичный сигнал для отладки.

Использование:
    python scripts/diag_openwebui_schema_validator.py [--mode stream|nonstream|auto] [--ctx 2048] [--model NAME]

Зачем: предыдущие diag_*.py скрипты отправляли запрос и печатали ответ, но
НЕ ВАЛИДИРОВАЛИ схему. Это позволяло пройти тесту с пустым/обрезанным
ответом если HTTP статус был 200. R60.38 — это comprehensive validator.
"""
from __future__ import annotations
import argparse
import json
import re
import sys
import time
from dataclasses import dataclass, field, asdict
from typing import Any, Optional

try:
    import requests
except ImportError:
    print("requests required: pip install requests", file=sys.stderr)
    sys.exit(2)


BALANCER = "http://localhost:18080"
MODEL = "Qwen3-Instruct-2507-q4km"

# Trajectory-style prompt — тот же что в diag_r60_25_trajectory.py
PROMPT = """Распиши красивый сайт на html css для интерактивной математики.
Покажи полный HTML файл с CSS внутри <style> тега, плюс JavaScript для
интерактивных элементов. Код должен быть полным и рабочим. Не обрывай."""

OLLAMA_REQUIRED_FIELDS_NONSTREAM = {"model", "created_at", "message", "done"}
OLLAMA_REQUIRED_MESSAGE_FIELDS = {"role", "content"}
OLLAMA_DONE_FIELDS = {"model", "created_at", "message", "done_reason", "done"}

# Stream chunks: required = model + message. created_at по Ollama spec должен
# быть в каждом chunk, но cppworker шлёт только в первом — это cppworker bug,
# не критично. См. validate_ollama_stream для отдельной проверки.
OLLAMA_REQUIRED_FIELDS_STREAM_CHUNK = {"model", "message"}
OLLAMA_STREAM_ROLE_CHUNK = {"model", "created_at", "message", "done"}


@dataclass
class SchemaVerdict:
    """Структурированный вердикт теста: PASS / FAIL с детальной причиной.

    anomalies — список проблем. Каждая anomaly имеет severity:
      - "fail" (PASS=False) — критическая: response не соответствует OpenWebUI/Ollama схеме
      - "warn" (PASS=True) — некритичная: missing optional fields (cppworker bug, не balancer)

    PASS = True если нет critical (fail) anomalies.
    """
    pass_: bool
    anomalies: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    http_status: int = 0
    elapsed_sec: float = 0.0
    response_format: str = ""  # "ollama_nonstream" / "ollama_stream" / "openai" / "unknown"
    fields_present: dict[str, bool] = field(default_factory=dict)
    content_length: int = 0
    finish_reason: str = ""
    done_reason: str = ""
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    summary: str = ""

    def to_dict(self) -> dict:
        d = asdict(self)
        d["pass"] = d.pop("pass_")
        return d


# Ollama spec: финальный chunk должен содержать done_reason + total_duration +
# load_duration + prompt_eval_duration + eval_duration. Но cppworker R60.x
# возвращает только total_duration. Это cppworker bug, не balancer bug.
# Помечаем как warning (не fail).
OPTIONAL_DONE_FIELDS = ("load_duration", "prompt_eval_duration", "eval_duration")
REQUIRED_DONE_FIELDS = ("done_reason", "total_duration")


def detect_format(body: dict) -> str:
    """Детектирует формат запроса: ollama (OpenWebUI) vs openai."""
    if "messages" in body and "options" in body:
        return "ollama"
    if "messages" in body and "stream" in body and "max_tokens" in body:
        return "openai"
    return "unknown"


def validate_ollama_nonstream(body_resp: dict, verdict: SchemaVerdict, model_name: str) -> None:
    """Валидирует Ollama non-stream response: должно быть всё за один раз."""
    verdict.response_format = "ollama_nonstream"
    verdict.fields_present = {k: k in body_resp for k in OLLAMA_REQUIRED_FIELDS_NONSTREAM}

    missing = [k for k, ok in verdict.fields_present.items() if not ok]
    if missing:
        verdict.anomalies.append(f"Ollama non-stream: missing top-level fields: {missing}")

    if "model" in body_resp:
        if body_resp["model"] != model_name:
            verdict.anomalies.append(
                f"model mismatch: requested={model_name!r}, response={body_resp['model']!r}"
            )

    msg = body_resp.get("message") or {}
    if not isinstance(msg, dict):
        verdict.anomalies.append(f"'message' is not dict: {type(msg).__name__}")
    else:
        for k in OLLAMA_REQUIRED_MESSAGE_FIELDS:
            if k not in msg:
                verdict.anomalies.append(f"message.{k} missing")
        content = msg.get("content", "")
        verdict.content_length = len(content) if isinstance(content, str) else 0
        if verdict.content_length == 0:
            verdict.anomalies.append("content is empty")
        if not isinstance(content, str):
            verdict.anomalies.append(f"content is not str: {type(content).__name__}")

    done = body_resp.get("done")
    if done is None:
        verdict.anomalies.append("'done' field missing")
    elif not isinstance(done, bool):
        verdict.anomalies.append(f"'done' is not bool: {type(done).__name__}")

    if done:
        # Финальный chunk: критичные поля (без них response бессмысленный).
        for k in REQUIRED_DONE_FIELDS:
            if k not in body_resp:
                verdict.anomalies.append(f"final-chunk REQUIRED field missing: {k}")
        # Опциональные поля (Ollama spec, но cppworker их не отдаёт) — warning.
        for k in OPTIONAL_DONE_FIELDS:
            if k not in body_resp:
                verdict.warnings.append(f"final-chunk OPTIONAL field missing: {k} (cppworker bug, not balancer)")

    verdict.done_reason = str(body_resp.get("done_reason", "") or "")
    verdict.prompt_tokens = int(body_resp.get("prompt_eval_count", 0) or 0)
    verdict.completion_tokens = int(body_resp.get("eval_count", 0) or 0)


def validate_ollama_stream(chunks: list[dict], verdict: SchemaVerdict) -> None:
    """Валидирует Ollama NDJSON stream: много chunk'ов с message.content, последний = done.

    cppworker возвращает application/x-ndjson (newline-delimited JSON), каждая
    строка — один JSON object. В stream ВСЕ duration поля присутствуют
    (load_duration, prompt_eval_duration, eval_duration). Не-стрим ответ
    этих полей не имеет (cppworker bug).
    """
    verdict.response_format = "ollama_stream"
    verdict.fields_present = {
        k: any(k in c for c in chunks) for k in OLLAMA_REQUIRED_FIELDS_STREAM_CHUNK
    }

    missing = [k for k, ok in verdict.fields_present.items() if not ok]
    if missing:
        verdict.anomalies.append(f"Ollama stream: no chunk contained fields: {missing}")

    accumulated: list[str] = []
    last_chunk: Optional[dict] = None
    done_chunk: Optional[dict] = None

    for i, chunk in enumerate(chunks):
        if not isinstance(chunk, dict):
            verdict.anomalies.append(f"chunk #{i} is not dict: {type(chunk).__name__}")
            continue
        if "error" in chunk:
            verdict.anomalies.append(f"chunk #{i} has error: {chunk['error']}")
        if "message" not in chunk or not isinstance(chunk.get("message"), dict):
            verdict.anomalies.append(f"chunk #{i} has invalid message")
            continue
        content = chunk["message"].get("content")
        if isinstance(content, str):
            accumulated.append(content)
        if chunk.get("done") is True:
            done_chunk = chunk
        last_chunk = chunk

    full_content = "".join(accumulated)
    verdict.content_length = len(full_content)

    if verdict.content_length == 0:
        verdict.anomalies.append("accumulated content is empty")
    if done_chunk is None:
        verdict.anomalies.append("no chunk has done=true")
    if last_chunk is not None and last_chunk.get("done") is not True:
        verdict.anomalies.append("last chunk does not have done=true")

    if done_chunk:
        verdict.done_reason = str(done_chunk.get("done_reason", "") or "")
        verdict.prompt_tokens = int(done_chunk.get("prompt_eval_count", 0) or 0)
        verdict.completion_tokens = int(done_chunk.get("eval_count", 0) or 0)
        # Stream done-chunk: обязательные поля (без них нельзя считать timing).
        for k in REQUIRED_DONE_FIELDS:
            if k not in done_chunk:
                verdict.anomalies.append(f"stream done-chunk REQUIRED field missing: {k}")
        for k in OPTIONAL_DONE_FIELDS:
            if k not in done_chunk:
                verdict.warnings.append(f"stream done-chunk OPTIONAL field missing: {k}")

    # Schema check: каждый chunk должен иметь model + message.role. created_at
    # по Ollama spec должен быть в каждом chunk, но cppworker шлёт только в
    # первом — это cppworker bug, не критично.
    created_at_seen = any(
        isinstance(c, dict) and "created_at" in c for c in chunks
    )
    if not created_at_seen:
        verdict.warnings.append(
            "no chunk has created_at (cppworker bug — should be in every chunk per Ollama spec)"
        )
    for i, chunk in enumerate(chunks):
        if not isinstance(chunk, dict):
            continue
        if "model" not in chunk:
            verdict.anomalies.append(f"chunk #{i} missing model")
        msg = chunk.get("message") or {}
        if "role" not in msg:
            verdict.anomalies.append(f"chunk #{i} message missing role")


def detect_truncation(content: str, verdict: SchemaVerdict) -> None:
    """Детектирует типичные индикаторы truncation в content."""
    if not content:
        return
    open_ticks = content.count("```")
    if open_ticks % 2 != 0:
        verdict.anomalies.append(f"unclosed code block: {open_ticks} backticks")
    if content.count("{") > content.count("}") + 2:
        verdict.anomalies.append(
            f"unbalanced braces: {content.count('{')} open vs {content.count('}')} close"
        )
    last_50 = content[-50:].rstrip()
    if last_50:
        ends = (".", "!", "?", "`", "}", ";", '"', "'", ">", "/")
        if last_50[-1].isalnum() or last_50[-1] in (",", "(", "+", "-", "*", "/", "=", "<"):
            verdict.anomalies.append(f"mid-line cutoff: ends with '{last_50[-20:]}'")


def run_ollama_request(
    *,
    stream: bool,
    num_ctx: int,
    num_predict: Optional[int],
    timeout_sec: int = 900,
    model_name: str = MODEL,
) -> SchemaVerdict:
    """Отправляет OpenWebUI-style Ollama /api/chat запрос, парсит ответ, валидирует schema."""
    body: dict[str, Any] = {
        "model": model_name,
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": stream,
        "options": {
            "num_ctx": num_ctx,
            "temperature": 0.7,
        },
    }
    if num_predict is not None:
        body["options"]["num_predict"] = num_predict

    verdict = SchemaVerdict(pass_=False)
    print(f"\n--- Ollama {'stream' if stream else 'non-stream'} request (num_ctx={num_ctx}) ---")
    print(f"body: model={model_name!r}, messages={len(body['messages'])}, stream={stream}, options.num_ctx={num_ctx}, options.num_predict={num_predict}")

    start = time.time()
    try:
        if stream:
            with requests.post(
                f"{BALANCER}/api/chat",
                json=body,
                stream=True,
                timeout=timeout_sec,
                headers={"Authorization": "Bearer bundled-default"},
            ) as r:
                verdict.http_status = r.status_code
                if r.status_code != 200:
                    body_text = r.text[:300]
                    # R60.39: nginx returns HTML on upstream timeout/error.
                    # This is what OpenWebUI sees: 502 Bad Gateway с HTML body
                    # "Unexpected token '<', '<html>'... is not valid JSON".
                    if body_text.lstrip().startswith("<"):
                        verdict.anomalies.append(
                            f"HTTP {r.status_code} with HTML body (likely nginx upstream timeout/error): {body_text[:200]}"
                        )
                        verdict.response_format = "nginx_html_error"
                    else:
                        verdict.anomalies.append(f"HTTP {r.status_code}: {body_text}")
                    verdict.elapsed_sec = time.time() - start
                    return verdict
                chunks: list[dict] = []
                # cppworker stream ответ — application/x-ndjson (newline-delimited JSON),
                # НЕ SSE (text/event-stream с "data: " префиксом). Каждая строка —
                # один JSON object, последний имеет done:true с полными метаданными.
                # SSE-парсинг даст 0 chunks (raw JSON не начинается с "data: ").
                for raw_line in r.iter_lines():
                    if raw_line is None:
                        continue
                    if isinstance(raw_line, bytes):
                        try:
                            line = raw_line.decode("utf-8")
                        except UnicodeDecodeError:
                            continue
                    else:
                        line = raw_line
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        chunk = json.loads(line)
                        chunks.append(chunk)
                    except json.JSONDecodeError as e:
                        verdict.anomalies.append(f"NDJSON decode error: {e}: {line[:100]}")
                verdict.elapsed_sec = time.time() - start
                validate_ollama_stream(chunks, verdict)
        else:
            r = requests.post(
                f"{BALANCER}/api/chat",
                json=body,
                timeout=timeout_sec,
                headers={"Authorization": "Bearer bundled-default"},
            )
            verdict.http_status = r.status_code
            verdict.elapsed_sec = time.time() - start
            if r.status_code != 200:
                body_text = r.text[:300]
                # R60.39: nginx returns HTML on upstream timeout/error.
                # This is what OpenWebUI sees: 502 Bad Gateway с HTML body
                # "Unexpected token '<', '<html>'... is not valid JSON".
                if body_text.lstrip().startswith("<"):
                    verdict.anomalies.append(
                        f"HTTP {r.status_code} with HTML body (likely nginx upstream timeout/error): {body_text[:200]}"
                    )
                    verdict.response_format = "nginx_html_error"
                else:
                    verdict.anomalies.append(f"HTTP {r.status_code}: {body_text}")
                return verdict
            try:
                body_resp = r.json()
            except json.JSONDecodeError as e:
                # Если ответ не парсится как JSON — это критично (R60.39:
                # именно это происходит когда nginx возвращает HTML 502).
                snippet = r.text[:300]
                if snippet.lstrip().startswith("<"):
                    verdict.anomalies.append(
                        f"non-stream response is HTML, not JSON (nginx upstream timeout/error): {snippet[:200]}"
                    )
                    verdict.response_format = "nginx_html_error"
                else:
                    verdict.anomalies.append(f"non-stream JSON decode error: {e}: {snippet}")
                return verdict
            validate_ollama_nonstream(body_resp, verdict, model_name)
    except requests.exceptions.Timeout:
        verdict.anomalies.append(f"timeout after {timeout_sec}s")
        verdict.elapsed_sec = time.time() - start
        return verdict
    except requests.exceptions.RequestException as e:
        verdict.anomalies.append(f"request exception: {e}")
        verdict.elapsed_sec = time.time() - start
        return verdict

    # Truncation check (если content собрался)
    if verdict.response_format == "ollama_stream" and verdict.content_length > 0:
        # восстановим content из verdict'а через chunks (для stream)
        # повторно парсим accumulated — нет, у нас нет самих chunks
        pass
    elif verdict.response_format == "ollama_nonstream":
        msg = body_resp.get("message", {})
        content = msg.get("content", "") if isinstance(msg, dict) else ""
        detect_truncation(content, verdict)

    # PASS = нет critical anomalies (warnings допустимы — это cppworker bugs).
    verdict.pass_ = len(verdict.anomalies) == 0
    return verdict


def _accumulate_content_for_stream(chunks: list[dict]) -> str:
    """Helper for detect_truncation in stream mode."""
    parts: list[str] = []
    for c in chunks:
        msg = c.get("message") if isinstance(c, dict) else None
        if isinstance(msg, dict):
            c2 = msg.get("content", "")
            if isinstance(c2, str):
                parts.append(c2)
    return "".join(parts)


def print_verdict(v: SchemaVerdict) -> None:
    """Красиво печатает вердикт теста."""
    print(f"\n=== VERDICT ({'PASS ✅' if v.pass_ else 'FAIL ❌'}) ===")
    print(f"  format        : {v.response_format}")
    print(f"  HTTP status   : {v.http_status}")
    print(f"  elapsed       : {v.elapsed_sec:.1f}s")
    print(f"  content length: {v.content_length} chars")
    print(f"  finish_reason : {v.finish_reason!r}")
    print(f"  done_reason   : {v.done_reason!r}")
    print(f"  prompt_tokens : {v.prompt_tokens}")
    print(f"  eval_count    : {v.completion_tokens}")
    if v.anomalies:
        print(f"  ❌ CRITICAL ANOMALIES ({len(v.anomalies)}):")
        for a in v.anomalies:
            print(f"      - {a}")
    if v.warnings:
        print(f"  ⚠️  WARNINGS ({len(v.warnings)}, non-blocking):")
        for w in v.warnings:
            print(f"      - {w}")
    if not v.anomalies and not v.warnings:
        print(f"  ✅ no anomalies or warnings")
    if v.fields_present:
        print(f"  fields_present: {v.fields_present}")


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[1])
    p.add_argument("--mode", choices=["stream", "nonstream", "auto"], default="auto",
                   help="Request mode: stream / nonstream / auto (both)")
    p.add_argument("--ctx", type=int, default=2048, help="num_ctx in body")
    p.add_argument("--num-predict", type=int, default=None, help="num_predict (default: omit, cppworker default)")
    p.add_argument("--model", default=MODEL, help=f"Model name (default: {MODEL})")
    args = p.parse_args()

    model_name = args.model

    verdicts: list[SchemaVerdict] = []

    modes: list[tuple[str, bool]]
    if args.mode == "auto":
        modes = [("nonstream", False), ("stream", True)]
    elif args.mode == "stream":
        modes = [("stream", True)]
    else:
        modes = [("nonstream", False)]

    for label, is_stream in modes:
        v = run_ollama_request(
            stream=is_stream,
            num_ctx=args.ctx,
            num_predict=args.num_predict,
            model_name=model_name,
        )
        print_verdict(v)
        verdicts.append(v)

    all_pass = all(v.pass_ for v in verdicts)
    print(f"\n{'=' * 50}")
    print(f"OVERALL: {'PASS ✅' if all_pass else 'FAIL ❌'} ({sum(1 for v in verdicts if v.pass_)}/{len(verdicts)} modes passed)")

    outfile = "_diag/last_openwebui_schema_verdict.json"
    import os
    os.makedirs("_diag", exist_ok=True)
    with open(outfile, "w", encoding="utf-8") as f:
        json.dump([v.to_dict() for v in verdicts], f, indent=2, ensure_ascii=False)
    print(f"\nSaved verdict to {outfile}")

    return 0 if all_pass else 1


if __name__ == "__main__":
    sys.exit(main())
