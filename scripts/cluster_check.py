#!/usr/bin/env python3
"""
Список бэкендов из API балансировщика с типом и статусом.

Пример:
    python scripts/cluster_check.py
    python scripts/cluster_check.py --admin http://localhost:18081
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request


def main() -> int:
    parser = argparse.ArgumentParser(description="Список бэкендов OllamaLegion")
    parser.add_argument(
        "--admin",
        default=os.environ.get("OLLAMALEGION_ADMIN", "http://localhost:18081"),
        help="URL Management API балансировщика (env: OLLAMALEGION_ADMIN)",
    )
    args = parser.parse_args()

    url = f"{args.admin.rstrip('/')}/api/v1/backends"
    try:
        with urllib.request.urlopen(url, timeout=10) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        print(f"HTTP {e.code}: {e.read().decode('utf-8', errors='replace')[:300]}", file=sys.stderr)
        return 1
    except Exception as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1

    backends = data.get("backends", [])
    total = data.get("total", len(backends))
    print(f"Total: {total}")
    for b in backends:
        print(
            f"  {b.get('id','?'):20s} "
            f"type={b.get('type', b.get('backendType','?'))} "
            f"status={b.get('status','?')} "
            f"cppWorkerPort={b.get('cppWorkerPort', 0)}"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())