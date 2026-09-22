#!/usr/bin/env node
/*
 * forward_11434.js — прозрачный HTTP-форвардер localhost:11434 → 127.0.0.1:18080.
 *
 * Зачем: Cline CLI с провайдером "ollama" жёстко берёт baseUrl из
 * globalState/providers.json, но при каждом старте перезаписывает providers.json
 * и теряет baseUrl → падает на дефолтный http://localhost:11434. Чтобы
 * протестировать РЕАЛЬНЫЙ клиентский путь Cline (/api/chat через балансер) без
 * правки его внутренностей, поднимаем форвардер на стандартном порту Ollama.
 *
 * Полностью потоковый (SSE/NDJSON не буферизуется).
 *
 * Запуск: node scripts/forward_11434.js [listenPort] [targetPort]
 */

const http = require('http');
const fs = require('fs');
const path = require('path');

const listenPort = parseInt(process.argv[2] || '11434', 10);
const targetPort = parseInt(process.argv[3] || '18080', 10);
const targetHost = '127.0.0.1';
const logFile = process.argv[4] || path.join(__dirname, '..', '_diag', 'forward-capture.log');

// Логируем тела POST-запросов, чтобы видеть ТОЧНЫЙ payload клиента (Cline).
// По умолчанию пишем только ключи и структуру сообщений, чтобы не раздувать файл.
function capture(method, url, body) {
  if (method !== 'POST') return;
  try {
    fs.mkdirSync(path.dirname(logFile), { recursive: true });
    let summary;
    try {
      const j = JSON.parse(body);
      summary = {
        url,
        topLevelKeys: Object.keys(j),
        model: j.model,
        stream: j.stream,
        tool_choice: j.tool_choice,
        options: j.options,
        toolsCount: Array.isArray(j.tools) ? j.tools.length : 0,
        messages: Array.isArray(j.messages)
          ? j.messages.map((m) => ({
              role: m.role,
              keys: Object.keys(m),
              contentPreview: typeof m.content === 'string' ? m.content.slice(0, 120) : m.content,
              tool_name: m.tool_name,
              tool_call_id: m.tool_call_id,
              tool_calls: m.tool_calls
                ? m.tool_calls.map((tc) => ({ id: tc.id, type: tc.type, fnKeys: tc.function ? Object.keys(tc.function) : null, name: tc.function && tc.function.name, argsType: tc.function ? typeof tc.function.arguments : null }))
                : undefined,
            }))
          : null,
        bodyRaw: body.length > 20000 ? body.slice(0, 20000) + '...[truncated]' : body,
      };
    } catch (e) {
      summary = { url, parseError: String(e), bodyRaw: body.slice(0, 4000) };
    }
    fs.appendFileSync(logFile, JSON.stringify(summary, null, 2) + '\n' + '-'.repeat(80) + '\n');
  } catch (e) { /* ignore logging errors */ }
}

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    const body = Buffer.concat(chunks);
    capture(req.method, req.url, body.toString('utf8'));
    const headers = Object.assign({}, req.headers);
    headers['content-length'] = body.length;
    const proxyReq = http.request(
      { host: targetHost, port: targetPort, path: req.url, method: req.method, headers },
      (proxyRes) => {
        res.writeHead(proxyRes.statusCode, proxyRes.headers);
        proxyRes.pipe(res);
      }
    );
    proxyReq.on('error', (err) => {
      if (!res.headersSent) res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'forward error: ' + err.message }));
    });
    proxyReq.end(body);
  });
});

server.listen(listenPort, '0.0.0.0', () => {
  console.log(`[forward] http://localhost:${listenPort} -> http://${targetHost}:${targetPort} (capture: ${logFile})`);
});

