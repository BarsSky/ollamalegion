#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""test_gguf_loop_meta_patch.py — тесты патча loop-схемы Inkling-2.

Проверяется на СИНТЕТИЧЕСКОМ мини-GGUF (никаких весов не нужно):
  1. после --fuse-loop-queries в файле arch=inkling2_loop, block_count =
     физические слои, массивы по визитам усечены, loop-ключи и контроллеры
     переименованы, loop_msg_query собран из матриц запросов;
  2. без --fuse-loop-queries метаданные уже верные, но loop_msg_query НЕТ —
     загрузчик дойдёт до missing tensor (отрицательный сценарий);
  3. ДО патча исходный заголовок объявляет block_count = число визитов (5), а
     физических блоков 3 — загрузчик искал бы blk.3 и падал;
  4. битый массив по визитам (визит не повторяет свой слой) отклоняется.

Запуск: python scripts/test_gguf_loop_meta_patch.py
"""
import importlib.util
import os
import struct
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))

# значения из "игрушечной" модели: 3 физических слоя, ядро [1,3), 2 прохода
N_EMBD = 8
GATE_RANK = 2
CORE_START, CORE_END, PASSES = 1, 3, 2
PHYSICAL = 3
VISITS = [0, 1, 2, 1, 2]          # 1 + 2*2 = 5 визитов
BLOCK_COUNT = len(VISITS)         # то, что объявляет loop-экспорт
MSG_QUERY_BYTES = N_EMBD * 4 * 4  # F32, 4 строки
SWA_PATTERN = [True, False, True, False, True]  # визит повторяет свой слой


# ---------------------------------------------------------------------------
# независимый мини-писатель GGUF (тест не должен зависеть от кода патча)
# ---------------------------------------------------------------------------

def _str(text):
    b = text.encode("utf-8")
    return struct.pack("<Q", len(b)) + b


def _kv(key, vtype, payload):
    return _str(key) + struct.pack("<I", vtype) + payload


def _u32(v):
    return struct.pack("<I", v)


def _arr_u32(values):
    return struct.pack("<I", 4) + struct.pack("<Q", len(values)) + b"".join(_u32(v) for v in values)


def _arr_bool(values):
    return struct.pack("<I", 7) + struct.pack("<Q", len(values)) + bytes(1 if v else 0 for v in values)


def _tensor_info(name, dims, ttype, offset):
    out = _str(name) + struct.pack("<I", len(dims))
    for d in dims:
        out += struct.pack("<Q", d)
    out += struct.pack("<I", ttype) + struct.pack("<Q", offset)
    return out


def _nbytes(dims):
    n = 1
    for d in dims:
        n *= d
    return n * 4  # только F32 в фикстуре


def _align(n):
    return (n + 31) // 32 * 32


def write_fixture(path, arch="inkling2", block_count=BLOCK_COUNT, swa_pattern=None, fuse_globals=True):
    """Пишет мини-GGUF: две матрицы запросов (offset 0 и region) + контроллеры.

    Раскладка данных повторяет настоящий файл: первые два описания — матрицы
    запросов, их байты идут первыми, дальше остальные тензоры подряд.
    """
    swa_pattern = SWA_PATTERN if swa_pattern is None else swa_pattern
    per = MSG_QUERY_BYTES
    region = (CORE_END - CORE_START) // 2 * per
    q_bytes = bytes((i * 7) & 0xFF for i in range(region))
    kv_bytes = bytes((i * 13 + 5) & 0xFF for i in range(region))

    tensors = []  # (name, dims, ttype, data)
    if fuse_globals:
        # матрицы запросов: [n_embd, 2*core] F32
        tensors.append(("loop_kv_res_query.weight", (N_EMBD, 2 * (CORE_END - CORE_START)), 0, kv_bytes))
        tensors.append(("loop_res_query.weight", (N_EMBD, 2 * (CORE_END - CORE_START)), 0, q_bytes))
    # базовый тензор блока 0, чтобы physical_count увидел слой 0
    tensors.append(("blk.0.attn_q.weight", (N_EMBD, N_EMBD), 0, b"\x00" * (N_EMBD * N_EMBD * 4)))
    # контроллеры ядра в именах loop-экспорта
    for il in range(CORE_START, CORE_END):
        for ctrl in ("attn", "k", "v", "mlp"):
            tensors.append(("blk.%d.loop_%s_down.weight" % (il, ctrl), (2 * N_EMBD, GATE_RANK), 0,
                            b"\x00" * (2 * N_EMBD * GATE_RANK * 4)))
            tensors.append(("blk.%d.loop_%s_gain.weight" % (il, ctrl), (GATE_RANK, N_EMBD), 0,
                            b"\x00" * (GATE_RANK * N_EMBD * 4)))
            tensors.append(("blk.%d.loop_%s_forget.weight" % (il, ctrl), (GATE_RANK, N_EMBD), 0,
                            b"\x00" * (GATE_RANK * N_EMBD * 4)))

    infos = []
    data = bytearray()
    for name, dims, ttype, blob in tensors:
        infos.append(_tensor_info(name, dims, ttype, len(data)))
        data += blob
        data += b"\x00" * (_align(len(data)) - len(data))

    arch = arch
    kv = [
        _kv("general.architecture", 8, _str(arch)),
        _kv("%s.block_count" % arch, 4, _u32(block_count)),
        _kv("%s.embedding_length" % arch, 4, _u32(N_EMBD)),
        _kv("%s.attention.head_count" % arch, 9,
            _arr_u32([4] * block_count)),
        _kv("%s.attention.head_count_kv" % arch, 9, _arr_u32([2] * block_count)),
        _kv("%s.attention.sliding_window_pattern" % arch, 9, _arr_bool(swa_pattern)),
        _kv("%s.num_loops" % arch, 4, _u32(PASSES)),
        _kv("%s.loop.core_start" % arch, 4, _u32(CORE_START)),
        _kv("%s.loop.core_end" % arch, 4, _u32(CORE_END)),
        _kv("%s.loop.gate_rank" % arch, 4, _u32(GATE_RANK)),
    ]

    header = bytearray(b"GGUF")
    header += struct.pack("<I", 3) + struct.pack("<Q", len(infos)) + struct.pack("<Q", len(kv))
    for entry in kv:
        header += entry
    for info in infos:
        header += info
    header += b"\x00" * (_align(len(header)) - len(header))

    with open(path, "wb") as f:
        f.write(header)
        f.write(data)


# ---------------------------------------------------------------------------
# загрузка модуля патча
# ---------------------------------------------------------------------------

def load_patch_module():
    spec = importlib.util.spec_from_file_location("gguf_loop_meta_patch",
                                                  os.path.join(HERE, "gguf_loop_meta_patch.py"))
    mod = importlib.util.module_from_spec(spec)
    sys.modules["gguf_loop_meta_patch"] = mod
    spec.loader.exec_module(mod)
    return mod


# ---------------------------------------------------------------------------
# тесты
# ---------------------------------------------------------------------------

def _read(mod, path):
    with open(path, "rb") as f:
        import mmap
        mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            return mod.read_header(mm)
        finally:
            mm.close()


def test_unpatched_file_is_loop_expanded(mod, tmp):
    src = os.path.join(tmp, "src.gguf")
    write_fixture(src)
    hdr = _read(mod, src)
    assert hdr.get("general.architecture").value == "inkling2", "arch источника"
    assert int(hdr.get("%s.block_count" % "inkling2").value) == BLOCK_COUNT == 5
    assert mod.physical_count(hdr.names()) == PHYSICAL == 3
    # именно из-за этого падает загрузчик: он ищет blk.3 и blk.4
    assert hdr.tensor("blk.3.attn_q.weight") is None
    assert hdr.get("inkling2.loop.start") is None
    print("  ok: до патча block_count=%d при %d физических слоях" % (BLOCK_COUNT, PHYSICAL))


def test_metadata_only_keeps_blocker(mod, tmp):
    src = os.path.join(tmp, "src.gguf")
    dst = os.path.join(tmp, "meta.gguf")
    write_fixture(src)
    rc = mod.main(["--src", src, "--dst", dst, "--force"])
    assert rc == 0, "main() вернул %d" % rc
    hdr = _read(mod, dst)
    assert hdr.get("general.architecture").value == "inkling2_loop"
    assert int(hdr.get("inkling2_loop.block_count").value) == PHYSICAL
    assert len(hdr.get("inkling2_loop.attention.sliding_window_pattern").value) == PHYSICAL
    assert hdr.get("inkling2_loop.loop.start") is not None
    assert hdr.get("blk.1.loop_msg_query.weight") is None, "без фьюза msg_query быть не должно"
    print("  ok: только метаданные — ключи есть, loop_msg_query нет (блокер загрузчика)")


def test_fused_patch_is_loadable_schema(mod, tmp):
    src = os.path.join(tmp, "src.gguf")
    dst = os.path.join(tmp, "fused.gguf")
    write_fixture(src)
    rc = mod.main(["--src", src, "--dst", dst, "--force", "--fuse-loop-queries"])
    assert rc == 0, "main() вернул %d" % rc
    hdr = _read(mod, dst)

    assert hdr.get("general.architecture").value == "inkling2_loop"
    assert int(hdr.get("inkling2_loop.block_count").value) == PHYSICAL
    for suffix in ("attention.head_count", "attention.head_count_kv",
                   "attention.sliding_window_pattern"):
        values = hdr.get("inkling2_loop." + suffix).value
        assert len(values) == PHYSICAL, "%s: длина %d" % (suffix, len(values))
    assert hdr.get("inkling2_loop.attention.sliding_window_pattern").value == [True, False, True]
    for suffix, want in (("loop.start", CORE_START), ("loop.end", CORE_END),
                         ("loop.count", PASSES), ("loop.gate_rank", GATE_RANK)):
        assert int(hdr.get("inkling2_loop." + suffix).value) == want, suffix
    for suffix in ("num_loops", "loop.core_start", "loop.core_end", "loop.varying"):
        assert hdr.get("inkling2_loop." + suffix) is not None, "нет алиаса " + suffix

    names = hdr.names()
    assert "blk.1.loop_q_ctrl_down.weight" in names
    assert "blk.2.loop_ffn_ctrl_forget.weight" in names
    assert "blk.1.loop_attn_down.weight" not in names
    assert "blk.1.loop_mlp_gain.weight" not in names
    assert "loop_res_query.weight" not in names and "loop_kv_res_query.weight" not in names

    per = MSG_QUERY_BYTES
    msg = [t for t in hdr.tensors if t.name.endswith("loop_msg_query.weight")]
    assert len(msg) == CORE_END - CORE_START, "msg_query: %d" % len(msg)
    assert all(tuple(t.dims) == (N_EMBD, 4) and t.ttype == 0 for t in msg)
    # раскладка смещений: подряд от нуля в порядке описаний (требование gguf_init)
    offsets = [t.offset for t in hdr.tensors]
    assert offsets == sorted(offsets) and offsets[0] == 0, "смещения не упорядочены с нуля"
    assert len(set(offsets)) == len(offsets), "смещения повторяются"
    assert offsets[1] == per, "второй msg_query не вплотную: %d != %d" % (offsets[1], per)

    # фолд строк: msg_query[i] = [q[2i], kv[2i], kv[2i+1], q[2i+1]]
    row = N_EMBD * 4
    region = (CORE_END - CORE_START) // 2 * per
    q = bytes((i * 7) & 0xFF for i in range(region))
    kv = bytes((i * 13 + 5) & 0xFF for i in range(region))
    with open(dst, "rb") as f:
        import mmap
        mm = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
        try:
            arch_hdr = mod.read_header(mm)
            for i in range(CORE_END - CORE_START):
                got = bytes(mm[arch_hdr.data_start + offsets[i]: arch_hdr.data_start + offsets[i] + per])
                want = (q[(2 * i) * row:(2 * i + 1) * row]
                        + kv[(2 * i) * row:(2 * i + 1) * row]
                        + kv[(2 * i + 1) * row:(2 * i + 2) * row]
                        + q[(2 * i + 1) * row:(2 * i + 2) * row])
                assert got == want, "msg_query[%d] не равен фолду" % i
        finally:
            mm.close()
    print("  ok: фьюз — arch/ключи/массивы, 2 msg_query с точным фолдом строк")


def test_broken_visit_array_is_rejected(mod, tmp):
    src = os.path.join(tmp, "broken.gguf")
    pattern = list(SWA_PATTERN)
    pattern[4] = False  # визит 4 ведёт в слой 2 (true), а значение false
    write_fixture(src, swa_pattern=pattern)
    hdr = _read(mod, src)
    try:
        mod.make_plan(hdr, fuse=False)
    except mod.PatchError as e:
        assert "не по визитам" in str(e), "неожиданная причина: %s" % e
    else:
        raise AssertionError("битый массив по визитам принят")
    print("  ok: массив не по визитам отклоняется")


def main():
    mod = load_patch_module()
    tests = [
        test_unpatched_file_is_loop_expanded,
        test_metadata_only_keeps_blocker,
        test_fused_patch_is_loadable_schema,
        test_broken_visit_array_is_rejected,
    ]
    failed = 0
    with tempfile.TemporaryDirectory(prefix="gguf-loop-patch-") as tmp:
        print("=== gguf_loop_meta_patch ===")
        for test in tests:
            try:
                test(mod, tmp)
            except AssertionError as e:
                failed += 1
                import traceback
                print("  FAIL %s: %s" % (test.__name__, e))
                traceback.print_exc()
            except Exception as e:  # noqa: BLE001 — тест должен печатать причину
                failed += 1
                print("  FAIL %s: %s: %s" % (test.__name__, type(e).__name__, e))
    print("=== %s ===" % ("PASS" if failed == 0 else "FAIL (%d)" % failed))
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
