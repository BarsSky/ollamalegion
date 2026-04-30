#!/usr/bin/env python3
"""
Расширенный Mock Ollama сервер для нагрузочного тестирования балансера.
Поддерживает: non-streaming, SSE streaming, симуляцию GPU-hang.

Переменные окружения:
  BACKEND_NAME  — имя бэкенда (для логов)
  BACKEND_MODELS — JSON-список моделей, например '["llama3.2:3b","qwen2.5:14b"]'
  HUNG_MODE     — если "true", ВСЕ streaming-запросы зависают (для S3)
"""

import http.server
import json
import os
import sys
import time
import random
import threading

BACKEND_NAME = os.environ.get("BACKEND_NAME", "mock-backend")
RAW_MODELS = os.environ.get("BACKEND_MODELS", '["llama3.2:3b"]')
try:
    MODELS = json.loads(RAW_MODELS)
except json.JSONDecodeError:
    MODELS = ["llama3.2:3b"]
HUNG_MODE = os.environ.get("HUNG_MODE", "false").lower() == "true"
_hung_lock = threading.Lock()

request_count = 0
lock = threading.Lock()


class MockHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[{BACKEND_NAME}] {self.address_string()} - {fmt % args}", flush=True)

    def do_GET(self):
        if self.path == "/admin/hung":
            self._send_json({"hung_mode": HUNG_MODE})
        elif self.path == "/api/tags":
            models_list = [{"name": m, "model": m, "size": 2019373184,
                            "digest": f"mock-digest-{m}", "modified_at": "2026-04-29T00:00:00Z"}
                           for m in MODELS]
            self._send_json({"models": models_list})
        elif self.path == "/api/ps":
            self._send_json({"models": [{"name": m} for m in MODELS]})
        elif self.path == "/api/version":
            self._send_json({"version": "0.5.0-mock"})
        else:
            self.send_error(404)

    def do_POST(self):
        global HUNG_MODE, request_count
        if self.path == "/admin/hung":
            cl = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(cl).decode("utf-8") if cl else "{}"
            try:
                req = json.loads(body)
            except json.JSONDecodeError:
                req = {}
            enable = req.get("enable", False)
            with _hung_lock:
                HUNG_MODE = enable
            print(f"[{BACKEND_NAME}] *** HUNG_MODE set to {HUNG_MODE} ***", flush=True)
            self._send_json({"ok": True, "hung_mode": HUNG_MODE})
            return

        cl = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(cl).decode("utf-8") if cl else "{}"
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            req = {}

        model = req.get("model", "unknown")
        is_stream = req.get("stream", False)
        is_hung = req.get("hung", False)  # Специальный параметр для симуляции зависания

        if self.path in ("/api/generate", "/api/chat"):
            with lock:
                request_count += 1
                cnt = request_count

            if is_stream:
                self._handle_streaming(model, req, is_hung, cnt)
            else:
                self._handle_non_streaming(model, req, cnt)
        else:
            self.send_error(404)

    def _handle_non_streaming(self, model, req, cnt):
        delay = random.uniform(0.02, 0.08)
        time.sleep(delay)
        resp = {
            "model": model,
            "created_at": "2026-04-29T00:00:00Z",
            "response": f"[{BACKEND_NAME}] Mock response #{cnt} for model {model}",
            "done": True,
            "done_reason": "stop",
            "total_duration": int(delay * 1_000_000_000),
            "eval_count": 10,
            "eval_duration": int(delay * 500_000_000),
        }
        if self.path == "/api/chat":
            resp["message"] = {"role": "assistant", "content": resp["response"]}
        self._send_json(resp)

    def _handle_streaming(self, model, req, is_hung, cnt):
        # Если HUNG_MODE глобален или запрос специально просит hung — зависаем
        should_hang = HUNG_MODE or is_hung
        if should_hang:
            print(f"[{BACKEND_NAME}] *** GPU HANG SIMULATION: streaming request #{cnt} will hang ***", flush=True)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.send_header("X-Accel-Buffering", "no")
        self.end_headers()

        try:
            if should_hang:
                # Отправляем пустой chunk (чтобы first-byte прошёл) и зависаем на 60 секунд
                self.wfile.write(b": ping\n\n")
                self.wfile.flush()
                time.sleep(60)  # Висим — клиентский first-byte timeout должен сработать
                return
            else:
                # Нормальный streaming: 5 токенов с задержкой
                for i in range(5):
                    token = f"[{BACKEND_NAME}] token {i+1} for {model} (req #{cnt})"
                    chunk = json.dumps({
                        "model": model,
                        "created_at": "2026-04-29T00:00:00Z",
                        "response": token + " ",
                        "done": False,
                    })
                    self.wfile.write(f"data: {chunk}\n\n".encode())
                    self.wfile.flush()
                    time.sleep(random.uniform(0.05, 0.15))

                # Финальный done
                final = json.dumps({
                    "model": model,
                    "created_at": "2026-04-29T00:00:00Z",
                    "response": "",
                    "done": True,
                    "done_reason": "stop",
                    "total_duration": int(500_000_000),
                    "eval_count": 5,
                    "eval_duration": int(500_000_000),
                })
                self.wfile.write(f"data: {final}\n\n".encode())
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            print(f"[{BACKEND_NAME}] Client disconnected during streaming", flush=True)

    def _send_json(self, data):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 11434
    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), MockHandler)
    print(f"[{BACKEND_NAME}] Listening on 0.0.0.0:{port}, models={MODELS}, hung_mode={HUNG_MODE}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print(f"[{BACKEND_NAME}] Shutdown. Total requests: {request_count}", flush=True)