#!/usr/bin/env python3
"""
Universal model preparation script for cppworker (R60.39).

Что делает:
1. Проверяет состояние модели через cppworker /api/models.
2. Если модель не загружена или загружена с неподходящим n_ctx — перезагружает
   с правильным n_ctx (через POST /api/models/load).
3. Polling статуса загрузки через /api/models/load/progress.
4. Проверяет что модель загружена с expected_n_ctx (через /api/models).
5. Прогоняет smoke test — маленький запрос чтобы убедиться что inference работает.
6. Возвращает структурированный статус (success/failure с деталями).

Использование:
  python scripts/prepare_model.py \\
    --cppworker http://localhost:18092 \\
    --model Qwen3-Instruct-2507-q4km \\
    --num-ctx 8192 \\
    --num-predict 4096 \\
    --num-gpu-layers -2 \\
    --batch-size 512

Зачем: пользователь жалуется что OpenWebUI получает 502/HTML ошибки потому что
cppworker либо не загружен с правильным n_ctx, либо зависает на запросе
с n_ctx < n_predict + prompt. Этот скрипт делает "cold start deterministic":
после запуска модель ГАРАНТИРОВАННО в правильном состоянии и inference работает.
"""
from __future__ import annotations
import argparse
import json
import sys
import time
from dataclasses import dataclass, field, asdict
from typing import Any, Optional

import requests


@dataclass
class PrepareResult:
    """Структурированный результат prepare_model."""
    success: bool
    model: str = ""
    num_ctx: int = 0
    num_predict: int = 0
    num_gpu_layers: int = 0
    batch_size: int = 0
    steps: list[str] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    load_duration_sec: float = 0.0
    smoke_test_response_chars: int = 0
    final_state: dict[str, Any] = field(default_factory=dict)
    to_dict = asdict


def _check_cppworker(cppworker: str, timeout: float = 10.0) -> tuple[bool, str]:
    """Проверяет что cppworker отвечает (не завис)."""
    try:
        r = requests.get(f"{cppworker}/api/models", timeout=timeout)
        if r.status_code != 200:
            return False, f"cppworker /api/models returned {r.status_code}: {r.text[:200]}"
        return True, "ok"
    except requests.exceptions.Timeout:
        return False, f"cppworker /api/models timeout after {timeout}s"
    except requests.exceptions.RequestException as e:
        return False, f"cppworker /api/models exception: {e}"


def _wait_for_cppworker_idle(
    cppworker: str,
    max_wait_sec: float = 600.0,
    poll_interval_sec: float = 5.0,
) -> tuple[bool, str, float]:
    """Ждёт пока cppworker не будет в состоянии 'idle' (нет in-flight loads).

    cppworker может быть в середине load'а (от другого клиента). Если мы пошлём
    /api/models/load во время другого load'а — cppworker вернёт
    'model is already being loaded by another request; waiting' и может зависнуть
    на минуты. Лучше подождать пока чужой load завершится.
    """
    start = time.time()
    while time.time() - start < max_wait_sec:
        try:
            r = requests.get(f"{cppworker}/api/models/load/progress", timeout=5.0)
            if r.status_code == 200:
                data = r.json()
                # Progress endpoint returns list of loading models (or empty if none).
                # Если list пустой И models loaded >= 1 — idle.
                # Если list непустой — кто-то грузит.
                in_flight = data if isinstance(data, list) else []
                # Also check models list to know what's loaded.
                models_resp = requests.get(f"{cppworker}/api/models", timeout=5.0).json()
                loaded_count = models_resp.get("count", 0)
                if len(in_flight) == 0:
                    return True, f"idle (loaded={loaded_count})", time.time() - start
        except requests.exceptions.RequestException:
            pass
        time.sleep(poll_interval_sec)
    return False, f"timeout after {max_wait_sec}s", max_wait_sec


def _get_model_state(cppworker: str, model: str, timeout: float = 10.0) -> dict[str, Any] | None:
    """Получает текущее состояние модели из cppworker."""
    try:
        r = requests.get(f"{cppworker}/api/models", timeout=timeout)
        if r.status_code != 200:
            return None
        body = r.json()
        for m in body.get("models", []):
            if m.get("name") == model or model in m.get("name", ""):
                return m
        return None
    except (requests.exceptions.RequestException, json.JSONDecodeError):
        return None


def _load_model(
    cppworker: str,
    model: str,
    num_ctx: int,
    num_gpu_layers: int,
    batch_size: int,
    kv_cache_type: str = "q4_0",
    timeout: float = 30.0,
) -> tuple[bool, str]:
    """POST /api/models/load с правильными параметрами.

    cppworker load API использует camelCase JSON keys (cmd/cppworker/types.go
    loadModelRequest struct):
      - name (string, required)
      - contextSize (int, optional) — НЕ ctx_size, НЕ n_ctx, НЕ context_size!
      - gpuLayers (int, optional) — -2 = auto offload
      - batchSize (int, optional)
      - kvCacheType (string, optional) — "q4_0"/"q8_0"/"f16"
      - useMmap (bool, optional)
      - path (string, optional)
      - flashAttn (int, optional)
      - numa (bool, optional)
      - parallel (int, optional)
      - overrideTensors/overrideTensorBufts ([]string, optional, MoE)
      - reason (string, optional, observability)

    cppworker использует strict JSON decoder — лишние/неправильные поля
    возвращают 400 invalid JSON: unknown field "X".
    """
    body: dict[str, Any] = {
        "name": model,
        "contextSize": num_ctx,
        "gpuLayers": num_gpu_layers,
        "batchSize": batch_size,
        "kvCacheType": kv_cache_type,
        "useMmap": True,
    }
    try:
        r = requests.post(f"{cppworker}/api/models/load", json=body, timeout=timeout)
        if r.status_code not in (200, 202):
            return False, f"load returned {r.status_code}: {r.text[:300]}"
        try:
            data = r.json()
        except json.JSONDecodeError:
            return True, "load accepted (no JSON body)"
        return True, data.get("message", "load accepted")
    except requests.exceptions.Timeout:
        return False, f"load timeout after {timeout}s (model may still be loading in background)"
    except requests.exceptions.RequestException as e:
        return False, f"load exception: {e}"


def _poll_load_progress(
    cppworker: str,
    model: str,
    max_wait_sec: float = 300.0,
    poll_interval_sec: float = 2.0,
) -> tuple[bool, str, float]:
    """Polling /api/models/load/progress пока модель не станет 'loaded' или timeout."""
    start = time.time()
    while time.time() - start < max_wait_sec:
        try:
            r = requests.get(
                f"{cppworker}/api/models/load/progress",
                params={"model": model},
                timeout=10.0,
            )
            if r.status_code == 200:
                data = r.json()
                # /api/models/load/progress может вернуть либо dict с одним
                # progress, либо список progress entries. Нормализуем.
                if isinstance(data, list):
                    progress_list = data
                else:
                    progress_list = [data]
                # Ищем entry для нашей модели.
                for entry in progress_list:
                    m = entry.get("model", entry)
                    name = m.get("name", "")
                    if model not in name and name not in model:
                        continue
                    state = m.get("state", "unknown")
                    if state == "loaded":
                        return True, "loaded", time.time() - start
                    if state == "error":
                        err = m.get("loading_error", "unknown")
                        return False, f"load error: {err}", time.time() - start
                # Если наша модель НЕ в progress и models list shows loaded —
                # значит load завершился (progress не обновился).
                models_resp = requests.get(f"{cppworker}/api/models", timeout=5.0).json()
                for m in models_resp.get("models", []):
                    if m.get("name") == model and m.get("state") == "loaded":
                        return True, "loaded (via /api/models check)", time.time() - start
        except requests.exceptions.RequestException:
            pass
        time.sleep(poll_interval_sec)
    return False, f"timeout after {max_wait_sec}s", max_wait_sec


def _smoke_test(
    cppworker: str,
    model: str,
    num_ctx: int,
    num_predict: int,
    timeout: float = 120.0,
) -> tuple[bool, str, int]:
    """Прогоняет маленький запрос чтобы убедиться что inference работает."""
    body = {
        "model": model,
        "messages": [{"role": "user", "content": "Скажи 'ok' одним словом"}],
        "stream": False,
        "max_tokens": 16,
        "temperature": 0.0,
    }
    # /v1/chat/completions — OpenAI-совместимый (cppworker понимает)
    try:
        r = requests.post(
            f"{cppworker}/v1/chat/completions",
            json=body,
            timeout=timeout,
        )
        if r.status_code != 200:
            return False, f"HTTP {r.status_code}: {r.text[:200]}", 0
        try:
            data = r.json()
            content = data.get("choices", [{}])[0].get("message", {}).get("content", "")
            return True, "ok", len(content)
        except (json.JSONDecodeError, KeyError, IndexError) as e:
            return False, f"response parse error: {e}: {r.text[:200]}", 0
    except requests.exceptions.Timeout:
        return False, f"timeout after {timeout}s", 0
    except requests.exceptions.RequestException as e:
        return False, f"exception: {e}", 0


def prepare_model(
    *,
    cppworker: str,
    model: str,
    num_ctx: int,
    num_predict: int,
    num_gpu_layers: int = -2,
    batch_size: int = 512,
    kv_cache_type: str = "q4_0",
    force_reload: bool = False,
    smoke_test: bool = True,
    max_load_wait_sec: float = 300.0,
) -> PrepareResult:
    """Universal prepare_model — приводит модель в known-good state."""
    res = PrepareResult(
        success=False,
        model=model,
        num_ctx=num_ctx,
        num_predict=num_predict,
        num_gpu_layers=num_gpu_layers,
        batch_size=batch_size,
    )
    res.steps.append(f"check_cppworker ({cppworker})")
    ok, msg = _check_cppworker(cppworker)
    if not ok:
        res.errors.append(f"cppworker unreachable: {msg}")
        return res
    res.steps.append(f"✓ cppworker reachable")

    # Ждём пока cppworker не будет idle (нет in-flight loads от других клиентов).
    res.steps.append(f"wait_for_idle (max={max_load_wait_sec}s)")
    ok, msg, dur = _wait_for_cppworker_idle(cppworker, max_load_wait_sec)
    if not ok:
        res.errors.append(f"cppworker never became idle: {msg}")
        return res
    res.steps.append(f"  ✓ cppworker idle after {dur:.1f}s ({msg})")

    # Текущее состояние
    res.steps.append(f"check_current_state (model={model})")
    current = _get_model_state(cppworker, model)
    if current is None:
        res.steps.append(f"  model not currently loaded")
    else:
        cur_ctx = current.get("context_size", 0)
        cur_state = current.get("state", "")
        res.steps.append(
            f"  current state={cur_state}, n_ctx={cur_ctx}"
        )
        if cur_state == "loaded":
            res.final_state["loaded_n_ctx"] = cur_ctx

    # Нужно ли перезагружать
    needs_reload = force_reload
    if current is None:
        needs_reload = True
        res.steps.append(f"  → reload needed (model not loaded)")
    elif current.get("state") != "loaded":
        needs_reload = True
        res.steps.append(f"  → reload needed (state={current.get('state')})")
    elif current.get("context_size", 0) < num_ctx:
        # Текущий n_ctx меньше требуемого — нужно перезагрузить чтобы влезло.
        # Если current ctx > num_ctx — не downgrading (stickiness, R60.31).
        needs_reload = True
        res.steps.append(
            f"  → reload needed (loaded n_ctx={current.get('context_size')} < required={num_ctx})"
        )

    if needs_reload:
        res.steps.append(f"load (n_ctx={num_ctx}, gpu_layers={num_gpu_layers})")
        ok, msg = _load_model(
            cppworker, model, num_ctx, num_gpu_layers, batch_size, kv_cache_type
        )
        if not ok:
            res.errors.append(f"load failed: {msg}")
            return res
        res.steps.append(f"  ✓ load accepted: {msg}")

        res.steps.append(f"poll_load_progress (max_wait={max_load_wait_sec}s)")
        ok, msg, dur = _poll_load_progress(cppworker, model, max_load_wait_sec)
        res.load_duration_sec = dur
        if not ok:
            res.errors.append(f"load poll failed: {msg}")
            return res
        res.steps.append(f"  ✓ loaded in {dur:.1f}s")
    else:
        res.steps.append(f"  ✓ model already in good state, no reload needed")

    # Финальная проверка
    res.steps.append(f"verify_final_state")
    final = _get_model_state(cppworker, model)
    if final is None:
        res.errors.append(f"model disappeared after load")
        return res
    final_ctx = final.get("context_size", 0)
    res.final_state = dict(final)
    if final_ctx < num_ctx:
        res.warnings.append(
            f"loaded n_ctx={final_ctx} < requested={num_ctx} (downgrade by cppworker?"
            f" feasible_max={final.get('feasible_max_context', '?')})"
        )
    else:
        res.steps.append(f"  ✓ final n_ctx={final_ctx} ≥ requested={num_ctx}")

    # Smoke test
    if smoke_test:
        res.steps.append(f"smoke_test (max_tokens=16, /v1/chat/completions)")
        ok, msg, chars = _smoke_test(cppworker, model, num_ctx, num_predict, 60.0)
        res.smoke_test_response_chars = chars
        if not ok:
            res.errors.append(f"smoke test failed: {msg}")
            return res
        res.steps.append(f"  ✓ smoke test ok, response {chars} chars")

    res.success = True
    return res


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[1])
    p.add_argument("--cppworker", default="http://localhost:18092", help="cppworker URL")
    p.add_argument("--model", default="Qwen3-Instruct-2507-q4km", help="model name")
    p.add_argument("--num-ctx", type=int, default=8192, help="required num_ctx (must be ≥ num_predict + expected_prompt_tokens)")
    p.add_argument("--num-predict", type=int, default=4096, help="expected num_predict for tests")
    p.add_argument("--num-gpu-layers", type=int, default=-2, help="gpu_layers (-2 = auto offload, -1 = all, 0 = CPU only, N = first N layers)")
    p.add_argument("--batch-size", type=int, default=512)
    p.add_argument("--kv-cache-type", default="q4_0", help="kv cache quantization")
    p.add_argument("--force-reload", action="store_true", help="always reload, ignore current state")
    p.add_argument("--max-load-wait", type=float, default=300.0, help="max wait for load to complete (sec)")
    p.add_argument("--no-smoke-test", action="store_true", help="skip smoke test")
    args = p.parse_args()

    res = prepare_model(
        cppworker=args.cppworker,
        model=args.model,
        num_ctx=args.num_ctx,
        num_predict=args.num_predict,
        num_gpu_layers=args.num_gpu_layers,
        batch_size=args.batch_size,
        kv_cache_type=args.kv_cache_type,
        force_reload=args.force_reload,
        smoke_test=not args.no_smoke_test,
        max_load_wait_sec=args.max_load_wait,
    )

    print(f"\n{'=' * 60}")
    print(f"prepare_model: {'SUCCESS ✅' if res.success else 'FAIL ❌'}")
    print(f"{'=' * 60}")
    print(f"model           : {res.model}")
    print(f"num_ctx         : {res.num_ctx}")
    print(f"num_predict     : {res.num_predict}")
    print(f"num_gpu_layers  : {res.num_gpu_layers}")
    print(f"batch_size      : {res.batch_size}")
    print(f"load_duration   : {res.load_duration_sec:.1f}s")
    print(f"smoke_test_chars: {res.smoke_test_response_chars}")
    print(f"\nSteps:")
    for s in res.steps:
        print(f"  {s}")
    if res.warnings:
        print(f"\n⚠️  WARNINGS ({len(res.warnings)}):")
        for w in res.warnings:
            print(f"  - {w}")
    if res.errors:
        print(f"\n❌ ERRORS ({len(res.errors)}):")
        for e in res.errors:
            print(f"  - {e}")

    return 0 if res.success else 1


if __name__ == "__main__":
    sys.exit(main())
