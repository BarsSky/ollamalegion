#!/usr/bin/env python3
"""oracle.py — «оракул движка»: сверка пред-проверки HF-заголовков с настоящим
sd-server, без скачивания гигабайтов весов.

ЗАЧЕМ ЭТОТ ИНСТРУМЕНТ. Пред-проверка (`internal/cppbackend/hf_probe.go`,
`GET /api/hf/probe`) утверждает, что движок pinned-версии узнаёт семейство файла
по именам тензоров. Проверить такое утверждение можно только на самом движке, а
качать 4 ГБ ради проверки одного правила — не вариант. Поэтому:

  1. `dump`   — читает Range-запросом первые 512 КБ файла на HuggingFace и
                печатает architecture, счётчик тензоров, префиксы и ЯКОРЯ из
                ModelLoader::get_sd_version (src/model_loader.cpp);
  2. `synth`  — собирает синтетический GGUF с ТЕМИ ЖЕ именами тензоров, но
                крошечными данными (1 элемент F32 на тензор) — файл на десятки
                килобайт вместо гигабайтов;
  3. `diff`   — сравнивает наборы имён тензоров двух файлов (именно так нашлась
                разница fused `img_mlp.gate_up` против split
                `img_mlp.gate_layer` + `img_mlp.proj`).

КАК ПРОВЕРИТЬ (порядок из docs/image-generation.md §8.2):

    python tools/sdcpp-probe-oracle/oracle.py dump leejet/Qwen-Image-2.1-GGUF::qwen_image_2.1-Q4_0.gguf
    python tools/sdcpp-probe-oracle/oracle.py synth /tmp/fake.gguf <repo>::<file> --arch qwen_image21
    docker cp /tmp/fake.gguf ol-stack-imageworker:/tmp/fake.gguf
    # DiT-файл: движок должен напечатать «Version: ...»
    docker exec ol-stack-imageworker /app/sd-server/sd-server --diffusion-model /tmp/fake.gguf --listen-port 18991
    # тот же файл как all-in-one: ожидаем «get sd version from file failed»
    docker exec ol-stack-imageworker /app/sd-server/sd-server --model /tmp/fake.gguf --listen-port 18992

Результат такой сверки для pinned master-929-3f8527a: и сборка leejet (fused
gate_up), и экспорт под ComfyUI (split gate_layer+proj, arch qwen_image21) дают
«Version: Qwen Image 2.1» через --diffusion-model и «get sd version from file
failed» через --model. Отсюда правило: причина ошибки — НЕ «чужой формат», а
несовпадение семейства профиля с файлом.
"""
from __future__ import annotations

import argparse
import json
import struct
import sys
import urllib.request

RANGE_BYTES = 512 * 1024
USER_AGENT = "ollamalegion-probe-oracle"

# Якоря из ModelLoader::get_sd_version (src/model_loader.cpp, pinned master-929-3f8527a).
# Держим в синхроне с internal/cppbackend/hf_probe.go → EngineAnchors.
ANCHORS = [
    ("qwen_image", "Qwen Image 2.1", "txt_in.text_norm.weight"),
    ("qwen_image", "Qwen Image", "transformer_blocks.0.img_mod.1.weight"),
    ("flux2", "Flux 2", "double_stream_modulation_img.lin.weight"),
    ("flux", "Flux", "double_blocks."),
    ("flux", "Flux", "single_transformer_blocks."),
    ("sd3", "SD3.x", "joint_blocks."),
    ("chroma", "Chroma", "nerf_final_layer_conv."),
    ("z_image", "Z-Image", "cap_embedder.0.weight"),
    ("other", "Wan", "blocks.0.cross_attn.norm_k.weight"),
    ("other", "PixArt", "t_block.1.weight"),
    ("other", "LLaDA Image", "sigvq_embedder.1.weight"),
    ("other", "Hunyuan Video", "txt_in.individual_token_refiner.blocks.0.adaLN_modulation.1.weight"),
]


def fetch_head(repo: str, filename: str, revision: str = "main") -> bytes:
    url = "https://huggingface.co/%s/resolve/%s/%s" % (repo, revision, filename)
    req = urllib.request.Request(url, headers={"Range": "bytes=0-%d" % (RANGE_BYTES - 1), "User-Agent": USER_AGENT})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.read(RANGE_BYTES)


def _read_string(buf: bytes, off: int) -> tuple[str, int]:
    (n,) = struct.unpack_from("<Q", buf, off)
    off += 8
    return buf[off:off + n].decode("utf-8", "replace"), off + n


_ARRAY_ELEMENT_SIZE = {0: 1, 1: 1, 7: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 10: 8, 11: 8, 12: 8}


def parse_gguf(buf: bytes):
    """Возвращает (kv-словарь строк, [(имя, dims), ...])."""
    off = 8
    tensor_count, kv_count = struct.unpack_from("<QQ", buf, off)
    off += 16
    kvs: dict[str, str] = {}
    for _ in range(kv_count):
        key, off = _read_string(buf, off)
        (vt,) = struct.unpack_from("<I", buf, off)
        off += 4
        if vt == 8:
            val, off = _read_string(buf, off)
            kvs[key] = val
        elif vt in (0, 1, 7):
            off += 1
        elif vt in (2, 3):
            off += 2
        elif vt in (4, 5, 6):
            off += 4
        elif vt in (10, 11, 12):
            off += 8
        elif vt == 9:
            (et,) = struct.unpack_from("<I", buf, off)
            (n,) = struct.unpack_from("<Q", buf, off + 4)
            off += 12
            if et == 8:
                for _ in range(n):
                    _, off = _read_string(buf, off)
            else:
                off += n * _ARRAY_ELEMENT_SIZE[et]
        else:
            raise ValueError("неизвестный тип KV %d у ключа %s" % (vt, key))
    tensors = []
    for _ in range(tensor_count):
        name, off = _read_string(buf, off)
        (nd,) = struct.unpack_from("<I", buf, off)
        dims = struct.unpack_from("<%dQ" % nd, buf, off + 4)
        tensors.append((name, dims))
        off += 4 + nd * 8 + 4 + 8
    return kvs, tensors


def build_gguf(tensor_names: list[str], arch: str = "") -> bytes:
    def gstr(s: str) -> bytes:
        raw = s.encode("utf-8")
        return struct.pack("<Q", len(raw)) + raw

    kvs = b""
    kv_count = 0
    if arch:
        kvs += gstr("general.architecture") + struct.pack("<I", 8) + gstr(arch)
        kv_count += 1
    kvs += gstr("general.name") + struct.pack("<I", 8) + gstr("probe-oracle-synthetic")
    kv_count += 1

    infos = b""
    offset = 0
    for name in tensor_names:
        infos += gstr(name) + struct.pack("<I", 1) + struct.pack("<Q", 1) + struct.pack("<I", 0) + struct.pack("<Q", offset)
        offset += 32
    header = b"GGUF" + struct.pack("<I", 3) + struct.pack("<Q", len(tensor_names)) + struct.pack("<Q", kv_count) + kvs + infos
    header += b"\x00" * ((-len(header)) % 32)
    return header + b"\x00" * (len(tensor_names) * 32)


def split_spec(spec: str) -> tuple[str, str]:
    repo, sep, filename = spec.partition("::")
    if not sep or not repo or not filename:
        raise SystemExit("укажите файл как <repo>::<filename>, например leejet/FLUX.1-schnell-gguf::flux1-schnell-q4_0.gguf")
    return repo, filename


def cmd_dump(spec: str) -> int:
    repo, filename = split_spec(spec)
    head = fetch_head(repo, filename)
    if head[:4] != b"GGUF":
        print("не GGUF: magic=%r" % head[:8])
        return 2
    kvs, tensors = parse_gguf(head)
    names = [n for n, _ in tensors]
    print("%s / %s" % (repo, filename))
    print("  architecture=%r, тензоров=%d" % (kvs.get("general.architecture", "<нет>"), len(names)))
    print("  ключи GGUF: %s" % ", ".join(sorted(kvs)[:12]))
    prefixes = []
    for n in names:
        p = n.split(".")[0]
        if p not in prefixes:
            prefixes.append(p)
    print("  префиксы: %s" % ", ".join(prefixes[:20]))
    hit = False
    for family, version, pat in ANCHORS:
        matches = [n for n in names if pat in n]
        if matches:
            hit = True
            print("  ЯКОРЬ %-24s → family=%-11s version=%-16s (%s)" % (pat, family, version, matches[0]))
    if not hit:
        print("  якорей нет: движок скажет «get sd version from file failed» (это VAE/text encoder/LoRA или незнакомое семейство)")
    return 0


def cmd_synth(out: str, spec: str, arch: str) -> int:
    repo, filename = split_spec(spec)
    _, tensors = parse_gguf(fetch_head(repo, filename))
    blob = build_gguf([n for n, _ in tensors], arch)
    with open(out, "wb") as fh:
        fh.write(blob)
    print("написано %s: тензоров=%d, arch=%r, размер=%d байт" % (out, len(tensors), arch or "<нет>", len(blob)))
    return 0


def cmd_diff(spec_a: str, spec_b: str) -> int:
    repo_a, file_a = split_spec(spec_a)
    repo_b, file_b = split_spec(spec_b)
    _, ta = parse_gguf(fetch_head(repo_a, file_a))
    _, tb = parse_gguf(fetch_head(repo_b, file_b))
    set_a = {n for n, _ in ta}
    set_b = {n for n, _ in tb}
    print("A: %s/%s — тензоров=%d" % (repo_a, file_a, len(set_a)))
    print("B: %s/%s — тензоров=%d" % (repo_b, file_b, len(set_b)))
    print("\nтолько в A (%d): %s" % (len(set_a - set_b), "; ".join(sorted(set_a - set_b)[:30])))
    print("\nтолько в B (%d): %s" % (len(set_b - set_a), "; ".join(sorted(set_b - set_a)[:30])))
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Оракул движка sd.cpp: сверка пред-проверки HF-заголовков")
    sub = parser.add_subparsers(dest="cmd", required=True)
    p_dump = sub.add_parser("dump", help="показать заголовок файла с HF и сработавшие якоря")
    p_dump.add_argument("spec", help="<repo>::<filename>")
    p_synth = sub.add_parser("synth", help="собрать синтетический GGUF с теми же именами тензоров")
    p_synth.add_argument("out", help="куда записать .gguf")
    p_synth.add_argument("spec", help="<repo>::<filename>")
    p_synth.add_argument("--arch", default="", help='значение general.architecture (например qwen_image21)')
    p_diff = sub.add_parser("diff", help="сравнить наборы имён тензоров двух файлов")
    p_diff.add_argument("spec_a")
    p_diff.add_argument("spec_b")
    args = parser.parse_args(argv)
    if args.cmd == "dump":
        return cmd_dump(args.spec)
    if args.cmd == "synth":
        return cmd_synth(args.out, args.spec, args.arch)
    if args.cmd == "diff":
        return cmd_diff(args.spec_a, args.spec_b)
    parser.error("неизвестная команда")
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
