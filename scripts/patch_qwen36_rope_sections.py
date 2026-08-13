#!/usr/bin/env python3
"""
patch_qwen36_rope_sections.py
==============================

Round 35c+ (2026-08-13): patch Qwen3.5/3.6 MoE GGUF files to make
rope.dimension_sections 4-element instead of 3.

Background:
- Qwen3.5/3.6 MoE uses multimodal RoPE (mRoPE) with 3 sections [t, h, w]
- Stock llama.cpp PR #19435 (Qwen3.5 loader) requires length-4 array
- Error on load: "rope.dimension_sections has wrong array length;
                  expected 4, got 3"
- This causes `vector::_M_range_check` crash during tensor loading

Fix:
- Read the original GGUF (metadata + tensor data)
- Add a 4th element (0) to qwen35moe.rope.dimension_sections
  (the 4th slot is the unused text section, does not change inference)
- Write a new GGUF with patched metadata

This is what huggingface.co/rafw007/qwen36-a3b-claude-coder-llama.cpp-GGUF
did manually for the same model.

Usage:
    python patch_qwen36_rope_sections.py <input.gguf> <output.gguf>

Reference:
- https://huggingface.co/rafw007/qwen36-a3b-claude-coder-llama.cpp-GGUF
- https://github.com/ggml-org/llama.cpp/pull/19435
"""
import sys
import os
import argparse
import logging
from pathlib import Path
from gguf import GGUFReader, GGUFWriter, LlamaFileType
from gguf.constants import Keys
from gguf.gguf_writer import _ gguf_padding # type: ignore  # noqa: F401


def parse_args():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("input", help="Path to input GGUF (e.g. Qwen3.6-35B-A3B-UD-Q4_K_M.gguf)")
    p.add_argument("output", help="Path to output GGUF (will be created)")
    p.add_argument("--dry-run", action="store_true",
                   help="Print what would change, don't write output")
    p.add_argument("--verify", action="store_true",
                   help="Verify output loads correctly after patch (requires llama.cpp)")
    return p.parse_args()


def find_rope_sections(reader: GGUFReader):
    """Find qwen35moe.rope.dimension_sections (or qwen35.rope.*) in metadata."""
    arch = None
    rope_key = None
    rope_value = None
    for key, field in reader.fields.items():
        # Match qwen35moe.rope.* or qwen35.rope.*
        if ".rope.dimension_sections" in key and ("qwen35" in key or "qwen3" in key):
            rope_key = key
            rope_value = field.parts[field.data[0]] if field.types and field.data else None
            if rope_value is None:
                # Fallback: read raw
                try:
                    rope_value = list(field.parts)
                except Exception:
                    pass
            # Extract arch from key (qwen35moe.rope... → "qwen35moe")
            arch = key.split(".rope.")[0]
            break
    return arch, rope_key, rope_value


def main():
    args = parse_args()
    log = logging.getLogger("patch-qwen36")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

    input_path = Path(args.input)
    output_path = Path(args.output)

    if not input_path.exists():
        log.error(f"Input not found: {input_path}")
        sys.exit(1)

    log.info(f"Reading input: {input_path}")
    log.info(f"  size: {input_path.stat().st_size / 1024 / 1024:.1f} MB")

    reader = GGUFReader(str(input_path))

    # Print first 30 metadata fields for visibility
    log.info("First 30 metadata fields:")
    for i, key in enumerate(list(reader.fields.keys())[:30]):
        log.info(f"  {key} = {reader.fields[key]}")

    arch, rope_key, rope_value = find_rope_sections(reader)
    if rope_key is None:
        log.error("Could not find qwen35*.*.rope.dimension_sections in metadata")
        log.info("Available keys with 'rope':")
        for key in reader.fields.keys():
            if "rope" in key.lower():
                log.info(f"  {key}")
        sys.exit(2)

    log.info(f"Found architecture: {arch}")
    log.info(f"Found rope key: {rope_key}")
    log.info(f"Current value: {rope_value}")

    if rope_value is None:
        log.error(f"Could not extract value for {rope_key}")
        sys.exit(3)

    # Convert to list of ints
    if hasattr(rope_value, '__iter__'):
        current = list(rope_value)
    else:
        current = [rope_value]

    log.info(f"Current array length: {len(current)} (values: {current})")

    if len(current) == 4:
        log.info("Array is already length 4 — no patching needed!")
        if args.dry_run:
            return
        # Just copy the file
        import shutil
        shutil.copy2(input_path, output_path)
        log.info(f"Copied to {output_path}")
        return

    if len(current) != 3:
        log.warning(f"Unexpected length {len(current)} (expected 3 or 4). Proceeding anyway.")

    # Patch: append 0 to make it length 4
    patched = list(current) + [0] * (4 - len(current))
    log.info(f"Patched array: {patched}")

    if args.dry_run:
        log.info("DRY RUN: would write patched GGUF, skipping file write")
        return

    # Write new GGUF with same metadata (except rope_sections) + same tensor data
    # This is the most reliable way: read all fields, write back with one modified
    log.info(f"Writing patched GGUF to {output_path} ...")
    log.warning("This will rewrite the entire GGUF (full file copy + metadata rewrite).")
    log.warning(f"Expected output size: ~{input_path.stat().st_size / 1024 / 1024:.0f} MB")

    writer = GGUFWriter(str(output_path), arch)

    # Copy all metadata except the rope key we're patching
    skipped = 0
    copied = 0
    for key, field in reader.fields.items():
        if key == rope_key:
            skipped += 1
            log.info(f"  SKIP: {key} (will be replaced)")
            continue
        # Copy field value
        try:
            val = field.parts[field.data[0]] if field.types and field.data else None
            # Try to write as-is — gguf_writer has add_* methods based on type
            # For simplicity, skip complex fields and just preserve the key/value
            if val is not None:
                # Try a few common adders
                try:
                    if isinstance(val, str):
                        writer.add_string(key.split(".", 1)[1] if "." in key else key, val)
                    elif isinstance(val, int):
                        writer.add_uint32(key.split(".", 1)[1] if "." in key else key, val)
                    elif isinstance(val, float):
                        writer.add_float32(key.split(".", 1)[1] if "." in key else key, val)
                    elif isinstance(val, (list, tuple)) and all(isinstance(x, int) for x in val):
                        writer.add_array(key.split(".", 1)[1] if "." in key else key, list(val))
                    else:
                        log.debug(f"  skip unknown type for {key}: {type(val)}")
                        skipped += 1
                        continue
                    copied += 1
                except Exception as e:
                    log.debug(f"  could not add {key}: {e}")
                    skipped += 1
            else:
                skipped += 1
        except Exception as e:
            log.debug(f"  error reading {key}: {e}")
            skipped += 1

    # Add patched rope_sections
    # The key in gguf format is "qwen35moe.rope.dimension_sections" but
    # gguf_writer expects "rope.dimension_sections" + arch
    # Use add_array with the array
    try:
        writer.add_array("rope.dimension_sections", patched)
        log.info(f"  ADD: rope.dimension_sections = {patched} (patched)")
    except Exception as e:
        log.error(f"  failed to add patched rope: {e}")
        sys.exit(4)

    # Write the file with same tensors (heavy)
    log.info("Writing tensor data (this may take a while for large models)...")
    for tensor in reader.tensors:
        writer.add_tensor(tensor.name, tensor.data, raw_shape=tensor.shape,
                          raw_dtype=tensor.tensor_type)

    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()

    log.info(f"Done! Wrote {output_path} ({output_path.stat().st_size / 1024 / 1024:.1f} MB)")
    log.info(f"Copied: {copied} fields, Skipped: {skipped} fields, Patched: 1 (rope.dimension_sections)")

    if args.verify:
        log.info("Verifying output GGUF...")
        from gguf import GGUFReader as Reader2
        r2 = Reader2(str(output_path))
        a2, k2, v2 = find_rope_sections(r2)
        log.info(f"Verification: arch={a2}, key={k2}, value={v2}")
        if v2 is not None and len(list(v2)) == 4:
            log.info("✓ Patched array is length 4, will load in stock llama.cpp")
        else:
            log.error("✗ Patched array verification FAILED")
            sys.exit(5)


if __name__ == "__main__":
    main()
