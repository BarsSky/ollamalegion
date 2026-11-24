#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""reexport_job.py — исполнитель ОДНОЙ задачи ре-экспорта внутри образа gguf-reexport.

ЗАЧЕМ ОТДЕЛЬНЫЙ ДРАЙВЕР, А НЕ ВЫЗОВ СКРИПТА ПАТЧА НАПРЯМУЮ. Go-хост держит
очередь, состояние и HTTP; python делает ровно одну работу и говорит на языке
событий (одна строка — один JSON-объект). Драйвер — единственное место, где
меняется представление «задача → конвертер»:

    {"event":"progress","stage":"reading|writing|validating","phase":"plan|copy|verify",
     "percent":42.0,"bytes_done":..,"bytes_total":..}
    {"event":"log","message":"..."}
    {"event":"result","duration_ms":..,"src_bytes":..,"dst_bytes":..,"facts":{...}}
    {"event":"error","code":"model_not_patchable","message":"..."}

ПОЧЕМУ ДВЕ КЛИЧКИ СТАДИИ (stage И phase). `stage` — стадия конвейера из
контракта (reading → writing → validating): по ней Go и тесты понимают, где
задача, и она не меняется вместе с переводами интерфейса. `phase` — короткая
метка, которую панель уже показывает словами (план/копирование/сверка); она
оставлена, чтобы WebUI не пришлось переучивать ради переименования.

ПОЧЕМУ СОБЫТИЯ, А НЕ ПАРСИНГ ЧЕЛОВЕЧЕСКОГО ТЕКСТА. Скрипт патча печатает
прогресс через `\\r` («копирование данных: 42.0%») — это для человека. Разбирать
такое в Go значит зависеть от формулировки; поэтому драйвер перехватывает функцию
прогресса модуля патча (`_write_progress` — она задумана как шов) и печатает
машиночитаемое событие. Формулировки в человеческом выводе могут меняться, формат
событий — контракт.

ЧТО ЗДЕСЬ ЕСТЬ И ЧЕГО НЕТ:
  * `--target inkling2_loop` — переупаковка метаданных loop-экспорта Inkling-2
    (нужны только python + стандартная библиотека; torch НЕ нужен, веса не
    пересчитываются);
  * `--target hf_to_gguf` — полный ре-экспорт из HF-чекпойнта через
    convert_hf_to_gguf.py из c/llama.cpp. Требует torch + transformers, поэтому
    доступность объявляется пробой (`--info`), а не обещанием;
  * `--info` — что установлено в этом образе (python/torch/конвертер/скрипт патча).

КОДЫ ВОЗВРАТА (их читает runner.go, менять в одиночку нельзя):
  0 — успех; 2 — вход/каталог недоступен; 3 — файл не подходит под патч;
  4 — мало места; 5 — результат существует без --force.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import traceback

# Пути, где может лежать модуль патча loop-метаданных. В образе — /app/py (рядом
# с драйвером); при локальном запуске из репозитория — scripts/ в корне.
_HERE = os.path.dirname(os.path.abspath(__file__))
PATCH_MODULE_CANDIDATES = [
    os.path.join(_HERE, "gguf_loop_meta_patch.py"),                      # в образе
    os.path.join(_HERE, "..", "..", "..", "scripts", "gguf_loop_meta_patch.py"),  # из репозитория
]

EXIT_BAD_INPUT = 2
EXIT_NOT_PATCHABLE = 3
EXIT_NO_SPACE = 4
EXIT_DST_EXISTS = 5


def emit(event: str, **fields) -> None:
    """Одно событие — одна строка stdout. flush обязателен: Go читает поток."""
    payload = {"event": event}
    payload.update(fields)
    sys.stdout.write(json.dumps(payload, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def emit_log(message: str) -> None:
    emit("log", message=message)


def emit_error(code: str, message: str, detail: str = "") -> None:
    emit("error", code=code, message=message, detail=detail)


def load_patch_module():
    """Импортирует модуль патча по первому существующему пути из кандидатов."""
    for cand in PATCH_MODULE_CANDIDATES:
        cand = os.path.abspath(cand)
        if not os.path.isfile(cand):
            continue
        spec = importlib.util.spec_from_file_location("gguf_loop_meta_patch", cand)
        if spec is None or spec.loader is None:
            continue
        mod = importlib.util.module_from_spec(spec)
        sys.modules["gguf_loop_meta_patch"] = mod
        spec.loader.exec_module(mod)
        return mod, cand
    return None, ""


def info() -> int:
    """Что установлено в образе. Вызывается хостом до первой задачи."""
    patch_mod, patch_path = load_patch_module()
    data = {
        "ok": True,
        "python": "%d.%d.%d" % sys.version_info[:3],
        "torch": "",
        "converter": "",
        "converter_present": False,
        "patch_script": patch_path,
    }
    try:  # torch — необязательная зависимость: её отсутствие НЕ ошибка образа
        import torch  # noqa: F401

        data["torch"] = str(getattr(torch, "__version__", "present"))
    except Exception:
        data["torch"] = ""
    conv = os.environ.get("REEXPORT_LLAMA_CONVERTER", os.path.join(_HERE, "..", "llama", "convert_hf_to_gguf.py"))
    conv = os.path.abspath(conv)
    data["converter"] = conv
    data["converter_present"] = os.path.isfile(conv)
    if patch_mod is None:
        data["error"] = "модуль патча gguf_loop_meta_patch.py не найден (искали: %s)" % ", ".join(
            os.path.abspath(p) for p in PATCH_MODULE_CANDIDATES)
    print(json.dumps(data, ensure_ascii=False))
    return 0


def _progress_hook(impl):
    """Подмена функции прогресса модуля патча на событие для Go.

    Модуль печатает прогресс сам (`_write_progress`), и функция вызывается по
    имени из `write_patched`, поэтому подмена атрибута модуля — штатный шов, а не
    хак: она не меняет ни одного байта логики патча и не дублирует её.

    Проценты — по скопированным байтам (их печатает сам патч): это единственная
    метрика, которая не врёт на 16-гигабайтном файле. 100 % здесь не печатаем:
    после копирования идёт сверка результата (stage=validating), и «100 %» до неё
    было бы обещанием, которого ещё нет.
    """

    def hook(done: int, total: int, last=[0.0]) -> None:
        percent = (100.0 * done / total) if total else 99.0
        emit("progress", stage="writing", phase="copy", percent=round(min(percent, 99.0), 2),
             bytes_done=int(done), bytes_total=int(total))

    impl._write_progress = hook  # type: ignore[attr-defined]


def _dir_bytes(path: str) -> int:
    """Суммарный размер файлов каталога (для оценки «выход ≈ вход»)."""
    total = 0
    for root, _dirs, files in os.walk(path):
        for name in files:
            try:
                p = os.path.join(root, name)
                if os.path.isfile(p):
                    total += os.path.getsize(p)
            except OSError:
                continue
    return total


class _ConvertProgress(threading.Thread):
    """Проценты записи для hf_to_gguf: конвертер пишет один файл.

    ЧЕСТНО О ГРАНИЦАХ. convert_hf_to_gguf.py собирает тензоры в памяти и пишет
    файл в конце, поэтому «процентов чтения» по байтам у него нет: до записи
    прогресс — стадия `reading` без процентов (её видно по стадии, а не по числу).
    Зато у записи процент есть: размер готового файла против оценки «выход ≈ вход»
    — той же оценки, по которой Go проверяет место. Опрос печатает не чаще
    одного события в секунду и не больше 99 %: 100 % печатает драйвер после
    проверки результата.
    """

    def __init__(self, dst: str, expected_bytes: int, stop: threading.Event, interval: float = 2.0):
        super().__init__(daemon=True)
        self._dst = dst
        self._expected = expected_bytes
        self._stop = stop
        self._interval = interval
        self._last = -1.0

    def run(self) -> None:
        while not self._stop.wait(self._interval):
            try:
                size = os.path.getsize(self._dst)
            except OSError:
                continue
            if self._expected <= 0 or size <= 0:
                continue
            percent = min(99.0, 100.0 * size / self._expected)
            if percent - self._last < 1.0 and percent < 99.0:
                continue
            self._last = percent
            emit("progress", stage="writing", phase="copy", percent=round(percent, 2),
                 bytes_done=int(size), bytes_total=int(self._expected))


def target_inkling2_loop(args) -> int:
    """Патч метаданных loop-экспорта Inkling-2 под схему inkling2_loop."""
    impl, patch_path = load_patch_module()
    if impl is None:
        emit_error("driver_broken", "модуль патча gguf_loop_meta_patch.py не найден в образе")
        return EXIT_BAD_INPUT
    emit_log("скрипт патча: %s" % patch_path)
    if not os.path.isfile(args.src):
        emit_error("input_missing", "нет файла: %s" % args.src)
        return EXIT_BAD_INPUT

    started = time.time()
    emit("progress", stage="reading", phase="plan", percent=0.0)
    try:
        hdr = impl._read_src_header(args.src)
        plan = impl.make_plan(hdr, args.fuse_loop_queries)
    except impl.PatchError as e:
        # Это НЕ сбой сервиса: файл просто не того типа. Причину показываем дословно.
        emit_error("model_not_patchable", str(e))
        return EXIT_NOT_PATCHABLE

    facts = dict(plan.facts)
    facts["physical_blocks"] = plan.physical
    facts["fuse"] = bool(plan.fuse)
    facts["visits"] = plan.facts.get("visits")
    for line in plan.kv_changes:
        emit_log("изменение метаданных: " + line)
    emit_log("контроллеры: переименовано %d тензоров" % len(plan.ctrl_renames))
    if plan.fuse:
        emit_log("loop_msg_query: %d тензоров займут байты двух матриц запросов (blk.%d..blk.%d)"
                 % (len(plan.msg_query), plan.core_start, plan.core_end - 1))
    else:
        emit_log("loop_msg_query НЕ добавляется: загрузчик дойдёт до missing tensor "
                 "(включите fuse_loop_queries)")

    if args.dry_run:
        result = {
            "duration_ms": int((time.time() - started) * 1000),
            "src_bytes": hdr.file_size,
            "dst_bytes": 0,
            "facts": facts,
            "changes": list(plan.kv_changes),
            "verify": {"dry_run": True},
        }
        emit("result", **result)
        return 0

    if not args.dst:
        emit_error("bad_request", "нужен --dst для записи результата")
        return EXIT_BAD_INPUT
    need = hdr.file_size - hdr.data_start + 64 * 1024 * 1024
    free = shutil.disk_usage(os.path.dirname(os.path.abspath(args.dst)) or ".").free
    if free < need:
        emit_error("insufficient_space",
                   "мало места: нужно %d Б, свободно %d Б в %s" % (need, free,
                                                                 os.path.dirname(os.path.abspath(args.dst))))
        return EXIT_NO_SPACE
    if os.path.exists(args.dst) and not args.force:
        emit_error("dst_exists", "файл назначения уже есть: %s (нужен --force)" % args.dst)
        return EXIT_DST_EXISTS

    _progress_hook(impl)
    emit_log("пишу %s ..." % args.dst)
    try:
        impl.write_patched(args.src, args.dst, plan, hdr)
    except impl.PatchError as e:
        emit_error("patch_failed", str(e))
        return EXIT_BAD_INPUT

    verify = {}
    final_stage, final_phase = "writing", "copy"
    if args.verify:
        final_stage, final_phase = "validating", "verify"
        emit("progress", stage=final_stage, phase=final_phase, percent=99.0)
        try:
            verify = impl.verify_patched(args.dst, args.src, plan)
        except impl.PatchError as e:
            emit_error("verify_failed", "результат не сошёлся с ожидаемой схемой: %s" % e)
            return EXIT_BAD_INPUT
    # 100 % — только здесь: копирование упирается в 99 %, а «готово» имеет право
    # сказать тот, кто уже сверил результат (или осознанно выключил сверку).
    emit("progress", stage=final_stage, phase=final_phase, percent=100.0)

    result = {
        "duration_ms": int((time.time() - started) * 1000),
        "src_bytes": hdr.file_size,
        "dst_bytes": os.path.getsize(args.dst),
        "facts": facts,
        "changes": list(plan.kv_changes),
        "verify": verify or {},
    }
    emit("result", **result)
    return 0


def target_hf_to_gguf(args) -> int:
    """Полный ре-экспорт: HF-чекпойнт → GGUF через convert_hf_to_gguf.py.

    Реализовано как запуск конвертера llama.cpp: он и есть единственный
    авторитетный конвертер (своя копия «умеющая всё» разошлась бы с апстримом за
    неделю). Если torch/конвертера нет — доступность цели уже объявлена false в
    /capabilities, поэтому сюда запрос не придёт; на всякий случай причина
    возвращается и здесь, а не «тихий» ноль.
    """
    conv = args.converter or os.path.join(_HERE, "..", "llama", "convert_hf_to_gguf.py")
    conv = os.path.abspath(conv)
    try:
        import torch  # noqa: F401
    except Exception as e:
        emit_error("target_unavailable",
                   "образ собран без torch: полный ре-экспорт из HF-чекпойнта недоступен (%s)" % e)
        return EXIT_BAD_INPUT
    if not os.path.isfile(conv):
        emit_error("target_unavailable", "нет конвертера llama.cpp: %s" % conv)
        return EXIT_BAD_INPUT
    if not os.path.isdir(args.src):
        emit_error("input_missing", "для полного ре-экспорта нужен каталог HF-чекпойнта: %s" % args.src)
        return EXIT_BAD_INPUT

    started = time.time()
    outtype = args.outtype or "q8_0"
    argv = [sys.executable, conv, args.src, "--outfile", args.dst, "--outtype", outtype]
    emit_log("конвертер: %s (outtype=%s)" % (" ".join(argv[:1] + argv[1:2]), outtype))
    # Стадия `reading` идёт всё время, пока конвертер читает чекпойнт: по байтам её
    # не измерить (вход читается целиком в память), поэтому процент здесь не врём,
    # а показываем стадию. Проценты появятся, когда конвертер начнёт писать файл.
    emit("progress", stage="reading", phase="plan", percent=0.0)
    expected_out = _dir_bytes(args.src)
    stop = threading.Event()
    ticker = _ConvertProgress(args.dst, expected_out, stop)
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
    ticker.start()
    try:
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.rstrip("\r\n")
            if line.strip():
                emit_log(line)
    finally:
        code = proc.wait()
        stop.set()
        ticker.join(timeout=5)
    if code != 0:
        emit_error("converter_failed", "convert_hf_to_gguf.py завершился с кодом %d" % code)
        return EXIT_BAD_INPUT
    emit("progress", stage="validating", phase="verify", percent=99.0)
    dst_bytes = os.path.getsize(args.dst) if os.path.exists(args.dst) else 0
    emit("progress", stage="validating", phase="verify", percent=100.0)
    emit("result", duration_ms=int((time.time() - started) * 1000),
         src_bytes=expected_out, dst_bytes=dst_bytes,
         facts={"outtype": outtype}, changes=["создан GGUF из HF-чекпойнта"], verify={})
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Исполнитель одной задачи ре-экспорта GGUF.")
    ap.add_argument("--info", action="store_true", help="сведения об окружении образа (JSON)")
    ap.add_argument("--json-events", action="store_true", help="печатать события (контракт с Go)")
    ap.add_argument("--job-id", default="", help="идентификатор задачи (для логов)")
    ap.add_argument("--target", default="", help="цель: inkling2_loop | hf_to_gguf")
    ap.add_argument("--src", default="", help="источник: GGUF-файл или каталог HF-чекпойнта")
    ap.add_argument("--dst", default="", help="результат")
    ap.add_argument("--converter", default="", help="путь к convert_hf_to_gguf.py")
    ap.add_argument("--outtype", default="", help="тип выхода для hf_to_gguf (f16/q8_0/...)")
    ap.add_argument("--fuse-loop-queries", action="store_true",
                    help="разложить глобальные матрицы запросов по блокам ядра")
    ap.add_argument("--dry-run", action="store_true", help="только план, без записи")
    ap.add_argument("--force", action="store_true", help="перезаписать результат")
    ap.add_argument("--no-verify", action="store_true", help="не проверять результат после записи")
    args = ap.parse_args(argv)
    args.verify = not args.no_verify

    if args.info:
        return info()

    if not args.target:
        emit_error("bad_request", "не указан --target")
        return EXIT_BAD_INPUT

    try:
        if args.target == "inkling2_loop":
            return target_inkling2_loop(args)
        if args.target == "hf_to_gguf":
            return target_hf_to_gguf(args)
        emit_error("unknown_target", "неизвестная цель: %s" % args.target)
        return EXIT_BAD_INPUT
    except SystemExit:
        raise
    except Exception:
        # Трассировка нужна в хвосте лога задачи: без неё «Python упал» неотличим
        # от «файл не тот».
        detail = traceback.format_exc()
        emit_log(detail)
        emit_error("internal", "непредвиденная ошибка драйвера: %s" % detail.strip().splitlines()[-1])
        return 1


if __name__ == "__main__":
    sys.exit(main())
