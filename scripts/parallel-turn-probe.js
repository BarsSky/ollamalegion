#!/usr/bin/env node
/*
 * parallel-turn-probe.js — N одновременных запросов к одной модели.
 *
 * ЗАЧЕМ. Проверка поведения «модель ещё грузится»: первый запрос запускает загрузку,
 * остальные приходят в окно загрузки. До R113 такой запрос отвечал за 0.15 с
 * чанком done:true (клиент видел завершённый ход без ответа); после — ЖДЁТ
 * готовности и отвечает нормально.
 *
 * ЗАПУСК:
 *   node scripts/parallel-turn-probe.js [--url http://127.0.0.1:18080/api/chat]
 *        [--payload debug/cline-real-request.json] [--count 3] [--timeout 900]
 *
 * Печатает по каждому запросу: HTTP, время до ПЕРВОГО байта, общее время,
 * done_reason, наличие tool_calls и форму аргументов. Код возврата 1, если
 * какой-то запрос получил done:true без content и без tool_calls (то есть
 * клиенту нечего показать — «COMPLETE» без ответа).
 */

'use strict';

const http = require('http');
const https = require('https');
const fs = require('fs');

function arg(name, def) {
    const i = process.argv.indexOf('--' + name);
    return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
}

const url = arg('url', 'http://127.0.0.1:18080/api/chat');
const payloadFile = arg('payload', 'debug/cline-real-request.json');
const count = parseInt(arg('count', '3'), 10);
const timeoutSec = parseInt(arg('timeout', '900'), 10);
const token = arg('token', process.env.LB_API_TOKEN || process.env.CPPWORKER_API_TOKEN || '');

const base = JSON.parse(fs.readFileSync(payloadFile, 'utf8'));
const body = Buffer.from(JSON.stringify(Object.assign({}, base, { stream: true })), 'utf8');
const u = new URL(url);
const lib = u.protocol === 'https:' ? https : http;

function oneRequest(id) {
    return new Promise((resolve) => {
        const started = Date.now();
        let firstByteAt = null;
        let content = '';
        const toolCalls = [];
        let doneReason = null;
        let done = false;
        let chunks = 0;
        const errors = [];

        const req = lib.request({
            hostname: u.hostname, port: u.port, path: u.pathname + u.search, method: 'POST',
            headers: Object.assign({ 'Content-Type': 'application/json', 'Content-Length': body.length },
                token ? { 'X-API-Token': token } : {}),
        }, (res) => {
            let buf = '';
            res.setEncoding('utf8');
            res.on('data', (chunk) => {
                if (firstByteAt === null) firstByteAt = Date.now();
                buf += chunk;
                let i;
                while ((i = buf.indexOf('\n')) >= 0) {
                    const line = buf.slice(0, i);
                    buf = buf.slice(i + 1);
                    consume(line);
                }
            });
            res.on('end', () => {
                if (buf.trim()) consume(buf);
                resolve({ id, status: res.statusCode, firstByteAt, elapsedMs: Date.now() - started, content, toolCalls, doneReason, done, chunks, errors });
            });
        });
        req.setTimeout(timeoutSec * 1000, () => req.destroy(new Error('timeout')));
        req.on('error', (e) => {
            errors.push(String(e.message));
            resolve({ id, status: 0, firstByteAt, elapsedMs: Date.now() - started, content, toolCalls, doneReason, done, chunks, errors });
        });
        req.end(body);

        function consume(line) {
            if (!line.trim()) return;
            let o;
            try { o = JSON.parse(line); } catch (e) { return; }
            chunks++;
            if (o.keepalive) return;
            if (o.error) errors.push(String(o.error));
            const msg = o.message || {};
            if (typeof msg.content === 'string') content += msg.content;
            if (Array.isArray(msg.tool_calls)) {
                for (const tc of msg.tool_calls) {
                    const fn = tc.function || {};
                    toolCalls.push({ name: fn.name, shape: Array.isArray(fn.arguments) ? 'array' : typeof fn.arguments });
                }
            }
            if (o.done === true) { done = true; doneReason = o.done_reason || null; }
        }
    });
}

(async () => {
    console.log(`запускаю ${count} одновременных запросов: ${url} (model=${base.model})`);
    const results = await Promise.all(Array.from({ length: count }, (_, i) => oneRequest(i + 1)));
    const problems = [];
    console.log('\n# | HTTP | 1-й байт | всего   | чанков | content | tool_calls | done_reason');
    for (const r of results) {
        console.log(`${r.id} | ${String(r.status).padEnd(4)} | ${String(r.firstByteAt ? ((r.firstByteAt - (r.firstByteAt - r.elapsedMs)) / 1000) + 's' : '—').padEnd(8)} | ${(r.elapsedMs / 1000).toFixed(2)}s`.padEnd(40) +
            ` | ${String(r.chunks).padEnd(6)} | ${String(r.content.length).padEnd(7)} | ${(r.toolCalls.map((t) => t.name + '(' + t.shape + ')').join(',') || '—')} | ${r.doneReason}`);
        if (r.errors.length) console.log(`   ошибки: ${r.errors.join(' | ')}`);
        if (!r.done) problems.push(`#${r.id}: стрим не закрылся`);
        if (r.done && r.content.trim() === '' && r.toolCalls.length === 0) {
            problems.push(`#${r.id}: done:true без content и tool_calls (клиенту нечего показать) — ${r.errors[0] || 'без ошибки'}`);
        }
        for (const t of r.toolCalls) {
            if (t.shape !== 'object') problems.push(`#${r.id}: arguments ${t.name} — ${t.shape}, ожидался object`);
        }
    }
    if (problems.length) {
        console.log('\n❌ ПРОБЛЕМЫ:');
        problems.forEach((p) => console.log('  - ' + p));
        process.exit(1);
    }
    console.log('\n✅ все запросы получили ответ (никто не завершился мгновенно без данных)');
})().catch((e) => {
    console.error('ERR ' + (e && e.message ? e.message : e));
    process.exit(1);
});
