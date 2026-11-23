#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gguf_loop_meta_patch.py — привести loop-экспорт Inkling-2 к схеме inkling2_loop.

ЗАЧЕМ. Файл Inkling-2-Mini-UD-Q4_K_M.gguf объявляет general.architecture="inkling2"
и block_count=48 (это ЧИСЛО ВИЗИТОВ слоя: 6 прелюдии + 12 ядра x 3 прохода +
6 постудии), а физических блоков в тензорах 24 (blk.0..blk.23). Загрузчик
c/llama.cpp (5bacb7c868) знает loop-схему как ОТДЕЛЬНУЮ архитектуру
"inkling2_loop" (src/models/inkling2-loop.cpp, models.h:2557-2650), у которой:

  * block_count — ФИЗИЧЕСКИЕ слои (24), а не визиты;
  * ключи loop.start / loop.end / loop.count / loop.gate_rank (+loop.varying);
  * контроллеры blk.{core}.loop_{q,k,v,ffn}_ctrl_{down,gain,forget};
  * blk.{core}.loop_msg_query — 4 строки (Q,K,V,FFN) запросов межпроходных
    сообщений; конвертер (c/llama.cpp/conversion/inkling2.py:315-329) складывает
    их из ДВУХ матриц чекпойнта: q = cross_pass_messages.queries,
    kv = cross_pass_messages.kv_queries:
        msg_query[i] = stack([q[2i], kv[2i], kv[2i+1], q[2i+1]])

В loop-экспорте (иначе упакованном) те же данные лежат как две глобальные
матрицы loop_res_query / loop_kv_res_query ([n_embd, 2*core]) и контроллеры
названы loop_{attn,k,v,mlp}_{down,gain,forget}. Поэтому загрузчик ищет blk.24,
не находит его и падает на check_tensor_dims.

ЧТО ДЕЛАЕТ СКРИПТ (ни один вес не пересчитывается и не сдвигается):

  1. general.architecture       inkling2 -> inkling2_loop
  2. все ключи inkling2.*       -> inkling2_loop.*
  3. block_count                48 (визиты) -> 24 (физические слои)
  4. массивы по визитам         head_count / head_count_kv / sliding_window_pattern
                                усекаются до 24 (первые 24 визита и есть
                                физические слои 0..23 — сверяется по раскладке
                                визитов, см. visits_for())
  5. имена контроллеров         loop_attn_* -> loop_q_ctrl_*, loop_k_* ->
                                loop_k_ctrl_*, loop_v_* -> loop_v_ctrl_*,
                                loop_mlp_* -> loop_ffn_ctrl_*
  6. --fuse-loop-queries         две глобальные матрицы запросов заменяются
                                ДВЕНАДЦАТЬЮ blk.{core}.loop_msg_query.weight в
                                ТЕХ ЖЕ байтах данных (обе матрицы вместе дают
                                ровно 12*2304*4*4 = 442368 Б). Правило фолда —
                                то же, что в конвертере (пункт выше). Это не
                                выдуманные веса: строки копируются из матриц,
                                которые уже лежат в файле.

Без --fuse-loop-queries получается «только метаданные»: такой файл доходит до
create_tensor и падает на missing tensor 'blk.6.loop_msg_query.weight' — это и
есть граница метаданных (docs/features/inkling2-loop.md).

ВАЖНО про раскладку данных. gguf_init_from_reader требует, чтобы смещения
тензоров в порядке их описаний шли подряд от нуля (ошибка
"tensor 'X' has offset N, expected M" иначе). Поэтому матрицы запросов НЕ
удаляются из списка, а заменяются на месте: две записи по 221184 Б становятся
двенадцатью записями по 36864 Б с теми же суммарными байтами. Остальные
описания не меняются, данные копируются как есть.

Использование:
  python scripts/gguf_loop_meta_patch.py --src <in.gguf> --dry-run
  python scripts/gguf_loop_meta_patch.py --src <in.gguf> --dst <out.gguf> --fuse-loop-queries
  python scripts/gguf_loop_meta_patch.py --check <file.gguf>      # только факты
  python scripts/gguf_loop_meta_patch.py --verify-data <src> <dst> # побайтовая сверка

Зависимостей нет (только стандартная библиотека): заголовок разбирается
собственным парсером GGUF v3, данные копируются кусками.
"""

from __future__ import annotations

import argparse
import json
import mmap
import os
import shutil
import struct
import sys
import time
from dataclasses import dataclass, field

GGUF_MAGIC = b"GGUF"
DEFAULT_ALIGNMENT = 32

# GGUF value types (gguf.h)
T_UINT8, T_INT8, T_UINT16, T_INT16, T_UINT32, T_INT32, T_FLOAT32 = 0, 1, 2, 3, 4, 5, 6
T_BOOL, T_STRING, T_ARRAY, T_UINT64, T_INT64, T_FLOAT64 = 7, 8, 9, 10, 11, 12

SCALAR_FMT = {
    T_UINT8: "<B", T_INT8: "<b", T_UINT16: "<H", T_INT16: "<h",
    T_UINT32: "<I", T_INT32: "<i", T_FLOAT32: "<f",
    T_BOOL: "<B", T_UINT64: "<Q", T_INT64: "<q", T_FLOAT64: "<d",
}

# имена тензоров по визитам/экспорту -> имена c/llama.cpp (llama-arch.cpp)
CTRL_RENAMES = (
    ("loop_attn_", "loop_q_ctrl_"),
    ("loop_k_", "loop_k_ctrl_"),
    ("loop_v_", "loop_v_ctrl_"),
    ("loop_mlp_", "loop_ffn_ctrl_"),
)
CTRL_SUFFIXES = ("down", "gain", "forget")

# ключи loop-схемы: канонические (их читает загрузчик) и алиасы (их читает
# Go-оценщик KV, internal/cppbackend/backend_gguf.go: .num_loops, .loop.core_*)
LOOP_KEYS_LOADER = ("loop.start", "loop.end", "loop.count", "loop.gate_rank", "loop.varying")
LOOP_ALIASES = ("num_loops", "loop.core_start", "loop.core_end")

COPY_CHUNK = 64 << 20


class PatchError(RuntimeError):
    """Файл не подходит под патч; текст печатается человеку."""


# ---------------------------------------------------------------------------
# разбор GGUF-заголовка
# ---------------------------------------------------------------------------

def _u32(buf, off: int) -> int:
    return struct.unpack_from("<I", buf, off)[0]


def _u64(buf, off: int) -> int:
    return struct.unpack_from("<Q", buf, off)[0]


def _align(n: int, a: int = DEFAULT_ALIGNMENT) -> int:
    return (n + a - 1) // a * a


def _read_string(buf, off: int):
    n = _u64(buf, off)
    off += 8
    return bytes(buf[off:off + n]), off + n


def _scalar_size(vtype: int) -> int:
    if vtype not in SCALAR_FMT:
        raise PatchError("неизвестный GGUF-тип значения: %d" % vtype)
    return struct.calcsize(SCALAR_FMT[vtype])


def _value_end(buf, off: int, vtype: int) -> int:
    if vtype in SCALAR_FMT:
        return off + _scalar_size(vtype)
    if vtype == T_STRING:
        return off + 8 + _u64(buf, off)
    if vtype == T_ARRAY:
        elem_type = _u32(buf, off)
        count = _u64(buf, off + 4)
        off += 12
        if elem_type == T_STRING:
            for _ in range(count):
                off += 8 + _u64(buf, off)
            return off
        return off + count * _scalar_size(elem_type)
    raise PatchError("неизвестный GGUF-тип значения: %d" % vtype)


def _scalar_value(buf, off: int, vtype: int):
    return struct.unpack_from(SCALAR_FMT[vtype], buf, off)[0]


def _array_values(buf, off: int):
    """(elem_type, [values]) для маленького массива (массивы по слоям)."""
    elem_type = _u32(buf, off)
    count = _u64(buf, off + 4)
    if elem_type == T_STRING:
        raise PatchError("строковый массив не поддерживается")
    size = _scalar_size(elem_type)
    fmt = SCALAR_FMT[elem_type]
    return elem_type, [struct.unpack_from(fmt, buf, off + 12 + i * size)[0] for i in range(count)]


@dataclass
class KVEntry:
    key: str
    vtype: int
    raw: bytes            # payload значения без типа
    value: object = None


@dataclass
class TensorInfo:
    name: str
    dims: tuple
    ttype: int
    offset: int


@dataclass
class GGUFHeader:
    version: int
    kv: list = field(default_factory=list)
    tensors: list = field(default_factory=list)
    data_start: int = 0
    file_size: int = 0

    def get(self, key):
        for e in self.kv:
            if e.key == key:
                return e
        return None

    def tensor(self, name):
        for t in self.tensors:
            if t.name == name:
                return t
        return None

    def names(self):
        return {t.name for t in self.tensors}


def read_header(buf) -> GGUFHeader:
    if bytes(buf[0:4]) != GGUF_MAGIC:
        raise PatchError("не GGUF: magic=%r" % bytes(buf[0:4]))
    version = _u32(buf, 4)
    n_tensors = _u64(buf, 8)
    n_kv = _u64(buf, 16)
    pos = 24
    hdr = GGUFHeader(version=version, file_size=len(buf))
    for _ in range(n_kv):
        key, pos = _read_string(buf, pos)
        vtype = _u32(buf, pos)
        pos += 4
        val_start = pos
        pos = _value_end(buf, pos, vtype)
        raw = bytes(buf[val_start:pos])
        value = None
        if vtype in SCALAR_FMT:
            value = _scalar_value(buf, val_start, vtype)
        elif vtype == T_STRING:
            value = raw[8:].decode("utf-8", "replace")
        elif vtype == T_ARRAY:
            elem_type = _u32(buf, val_start)
            count = _u64(buf, val_start + 4)
            if elem_type != T_STRING and count <= 4096:
                value = _array_values(buf, val_start)[1]
        hdr.kv.append(KVEntry(key=key.decode("utf-8", "replace"), vtype=vtype, raw=raw, value=value))
    for _ in range(n_tensors):
        name, pos = _read_string(buf, pos)
        n_dims = _u32(buf, pos)
        pos += 4
        dims = tuple(struct.unpack_from("<%dQ" % n_dims, buf, pos))
        pos += 8 * n_dims
        ttype = _u32(buf, pos)
        offset = _u64(buf, pos + 4)
        pos += 12
        hdr.tensors.append(TensorInfo(name=name.decode("utf-8", "replace"),
                                      dims=dims, ttype=ttype, offset=offset))
    hdr.data_start = _align(pos)
    return hdr


# ---------------------------------------------------------------------------
# запись GGUF-заголовка
# ---------------------------------------------------------------------------

def _enc_str(text: str) -> bytes:
    b = text.encode("utf-8")
    return struct.pack("<Q", len(b)) + b


def build_header(kv_entries, tensors) -> bytes:
    out = bytearray(GGUF_MAGIC)
    out += struct.pack("<I", 3)
    out += struct.pack("<Q", len(tensors))
    out += struct.pack("<Q", len(kv_entries))
    for e in kv_entries:
        out += _enc_str(e.key) + struct.pack("<I", e.vtype) + e.raw
    for t in tensors:
        out += _enc_str(t.name)
        out += struct.pack("<I", len(t.dims))
        for d in t.dims:
            out += struct.pack("<Q", d)
        out += struct.pack("<I", t.ttype)
        out += struct.pack("<Q", t.offset)
    out += b"\x00" * (_align(len(out)) - len(out))
    return bytes(out)


# ---------------------------------------------------------------------------
# факты и проверки схемы
# ---------------------------------------------------------------------------

def visits_for(block_count: int, core_start: int, core_end: int, passes: int):
    """Раскладка визитов слоя: прелюдия + ядро x проходы + постудия."""
    return (list(range(0, core_start))
            + list(range(core_start, core_end)) * passes
            + list(range(core_end, block_count)))


def physical_count(names) -> int:
    phys = set()
    for name in names:
        parts = name.split(".")
        if len(parts) >= 2 and parts[0] == "blk" and parts[1].isdigit():
            phys.add(int(parts[1]))
    return (max(phys) + 1) if phys else 0


def _expect(cond: bool, msg: str):
    if not cond:
        raise PatchError(msg)


@dataclass
class Plan:
    facts: dict
    kv_changes: list
    ctrl_renames: list
    fuse: bool
    regions: list = field(default_factory=list)  # [(имя матрицы, [(блок, offset)])]
    msg_query: list = field(default_factory=list)  # [(имя, offset)]

    @property
    def physical(self):
        return self.facts["physical_blocks"]

    @property
    def core_start(self):
        return self.facts["core"][0]

    @property
    def core_end(self):
        return self.facts["core"][1]

    @property
    def passes(self):
        return self.facts["passes"]


def make_plan(hdr: GGUFHeader, fuse: bool) -> Plan:
    arch_e = hdr.get("general.architecture")
    _expect(arch_e is not None, "в файле нет general.architecture")
    arch = arch_e.value
    _expect(arch in ("inkling2", "inkling2_loop"),
            "ожидался general.architecture=inkling2, получено %r" % (arch,))

    block_e = hdr.get("%s.block_count" % arch)
    _expect(block_e is not None, "нет %s.block_count" % arch)
    block_count = int(block_e.value)

    names = hdr.names()
    physical = physical_count(names)
    _expect(physical > 0, "в файле нет тензоров blk.N.*")

    def _loop(key):
        return hdr.get("%s.%s" % (arch, key)) or hdr.get("inkling2_loop.%s" % key)

    e_start, e_end = _loop("loop.core_start"), _loop("loop.core_end")
    e_pass, e_rank = _loop("num_loops"), _loop("loop.gate_rank")
    if (e_pass is None or e_start is None) and hdr.get("inkling2_loop.loop.count"):
        e_pass, e_start = hdr.get("inkling2_loop.loop.count"), hdr.get("inkling2_loop.loop.start")
    if e_end is None:
        e_end = hdr.get("inkling2_loop.loop.end")
    _expect(e_start is not None and e_end is not None and e_pass is not None and e_rank is not None,
            "в файле нет полного набора loop-ключей (core_start/core_end/num_loops/gate_rank)")
    core_start, core_end, passes = int(e_start.value), int(e_end.value), int(e_pass.value)
    gate_rank = int(e_rank.value)

    _expect(0 < core_start < core_end <= physical,
            "ядро цикла [%d,%d) не укладывается в %d физических блоков" % (core_start, core_end, physical))
    _expect(passes >= 2, "num_loops=%d: loop-схема требует >= 2 проходов" % passes)

    visits = visits_for(physical, core_start, core_end, passes)
    _expect(len(visits) == block_count,
            "block_count=%d не равно числу визитов %d (ядро [%d,%d) x %d)"
            % (block_count, len(visits), core_start, core_end, passes))

    n_embd_e = hdr.get("%s.embedding_length" % arch)
    _expect(n_embd_e is not None, "нет %s.embedding_length" % arch)
    n_embd = int(n_embd_e.value)

    # массивы по визитам: длина = block_count, и каждый визит повторяет свой слой
    for suffix in ("attention.head_count", "attention.head_count_kv", "attention.sliding_window_pattern"):
        e = hdr.get("%s.%s" % (arch, suffix))
        _expect(e is not None, "нет %s.%s" % (arch, suffix))
        _expect(e.vtype == T_ARRAY, "%s.%s не массив" % (arch, suffix))
        _expect(len(e.value) == block_count,
                "%s.%s: длина %d, ожидалось block_count=%d" % (arch, suffix, len(e.value), block_count))
        for j in range(physical, block_count):
            _expect(e.value[j] == e.value[visits[j]],
                    "%s.%s[%d]=%r, но визит %d ведёт в слой %d со значением %r — массив не по визитам"
                    % (arch, suffix, j, e.value[j], j, visits[j], e.value[visits[j]]))

    # контроллеры ядра: наличие и формы
    missing = []
    renames = []
    for il in range(core_start, core_end):
        for src, dst in CTRL_RENAMES:
            for suf in CTRL_SUFFIXES:
                old = "blk.%d.%s%s.weight" % (il, src, suf)
                new = "blk.%d.%s%s.weight" % (il, dst, suf)
                t = hdr.tensor(old) or hdr.tensor(new)
                if t is None:
                    missing.append(old)
                    continue
                want = (2 * n_embd, gate_rank) if suf == "down" else (gate_rank, n_embd)
                _expect(tuple(t.dims) == want, "%s: форма %r, ожидалось %r" % (t.name, t.dims, want))
                if old in names:
                    renames.append((old, new))
    _expect(not missing, "нет контроллеров ядра (%d): %s" % (len(missing), ", ".join(missing[:6])))

    # запросы межпроходных сообщений: две матрицы -> core x loop_msg_query
    regions = []
    msg_query = []
    core = core_end - core_start
    if fuse:
        per = n_embd * 4 * 4  # байт на один blk.N.loop_msg_query.weight (F32, 4 строки)
        q_t = hdr.tensor("loop_res_query.weight")
        kv_t = hdr.tensor("loop_kv_res_query.weight")
        _expect(q_t is not None and kv_t is not None,
                "нет loop_res_query/loop_kv_res_query — не из чего собрать loop_msg_query")
        want = (n_embd, 2 * core)
        for t in (q_t, kv_t):
            _expect(tuple(t.dims) == want, "%s: форма %r, ожидалось %r" % (t.name, t.dims, want))
            _expect(t.ttype == 0, "%s: не F32" % t.name)
            _expect(t.offset % DEFAULT_ALIGNMENT == 0, "%s: offset %d не выровнен" % (t.name, t.offset))
            _expect(2 * (2 * core) * n_embd * 4 == core * 4 * n_embd * 4,
                    "площадь матриц запросов не равна %d loop_msg_query" % core)
        # матрицы соседние и покрывают ровно core*per байт вдвоём
        order = sorted([q_t, kv_t], key=lambda t: t.offset)
        _expect(order[0].offset == 0,
                "первая матрица запросов не с offset 0 (%d): gguf_init_from_reader ждёт данные с нуля"
                % order[0].offset)
        _expect(order[1].offset == order[0].offset + (core // 2) * per,
                "матрицы запросов не идут подряд: %d и %d" % (order[0].offset, order[1].offset))
        _expect(hdr.tensors[0] in order and hdr.tensors[1] in order,
                "матрицы запросов должны быть первыми двумя описаниями тензоров")
        for k, mat in enumerate(order):
            blocks = list(range(core_start + k * (core // 2), core_start + (k + 1) * (core // 2)))
            _expect(len(blocks) == core // 2, "ядро %d не делится на 2 области" % core)
            entries = []
            for j, blk in enumerate(blocks):
                off = mat.offset + j * per
                name = "blk.%d.loop_msg_query.weight" % blk
                entries.append((blk, off))
                msg_query.append((name, off))
            regions.append((mat.name, entries))
    else:
        for il in range(core_start, core_end):
            _expect(hdr.tensor("blk.%d.loop_msg_query.weight" % il) is None,
                    "файл уже содержит loop_msg_query — патч не требуется")

    facts = {
        "version": hdr.version,
        "arch": arch,
        "block_count": block_count,
        "physical_blocks": physical,
        "visits": len(visits),
        "core": [core_start, core_end],
        "passes": passes,
        "gate_rank": gate_rank,
        "tensors": len(hdr.tensors),
        "data_start": hdr.data_start,
        "msg_query": len(msg_query),
    }
    kv_changes = [
        "general.architecture: %s -> inkling2_loop" % arch,
        "%s.block_count: %d -> %d (физические слои)" % (arch, block_count, physical),
        "префикс ключей: %s.* -> inkling2_loop.*" % arch,
    ]
    for suffix in ("attention.head_count", "attention.head_count_kv", "attention.sliding_window_pattern"):
        kv_changes.append("%s.%s: длина %d -> %d" % (arch, suffix, block_count, physical))
    kv_changes.append("добавлены: inkling2_loop.loop.start/end/count/varying "
                      "(+ алиасы num_loops, loop.core_start/end для оценки KV)")
    return Plan(facts=facts, kv_changes=kv_changes, ctrl_renames=renames, fuse=fuse,
                regions=regions, msg_query=msg_query)


# ---------------------------------------------------------------------------
# новый заголовок
# ---------------------------------------------------------------------------

def _renamed_key(key: str) -> str:
    if key.startswith("inkling2."):
        return "inkling2_loop." + key[len("inkling2."):]
    return key


def build_new_kv(hdr: GGUFHeader, plan: Plan) -> list:
    arch = plan.facts["arch"]
    physical = plan.physical
    out = []
    for e in hdr.kv:
        raw = e.raw
        if e.key == "general.architecture":
            raw = _enc_str("inkling2_loop")
        elif e.key == "%s.block_count" % arch:
            raw = struct.pack("<I", physical)
        elif e.key in ("%s.attention.head_count" % arch,
                       "%s.attention.head_count_kv" % arch,
                       "%s.attention.sliding_window_pattern" % arch):
            elem_type = _u32(e.raw, 0)
            elem_size = _scalar_size(elem_type)
            raw = struct.pack("<I", elem_type) + struct.pack("<Q", physical) + e.raw[12:12 + physical * elem_size]
        out.append(KVEntry(key=_renamed_key(e.key), vtype=e.vtype, raw=raw, value=e.value))

    have = {e.key for e in out}
    loop_values = {
        "loop.start": plan.core_start,
        "loop.end": plan.core_end,
        "loop.count": plan.passes,
    }
    for suffix, value in loop_values.items():
        key = "inkling2_loop." + suffix
        if key not in have:
            out.append(KVEntry(key=key, vtype=T_UINT32, raw=struct.pack("<I", value)))
    if "inkling2_loop.loop.varying" not in have:
        out.append(KVEntry(key="inkling2_loop.loop.varying", vtype=T_BOOL, raw=struct.pack("<B", 0)))
    return out


def build_new_tensors(hdr: GGUFHeader, plan: Plan):
    """Описания тензоров: переименование контроллеров и замена двух матриц
    запросов на core x loop_msg_query (смещения не меняются)."""
    renames = dict(plan.ctrl_renames)
    region = {name: entries for name, entries in plan.regions}
    out = []
    for t in hdr.tensors:
        entries = region.get(t.name)
        if entries is not None:
            for blk, off in entries:
                out.append(TensorInfo("blk.%d.loop_msg_query.weight" % blk,
                                      (t.dims[0], 4), 0, off))
            continue
        out.append(TensorInfo(renames.get(t.name, t.name), t.dims, t.ttype, t.offset))
    return out


# ---------------------------------------------------------------------------
# данные: фолд матриц запросов
# ---------------------------------------------------------------------------

def fold_queries(q_bytes: bytes, kv_bytes: bytes, n_embd: int, core: int):
    """msg_query[i] = stack([q[2i], kv[2i], kv[2i+1], q[2i+1]]) — conversion/inkling2.py:328."""
    row = n_embd * 4
    _expect(len(q_bytes) == core * 2 * row, "loop_res_query: %d Б вместо %d" % (len(q_bytes), core * 2 * row))
    _expect(len(kv_bytes) == core * 2 * row, "loop_kv_res_query: %d Б вместо %d" % (len(kv_bytes), core * 2 * row))
    out = []
    for i in range(core):
        out.append(q_bytes[(2 * i) * row:(2 * i + 1) * row]
                   + kv_bytes[(2 * i) * row:(2 * i + 1) * row]
                   + kv_bytes[(2 * i + 1) * row:(2 * i + 2) * row]
                   + q_bytes[(2 * i + 1) * row:(2 * i + 2) * row])
    return out


def _region_bytes(mm, hdr: GGUFHeader, name: str) -> bytes:
    t = hdr.tensor(name)
    _expect(t is not None, "нет " + name)
    n = 1
    for d in t.dims:
        n *= d
    _expect(t.ttype == 0, "%s: не F32" % name)
    n *= 4
    return bytes(mm[hdr.data_start + t.offset: hdr.data_start + t.offset + n])


def _write_progress(done: int, total: int, last=[0.0]):
    now = time.time()
    if now - last[0] < 5.0 and done < total:
        return
    last[0] = now
    sys.stderr.write("\r  копирование данных: %.1f%% (%d/%d МиБ)"
                     % (100.0 * done / total if total else 100.0, done >> 20, total >> 20))
    sys.stderr.flush()
    if done >= total:
        sys.stderr.write("\n")


def write_patched(src_path: str, dst_path: str, plan: Plan, hdr: GGUFHeader):
    core = plan.core_end - plan.core_start
    n_embd = hdr.get("%s.embedding_length" % plan.facts["arch"]).value
    folds = None
    if plan.fuse:
        with open(src_path, "rb") as f:
            mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
            try:
                q_b = _region_bytes(mm, hdr, "loop_res_query.weight")
                kv_b = _region_bytes(mm, hdr, "loop_kv_res_query.weight")
            finally:
                mm.close()
        folds = fold_queries(q_b, kv_b, n_embd, core)

    kv = build_new_kv(hdr, plan)
    tensors = build_new_tensors(hdr, plan)
    header = build_header(kv, tensors)
    data_size = hdr.file_size - hdr.data_start
    need = len(header) + data_size
    free = shutil.disk_usage(os.path.dirname(os.path.abspath(dst_path))).free
    _expect(free > need + (64 << 20), "мало места: нужно %d Б, свободно %d Б" % (need, free))

    with open(src_path, "rb") as fsrc:
        mm = mmap.mmap(fsrc.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            with open(dst_path, "wb") as fdst:
                fdst.write(header)
                copied = 0
                while copied < data_size:
                    n = min(COPY_CHUNK, data_size - copied)
                    fdst.write(mm[hdr.data_start + copied: hdr.data_start + copied + n])
                    copied += n
                    if data_size > COPY_CHUNK:
                        _write_progress(copied, data_size)
                if folds is not None:
                    # те же байты данных: две матрицы запросов -> core x msg_query
                    for _name, entries in plan.regions:
                        for blk, off in entries:
                            fdst.seek(len(header) + off)
                            fdst.write(folds[blk - plan.core_start])
                fdst.flush()
                os.fsync(fdst.fileno())
        finally:
            mm.close()


# ---------------------------------------------------------------------------
# проверка результата
# ---------------------------------------------------------------------------

def _expected_offsets(hdr: GGUFHeader, plan: Plan):
    """Ожидаемые смещения в порядке описаний: подряд от нуля (правило gguf_init)."""
    if not plan.fuse:
        return [t.offset for t in hdr.tensors]
    region = {name: entries for name, entries in plan.regions}
    out = []
    for t in hdr.tensors:
        entries = region.get(t.name)
        if entries is None:
            out.append(t.offset)
        else:
            out.extend(off for _blk, off in entries)
    return out


def verify_patched(dst_path: str, src_path: str, plan: Plan) -> dict:
    with open(dst_path, "rb") as f:
        mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            hdr = read_header(mm)
            arch = hdr.get("general.architecture").value
            _expect(arch == "inkling2_loop", "general.architecture=%r" % (arch,))
            _expect(int(hdr.get("inkling2_loop.block_count").value) == plan.physical,
                    "block_count != %d" % plan.physical)
            for suffix in ("attention.head_count", "attention.head_count_kv",
                           "attention.sliding_window_pattern"):
                e = hdr.get("inkling2_loop." + suffix)
                _expect(e is not None, "нет inkling2_loop." + suffix)
                _expect(len(e.value) == plan.physical,
                        "%s: длина %d, ожидалось %d" % (suffix, len(e.value), plan.physical))
            for suffix, want in (("loop.start", plan.core_start), ("loop.end", plan.core_end),
                                 ("loop.count", plan.passes)):
                e = hdr.get("inkling2_loop." + suffix)
                _expect(e is not None and int(e.value) == want,
                        "inkling2_loop.%s=%r, ожидалось %r" % (suffix, e and e.value, want))
            for suffix in ("loop.gate_rank",) + LOOP_ALIASES:
                _expect(hdr.get("inkling2_loop." + suffix) is not None, "нет inkling2_loop." + suffix)

            names = hdr.names()
            for _old, new in plan.ctrl_renames:
                _expect(new in names, "нет переименованного тензора " + new)
            if plan.fuse:
                for name in ("loop_res_query.weight", "loop_kv_res_query.weight"):
                    _expect(name not in names, "не заменена матрица " + name)
                _expect(len(plan.msg_query) == plan.core_end - plan.core_start,
                        "msg_query: %d вместо %d" % (len(plan.msg_query), plan.core_end - plan.core_start))

            # порядок описаний и смещения: подряд от нуля, как ждёт gguf_init_from_reader
            offsets = [t.offset for t in hdr.tensors]
            _expect(offsets == sorted(offsets), "описания тензоров не упорядочены по offset")
            _expect(offsets[0] == 0, "первый тензор не с offset 0 (%d)" % offsets[0])
            _expect(len(offsets) == len(set(offsets)), "у двух тензоров один offset")
            _expect(offsets == _expected_offsets(_read_src_header(src_path), plan),
                    "смещения не совпали с планом: ожидалось %d тензоров с тем же порядком"
                    % len(_expected_offsets(_read_src_header(src_path), plan)))

            # данные msg_query сверяются с фолдом матриц ИСХОДНОГО файла
            if plan.fuse:
                with open(src_path, "rb") as fs:
                    mms = mmap.mmap(fs.fileno(), 0, access=mmap.ACCESS_READ)
                    try:
                        src = read_header(mms)
                        n_embd = src.get("%s.embedding_length" % plan.facts["arch"]).value
                        core = plan.core_end - plan.core_start
                        folds = fold_queries(_region_bytes(mms, src, "loop_res_query.weight"),
                                             _region_bytes(mms, src, "loop_kv_res_query.weight"),
                                             n_embd, core)
                        per = n_embd * 4 * 4
                        for idx, (name, off) in enumerate(plan.msg_query):
                            t = hdr.tensor(name)
                            _expect(t is not None, "нет " + name)
                            _expect(tuple(t.dims) == (n_embd, 4), "%s: форма %r" % (name, t.dims))
                            _expect(t.ttype == 0, "%s: не F32" % name)
                            got = bytes(mm[hdr.data_start + t.offset: hdr.data_start + t.offset + per])
                            _expect(got == folds[idx], "%s: данные не равны фолду матриц" % name)
                    finally:
                        mms.close()
            return {
                "arch": arch,
                "block_count": int(hdr.get("inkling2_loop.block_count").value),
                "tensors": len(hdr.tensors),
                "data_start": hdr.data_start,
                "visits": plan.facts["visits"],
                "loop": [plan.core_start, plan.core_end, plan.passes],
                "msg_query": plan.facts["msg_query"],
            }
        finally:
            mm.close()


def _read_src_header(path: str) -> GGUFHeader:
    with open(path, "rb") as f:
        mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            return read_header(mm)
        finally:
            mm.close()


def verify_data_identical(src_path: str, dst_path: str) -> None:
    """Побайтовая сверка данных: всё совпадает, кроме двух заменённых областей."""
    src_hdr = _read_src_header(src_path)
    dst_hdr = _read_src_header(dst_path)
    src_size = src_hdr.file_size - src_hdr.data_start
    dst_size = dst_hdr.file_size - dst_hdr.data_start
    _expect(src_size == dst_size, "размер данных: %d в источнике, %d в назначении" % (src_size, dst_size))

    replaced = []
    if dst_hdr.tensor("blk.6.loop_msg_query.weight") is not None or any(
            t.name.endswith("loop_msg_query.weight") for t in dst_hdr.tensors):
        for name in ("loop_res_query.weight", "loop_kv_res_query.weight"):
            t = src_hdr.tensor(name)
            _expect(t is not None, "нет " + name)
            n = 1
            for d in t.dims:
                n *= d
            replaced.append((t.offset, t.offset + n * 4))
        replaced.sort()

    # сравниваем куски между заменёнными областями: там должен быть полный побайтовый паритет
    segments = []
    pos = 0
    for lo, hi in replaced:
        if lo > pos:
            segments.append((pos, lo))
        pos = max(pos, hi)
    if pos < src_size:
        segments.append((pos, src_size))
    _expect(bool(segments), "нечего сравнивать")

    with open(src_path, "rb") as fs, open(dst_path, "rb") as fd:
        mms = mmap.mmap(fs.fileno(), 0, access=mmap.ACCESS_READ)
        mmd = mmap.mmap(fd.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            for lo, hi in segments:
                pos = lo
                while pos < hi:
                    n = min(COPY_CHUNK, hi - pos)
                    a = mms[src_hdr.data_start + pos: src_hdr.data_start + pos + n]
                    b = mmd[dst_hdr.data_start + pos: dst_hdr.data_start + pos + n]
                    if a != b:
                        _expect(False, "данные разошлись на offset %d (%d..%d)" % (pos, lo, hi))
                    pos += n
        finally:
            mms.close()
            mmd.close()


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _facts_text(path: str, hdr: GGUFHeader) -> str:
    arch = hdr.get("general.architecture")
    arch = arch.value if arch else "?"
    lines = ["файл: %s" % path,
             "gguf: v%d, ключей %d, тензоров %d, данные с %d"
             % (hdr.version, len(hdr.kv), len(hdr.tensors), hdr.data_start),
             "general.architecture: %s" % arch,
             "физических блоков: %d" % physical_count(hdr.names())]
    for suffix in ("block_count", "num_loops", "loop.core_start", "loop.core_end",
                   "loop.gate_rank", "loop.start", "loop.end", "loop.count", "loop.varying"):
        e = hdr.get("%s.%s" % (arch, suffix))
        lines.append("  %-24s %s" % (suffix, e.value if e else "НЕТ"))
    for suffix in ("attention.head_count", "attention.head_count_kv", "attention.sliding_window_pattern"):
        e = hdr.get("%s.%s" % (arch, suffix))
        lines.append("  %-24s %s" % (suffix, "длина %d" % len(e.value) if e and e.value is not None else "НЕТ"))
    msg = [t.name for t in hdr.tensors if t.name.endswith("loop_msg_query.weight")]
    lines.append("  loop_msg_query             %s" % ("%d тензоров" % len(msg) if msg else "НЕТ"))
    return "\n".join(lines)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description="Патч метаданных loop-экспорта Inkling-2 под схему inkling2_loop (c/llama.cpp).")
    ap.add_argument("--src", help="исходный GGUF")
    ap.add_argument("--dst", help="куда писать пропатченный GGUF (--src не изменяется)")
    ap.add_argument("--dry-run", action="store_true", help="только показать план, ничего не писать")
    ap.add_argument("--fuse-loop-queries", action="store_true",
                    help="заменить loop_res_query/loop_kv_res_query на core x loop_msg_query "
                         "в тех же байтах данных (правило конвертера)")
    ap.add_argument("--force", action="store_true", help="перезаписать существующий --dst")
    ap.add_argument("--check", metavar="FILE", help="только факты заголовка, без патча")
    ap.add_argument("--verify-data", nargs=2, metavar=("SRC", "DST"),
                    help="побайтово сверить данные SRC и DST (кроме заменённых областей)")
    ap.add_argument("--json", action="store_true", help="факты/план в JSON")
    args = ap.parse_args(argv)

    if args.check:
        hdr = _read_src_header(args.check)
        print(_facts_text(args.check, hdr))
        return 0

    if args.verify_data:
        verify_data_identical(args.verify_data[0], args.verify_data[1])
        print("данные совпадают везде, кроме заменённых областей матриц запросов")
        return 0

    if not args.src:
        ap.error("нужен --src (или --check)")
    if not os.path.isfile(args.src):
        print("нет файла: %s" % args.src, file=sys.stderr)
        return 2

    hdr = _read_src_header(args.src)
    try:
        plan = make_plan(hdr, args.fuse_loop_queries)
    except PatchError as e:
        print("патч не нужен/невозможен: %s" % e, file=sys.stderr)
        return 3

    if args.json:
        print(json.dumps({"facts": plan.facts, "kv_changes": plan.kv_changes,
                          "ctrl_renames": len(plan.ctrl_renames),
                          "msg_query": [n for n, _ in plan.msg_query]},
                         ensure_ascii=False, indent=2))
    else:
        print(_facts_text(args.src, hdr))
        print("\nплан патча:")
        for line in plan.kv_changes:
            print("  * " + line)
        print("  * контроллеры: переименовано %d тензоров (attn/k/v/mlp -> q/k/v/ffn _ctrl)"
              % len(plan.ctrl_renames))
        if plan.fuse:
            print("  * %d тензоров loop_msg_query займут байты двух матриц запросов "
                  "(blk.%d..blk.%d)" % (len(plan.msg_query), plan.core_start, plan.core_end - 1))
        else:
            print("  * loop_msg_query НЕ добавляется: загрузчик дойдёт до missing tensor")
        print("  * визиты: %d, физических слоёв: %d" % (plan.facts["visits"], plan.physical))

    if args.dry_run:
        print("\ndry-run: файл не записан")
        return 0
    if not args.dst:
        ap.error("нужен --dst (или --dry-run)")
    if os.path.exists(args.dst) and not args.force:
        print("файл назначения уже есть: %s (--force чтобы перезаписать)" % args.dst, file=sys.stderr)
        return 2

    print("пишу %s ..." % args.dst)
    write_patched(args.src, args.dst, plan, hdr)
    facts = verify_patched(args.dst, args.src, plan)
    print("готово: %s" % json.dumps(facts, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
