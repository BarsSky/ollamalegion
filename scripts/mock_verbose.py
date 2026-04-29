#!/usr/bin/env python3
import http.server, socketserver, sys

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 11436

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[MOCK] {fmt % args}', flush=True)

    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        if self.path == '/api/version':
            self.wfile.write(b'{"version":"0.5.0-mock"}')
        elif self.path == '/api/tags':
            self.wfile.write(b'{"models":[{"name":"llama3.2","model":"llama3.2","size":2019373184}]}')
        else:
            self.wfile.write(b'{"error":"not found"}')

    def do_POST(self):
        print(f'[MOCK] POST {self.path}', flush=True)
        cl = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(cl)
        print(f'[MOCK] Body: {body.decode()[:200]}', flush=True)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        resp = '{"model":"llama3.2","response":"OK","done":true}'
        self.wfile.write(resp.encode())
        print('[MOCK] Sent response', flush=True)

server = socketserver.ThreadingTCPServer(('127.0.0.1', PORT), Handler)
print(f'[MOCK] Listening on 127.0.0.1:{PORT}', flush=True)
server.serve_forever()