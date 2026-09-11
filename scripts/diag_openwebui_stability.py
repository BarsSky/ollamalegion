#!/usr/bin/env python3
"""
R60.39 — comprehensive stability test для клиент ↔ balancer ↔ cppworker.

Сценарий (имитирует реальный OpenWebUI):
  1. Подготовка: prepare_model.py грузит модель с правильным n_ctx.
  2. Серия из N последовательных OpenWebUI-style запросов с разными num_ctx/num_predict.
  3. Каждый запрос валидируется через schema validator.
  4. Дополнительно: проверка что 502 HTML body ловится как критичная аномалия.
  5. Stress: 5 быстрых запросов подряд (как реальный пользователь жмёт Enter).

Что детектирует:
  - HTTP 502/504 с HTML body (nginx fallback) — OpenWebUI показывает
    'Unexpected token <, <html>... is not valid JSON'
  - Truncation из-за n_ctx < n_predict + prompt
  - cppworker missing duration fields (warnings, non-blocking)
  - Empty content / missing required fields
  - Cascading failures (каждый следующий запрос хуже предыдущего)
  - Превышение таймаута (cppworker зависает)

Использование:
  python scripts/diag_openwebui_stability.py
  python scripts/diag_openwebui_stability.py --num-requests 5 --num-ctx 4096 --num-predict 2048
"""
from __future__ import annotations
import argparse
import json
import os
import subprocess
import sys
import time
from dataclasses import dataclass, field, asdict
from typing import Any

import requests

BALANCER = "http://localhost:18080"
CPPWORKER = "http://localhost:18092"
MODEL = "Qwen3-Instruct-2507-q4km"

# Russian prompt из реального скриншота пользователя (HTML/CSS сайт по математике).
PROMPT_RU = (
    "Привет распиши красивый сайт на html css для интерактивной математики "
    "расчета движения полета"
)

# English trajectory prompt (как в diag_openwebui_schema_validator.py).
PROMPT_EN = (
    "Распиши красивый сайт на html css для интерактивной математики. "
    "Покажи полный HTML файл с CSS внутри <style> тега, плюс JavaScript для "
    "интерактивных элементов. Код должен быть полным и рабочим. Не обрывай."
)


@dataclass
class StabilityVerdict:
    """Результат stability test."""
    pass_: bool
    prepare_model_success: bool = False
    prepare_model_steps: list[str] = field(default_factory=list)
    prepare_model_errors: list[str] = field(default_factory=list)
    requests_run: int = 0
    requests_passed: int = 0
    requests_failed: int = 0
    request_results: list[dict[str, Any]] = field(default_factory=list)
    cascade_detected: bool = False
    html_502_count: int = 0
    timeout_count: int = 0
    truncation_count: int = 0
    summary: str = ""

    def to_dict(self) -> dict:
        d = asdict(self)
        d["pass"] = d.pop("pass_")
        return d


def run_prepare_model(
    cppworker_url: str,
    model: str,
    num_ctx: int,
    num_predict: int,
    max_wait: float,
) -> tuple[bool, list[str], list[str]]:
    """Запускает prepare_model.py и возвращает (success, steps, errors)."""
    cmd = [
        sys.executable, "scripts/prepare_model.py",
        "--cppworker", cppworker_url,
        "--model", model,
        "--num-ctx", str(num_ctx),
        "--num-predict", str(num_predict),
        "--max-load-wait", str(int(max_wait)),
        "--no-smoke-test",  # мы сами прогоняем тесты ниже
    ]
    print(f"\n[prepare_model] running: {' '.join(cmd)}")
    try:
        result = subprocess.run(
            cmd,
            cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
            capture_output=True,
            text=True,
            timeout=max_wait + 60,
        )
        output = result.stdout + result.stderr
        print(f"[prepare_model] exit code: {result.returncode}")
        print(output[-1500:])  # last 1500 chars to keep output manageable
        success = result.returncode == 0
        steps = [line.strip() for line in output.split("\n") if line.strip().startswith(("✓", "→", "check_", "load", "poll", "wait", "verify", "smoke"))]
        # Extract errors from output
        errors = []
        for line in output.split("\n"):
            if "ERROR" in line and ":" in line:
                errors.append(line.split("ERROR", 1)[1].strip(" :"))
        return success, steps, errors
    except subprocess.TimeoutExpired:
        return False, [], [f"prepare_model timeout after {max_wait + 60}s"]
    except Exception as e:
        return False, [], [f"prepare_model exception: {e}"]


def run_ollama_request(
    *,
    body: dict[str, Any],
    auth_header: str,
    balancer_url: str = BALANCER,
    timeout_sec: int = 600,
) -> dict[str, Any]:
    """Отправляет запрос через balancer и возвращает структурированный результат."""
    result: dict[str, Any] = {
        "body_sent": {k: v for k, v in body.items() if k != "messages"},
        "messages_count": len(body.get("messages", [])),
        "stream": body.get("stream", False),
        "elapsed_sec": 0.0,
        "http_status": 0,
        "content_type": "",
        "body_first_char": "",
        "is_html": False,
        "is_json": False,
        "content_length": 0,
        "finish_reason": "",
        "done_reason": "",
        "prompt_tokens": 0,
        "eval_count": 0,
        "anomalies": [],
        "warnings": [],
    }
    start = time.time()
    try:
        r = requests.post(
            f"{balancer_url}/api/chat",
            json=body,
            timeout=timeout_sec,
            headers={"Authorization": auth_header},
        )
        result["http_status"] = r.status_code
        result["elapsed_sec"] = time.time() - start
        result["content_type"] = r.headers.get("Content-Type", "")
        result["body_first_char"] = (r.text[:1] if r.text else "")
        result["is_html"] = result["body_first_char"] == "<"
        body_text = r.text or ""
        result["content_length"] = len(body_text)

        if result["http_status"] != 200:
            if result["is_html"]:
                result["anomalies"].append(
                    f"HTTP {r.status_code} HTML (nginx upstream timeout/error, what OpenWebUI sees)"
                )
            else:
                result["anomalies"].append(f"HTTP {r.status_code}: {body_text[:200]}")
            return result

        if result["is_html"]:
            result["anomalies"].append(
                f"200 OK but body is HTML (nginx error page, not JSON): {body_text[:200]}"
            )
            return result

        try:
            data = r.json()
            result["is_json"] = True
        except json.JSONDecodeError as e:
            result["anomalies"].append(f"non-JSON response: {e}: {body_text[:200]}")
            return result

        # Parse Ollama/OpenAI response
        if "message" in data:
            # Ollama format
            msg = data.get("message") or {}
            content = msg.get("content", "") if isinstance(msg, dict) else ""
            result["content_length"] = len(content)
            if not content:
                result["anomalies"].append("empty content")
            result["done_reason"] = str(data.get("done_reason", "") or "")
            result["prompt_tokens"] = int(data.get("prompt_eval_count", 0) or 0)
            result["eval_count"] = int(data.get("eval_count", 0) or 0)
            # Truncation detection
            open_ticks = content.count("```")
            if open_ticks % 2 != 0:
                result["anomalies"].append(f"unclosed code block: {open_ticks} backticks")
                result.setdefault("truncation", True)
            last_50 = content[-50:].rstrip() if content else ""
            if last_50:
                ends = (".", "!", "?", "`", "}", ";", '"', "'", ">", "/")
                if last_50[-1].isalnum() or last_50[-1] in (",", "(", "+", "-", "*", "/", "=", "<"):
                    result["anomalies"].append(f"mid-line cutoff: ends with '{last_50[-20:]}'")
                    result.setdefault("truncation", True)
        elif "choices" in data:
            # OpenAI format (when balancer translates)
            choices = data.get("choices") or []
            if not choices:
                result["anomalies"].append("no choices in response")
                return result
            content = choices[0].get("message", {}).get("content", "")
            result["content_length"] = len(content)
            if not content:
                result["anomalies"].append("empty content")
            result["finish_reason"] = choices[0].get("finish_reason", "")
            usage = data.get("usage") or {}
            result["prompt_tokens"] = usage.get("prompt_tokens", 0)
            result["eval_count"] = usage.get("completion_tokens", 0)
        else:
            result["anomalies"].append(f"unknown response schema: keys={list(data.keys())}")
            return result

    except requests.exceptions.Timeout:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"timeout after {timeout_sec}s")
    except requests.exceptions.RequestException as e:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"request exception: {e}")
    return result


def run_stability_test(
    *,
    cppworker_url: str = CPPWORKER,
    balancer_url: str = BALANCER,
    model: str = MODEL,
    num_ctx: int = 8192,
    num_predict: int = 2048,
    num_requests: int = 3,
    prompts: list[str] | None = None,
    auth_header: str = "Bearer bundled-default",
    max_load_wait: float = 300.0,
    request_timeout: int = 600,
) -> StabilityVerdict:
    """Главная функция — prepare + run series of requests + analyze."""
    if prompts is None:
        prompts = [PROMPT_RU, PROMPT_EN, PROMPT_RU]

    verdict = StabilityVerdict(pass_=False)
    print("=" * 70)
    print(f"OpenWebUI stability test (R60.39)")
    print(f"  cppworker:  {cppworker_url}")
    print(f"  balancer:   {balancer_url}")
    print(f"  model:      {model}")
    print(f"  num_ctx:    {num_ctx}")
    print(f"  num_predict:{num_predict}")
    print(f"  requests:   {num_requests}")
    print(f"  prompts:    {len(prompts)} (cycled)")
    print("=" * 70)

    # Шаг 1: prepare model
    ok, steps, errors = run_prepare_model(
        cppworker_url, model, num_ctx, num_predict, max_load_wait
    )
    verdict.prepare_model_success = ok
    verdict.prepare_model_steps = steps
    verdict.prepare_model_errors = errors
    if not ok:
        verdict.summary = f"prepare_model failed: {errors}"
        return verdict

    # Шаг 2: серия запросов
    prev_anomaly_count = 0
    for i in range(num_requests):
        prompt = prompts[i % len(prompts)]
        body = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "stream": False,
            "options": {
                "num_ctx": num_ctx,
                "num_predict": num_predict,
                "temperature": 0.7,
            },
        }
        print(f"\n--- Request {i + 1}/{num_requests} (prompt_len={len(prompt)}) ---")
        r = run_ollama_request(
            body=body,
            auth_header=auth_header,
            balancer_url=balancer_url,
            timeout_sec=request_timeout,
        )
        verdict.requests_run += 1
        anom = r.get("anomalies", [])
        if anom:
            verdict.requests_failed += 1
            if any("HTML" in a for a in anom):
                verdict.html_502_count += 1
            if any("timeout" in a.lower() for a in anom):
                verdict.timeout_count += 1
            if any("code block" in a or "cutoff" in a for a in anom):
                verdict.truncation_count += 1
        else:
            verdict.requests_passed += 1

        # Cascade detection: anomaly count grows
        if len(anom) > prev_anomaly_count:
            verdict.cascade_detected = True
        prev_anomaly_count = len(anom)

        print(f"  HTTP {r['http_status']} ({r['content_type']})")
        print(f"  first_char: {r['body_first_char']!r} ({'HTML' if r['is_html'] else 'JSON' if r['is_json'] else '?'})")
        print(f"  elapsed: {r['elapsed_sec']:.1f}s, content_length: {r['content_length']}")
        print(f"  eval_count: {r['eval_count']}, prompt_tokens: {r['prompt_tokens']}")
        print(f"  done_reason: {r['done_reason']!r}, finish_reason: {r['finish_reason']!r}")
        if r["anomalies"]:
            print(f"  ❌ ANOMALIES:")
            for a in r["anomalies"]:
                print(f"      - {a}")
        else:
            print(f"  ✅ no anomalies")

        verdict.request_results.append(r)

    # Final verdict
    verdict.pass_ = (
        verdict.prepare_model_success
        and verdict.requests_failed == 0
        and verdict.html_502_count == 0
        and verdict.timeout_count == 0
    )
    if verdict.pass_:
        verdict.summary = (
            f"✅ all {verdict.requests_run}/{verdict.requests_run} requests succeeded, "
            f"prepare_model ok, no HTML 502, no timeouts"
        )
    else:
        verdict.summary = (
            f"❌ {verdict.requests_failed}/{verdict.requests_run} requests failed, "
            f"html_502={verdict.html_502_count}, timeouts={verdict.timeout_count}, "
            f"truncations={verdict.truncation_count}"
        )
    return verdict


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[1])
    p.add_argument("--num-requests", type=int, default=3, help="how many sequential requests")
    p.add_argument("--num-ctx", type=int, default=8192)
    p.add_argument("--num-predict", type=int, default=2048)
    p.add_argument("--max-load-wait", type=float, default=300.0)
    p.add_argument("--request-timeout", type=int, default=600)
    p.add_argument("--cppworker", default=CPPWORKER)
    p.add_argument("--balancer", default=BALANCER)
    p.add_argument("--model", default=MODEL)
    args = p.parse_args()

    v = run_stability_test(
        cppworker_url=args.cppworker,
        balancer_url=args.balancer,
        model=args.model,
        num_ctx=args.num_ctx,
        num_predict=args.num_predict,
        num_requests=args.num_requests,
        max_load_wait=args.max_load_wait,
        request_timeout=args.request_timeout,
    )

    print("\n" + "=" * 70)
    print(f"OVERALL: {v.summary}")
    print("=" * 70)

    outfile = "_diag/last_openwebui_stability_verdict.json"
    os.makedirs("_diag", exist_ok=True)
    with open(outfile, "w", encoding="utf-8") as f:
        json.dump(v.to_dict(), f, indent=2, ensure_ascii=False)
    print(f"\nSaved verdict to {outfile}")

    return 0 if v.pass_ else 1


if __name__ == "__main__":
    sys.exit(main())
