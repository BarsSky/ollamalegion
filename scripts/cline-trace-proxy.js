// cline-trace-proxy.js — диагностический прокси между клиентом (Cline) и
// балансером.
//
// ЗАЧЕМ. Когда Cline показывает «COMPLETE»/обрыв/сжатие, нужно видеть, ЧТО
// именно клиент прислал и ЧТО получил в ответ, а не догадываться по логам
// балансера. Прокси прозрачно пересылает запросы и пишет в NDJSON:
//   {ts, method, url, model, request_body}
//   {ts, status, response_json | response_stream_file, stream_bytes, chunks}
// Для стриминговых ответов (SSE/NDJSON) сырой поток сохраняется отдельным
// файлом рядом, чтобы можно было разобрать кадры вручную.
//
// ЗАПУСК:
//   node scripts/cline-trace-proxy.js --port 18090 --target http://127.0.0.1:18080 --out C:\temp\cline-trace.ndjson
// затем в Cline указать base URL http://127.0.0.1:18090 (Ollama/OpenAI — любой
// путь пересылается как есть).
//
// БЕЗ ЗАВИСИМОСТЕЙ (http/https из Node), ничего не меняет в ответе.
'use strict';

const http = require('http');
const https = require('https');
const fs = require('fs');
const path = require('path');
const { URL } = require('url');

function arg(name, def) {
    const i = process.argv.indexOf('--' + name);
    return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
}

const PORT = parseInt(arg('port', '18090'), 10);
const TARGET = new URL(arg('target', 'http://127.0.0.1:18080'));
const OUT = path.resolve(arg('out', path.join(process.cwd(), 'cline-trace.ndjson')));
const MAX_BODY = parseInt(arg('max-body', '2000000'), 10);
const STREAM_DIR = OUT.replace(/\.ndjson$/, '') + '.streams';
fs.mkdirSync(path.dirname(OUT), { recursive: true });
fs.mkdirSync(STREAM_DIR, { recursive: true });

const logStream = fs.createWriteStream(OUT, { flags: 'a' });
let seq = 0;

function redact(headers) {
    const out = {};
    for (const [k, v] of Object.entries(headers || {})) {
        if (/authorization|api[-_]?key|token|cookie/i.test(k)) out[k] = '<redacted>';
        else out[k] = v;
    }
    return out;
}

function logEvent(obj) {
    logStream.write(JSON.stringify(obj) + '\n');
}

function isStreamResponse(headers) {
    const ct = String(headers['content-type'] || '');
    return ct.includes('text/event-stream') || ct.includes('application/x-ndjson');
}

function proxyReq(clientReq, clientRes) {
    const id = ++seq;
    const chunks = [];
    clientReq.on('data', (c) => {
        chunks.push(c);
        if (Buffer.concat(chunks).length > MAX_BODY) clientReq.destroy();
    });
    clientReq.on('end', () => {
        const bodyBuf = Buffer.concat(chunks);
        const bodyText = bodyBuf.toString('utf8');
        let model = '';
        try { model = JSON.parse(bodyText).model || ''; } catch (e) { /* not JSON */ }

        logEvent({
            ts: new Date().toISOString(), id, event: 'request',
            method: clientReq.method, url: clientReq.url, model,
            headers: redact(clientReq.headers),
            request_body: bodyText,
        });

        const upstream = (TARGET.protocol === 'https:' ? https : http).request({
            protocol: TARGET.protocol,
            hostname: TARGET.hostname,
            port: TARGET.port || (TARGET.protocol === 'https:' ? 443 : 80),
            method: clientReq.method,
            path: clientReq.url,
            headers: { ...clientReq.headers, host: TARGET.host },
        }, (upRes) => {
            const stream = isStreamResponse(upRes.headers);
            if (!stream) {
                const resChunks = [];
                upRes.on('data', (c) => resChunks.push(c));
                upRes.on('end', () => {
                    const resBuf = Buffer.concat(resChunks);
                    logEvent({
                        ts: new Date().toISOString(), id, event: 'response',
                        status: upRes.statusCode, headers: redact(upRes.headers),
                        response_body: resBuf.toString('utf8'),
                    });
                    clientRes.writeHead(upRes.statusCode, upRes.headers);
                    clientRes.end(resBuf);
                });
                return;
            }

            // Стрим: отдаём клиенту сразу, параллельно пишем сырой поток.
            const streamFile = path.join(STREAM_DIR, `${id}.ndjson`);
            const streamOut = fs.createWriteStream(streamFile);
            let bytes = 0, chunksCount = 0, first = '', last = '';
            clientRes.writeHead(upRes.statusCode, upRes.headers);
            upRes.on('data', (c) => {
                bytes += c.length;
                chunksCount++;
                if (!first) first = c.toString('utf8').slice(0, 400);
                last = c.toString('utf8').slice(-400);
                streamOut.write(c);
                clientRes.write(c);
            });
            upRes.on('end', () => {
                streamOut.end();
                logEvent({
                    ts: new Date().toISOString(), id, event: 'response_stream',
                    status: upRes.statusCode, headers: redact(upRes.headers),
                    stream_file: streamFile, stream_bytes: bytes, stream_chunks: chunksCount,
                    first_chunk: first, last_chunk: last,
                });
                clientRes.end();
            });
        });
        upstream.on('error', (e) => {
            logEvent({ ts: new Date().toISOString(), id, event: 'proxy_error', error: String(e) });
            try { clientRes.writeHead(502, { 'content-type': 'application/json' }); } catch (err) { /* headers sent */ }
            clientRes.end(JSON.stringify({ error: 'trace proxy upstream error', detail: String(e) }));
        });
        upstream.end(bodyBuf);
    });
}

const server = http.createServer(proxyReq);
server.listen(PORT, '127.0.0.1', () => {
    console.log(`cline-trace-proxy: http://127.0.0.1:${PORT} -> ${TARGET.href}; log=${OUT}, streams=${STREAM_DIR}`);
});
