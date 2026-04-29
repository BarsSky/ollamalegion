#!/usr/bin/env python3
"""
Mock Ollama сервер для тестирования балансировщика.
Эмулирует /api/generate, /api/tags, /api/ps, /api/version
"""

import http.server
import json
import sys
import time
import random


class MockOllamaHandler(http.server.BaseHTTPRequestHandler):
    request_count = 0
    server_name = "mock-ollama"

    def log_message(self, format, *args):
        print(f"[{self.server_name}] {self.address_string()} - {format % args}")

    def do_GET(self):
        if self.path == "/api/tags":
            self._send_json({
                "models": [
                    {
                        "name": "llama3.2",
                        "model": "llama3.2",
                        "size": 2019373184,
                        "digest": "mock-digest-1",
                        "modified_at": "2026-04-29T00:00:00Z",
                        "details": {
                            "format": "gguf",
                            "family": "llama",
                            "families": ["llama"],
                            "parameter_size": "3.2B",
                            "quantization_level": "Q4_K_M"
                        }
                    }
                ]
            })
        elif self.path == "/api/ps":
            self._send_json({"models": []})
        elif self.path == "/api/version":
            self._send_json({"version": "0.5.0-mock"})
        else:
            self.send_error(404)

    def do_POST(self):
        content_len = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_len).decode("utf-8") if content_len else "{}"
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            req = {}

        if self.path == "/api/generate":
            MockOllamaHandler.request_count += 1
            model = req.get("model", "unknown")
            prompt = req.get("prompt", "")
            # Эмулируем небольшую задержку обработки (50-200 мс)
            delay = random.uniform(0.05, 0.2)
            time.sleep(delay)

            self._send_json({
                "model": model,
                "created_at": "2026-04-29T00:00:00Z",
                "response": f"Mock response for: {prompt[:30]}...",
                "done": True,
                "done_reason": "stop",
                "context": [],
                "total_duration": int(delay * 1_000_000_000),
                "load_duration": 0,
                "prompt_eval_count": len(prompt.split()),
                "prompt_eval_duration": 0,
                "eval_count": 10,
                "eval_duration": int(delay * 1_000_000_000),
            })
        elif self.path == "/api/chat":
            MockOllamaHandler.request_count += 1
            self._send_json({
                "model": req.get("model", "unknown"),
                "message": {"role": "assistant", "content": "Mock chat response"},
                "done": True,
            })
        else:
            self.send_error(404)

    def _send_json(self, data):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode("utf-8"))


def run(port=11434, name="mock-ollama"):
    MockOllamaHandler.server_name = name
    server = http.server.HTTPServer(("0.0.0.0", port), MockOllamaHandler)
    print(f"[{name}] Mock Ollama listening on 0.0.0.0:{port}")
    print(f"[{name}] Endpoints: /api/generate, /api/tags, /api/ps, /api/version")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print(f"\n[{name}] Shutting down. Total requests handled: {MockOllamaHandler.request_count}")


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 11434
    name = sys.argv[2] if len(sys.argv) > 2 else "mock-ollama"
    run(port, name)