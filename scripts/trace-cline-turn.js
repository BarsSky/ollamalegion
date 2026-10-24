#!/usr/bin/env node
/*
 * trace-cline-turn.js — ЯВНОЕ ОТСЛЕЖИВАНИЕ ОДНОГО ХОДА клиента Cline.
 *
 * ЗАЧЕМ. Спор «инструмент в теле ответа / COMPLETE без стрима» нельзя решать по
 * логам сервера: они показывают внутренние решения, а не то, что реально уехало
 * клиенту. Скрипт отправляет запрос формы Cline (payload-файл, stream=true),
 * сохраняет КАЖДУЮ сырую строку ответа и печатает разбор по существу:
 *
 *   - стримил ли сервер content/thinking по ходу генерации или отдал один чанк в конце;
 *   - попали ли маркеры размышлений/сырой JSON вызова в content (утечка в тело ответа);
 *   - пришёл ли вызов инструмента в message.tool_calls (объект аргументов);
 *   - был ли done:true ДО первого содержательного чанка (клиент покажет «COMPLETE»);
 *   - итоговое собранное сообщение ассистента.
 *
 * ЗАПУСК
 *   node scripts/trace-cline-turn.js --url http://127.0.0.1:18080/api/chat \
 *        --payload debug/probe_tools.json [--model gemma-4-E4B-it-Q4_K_M] [--timeout 900]
 *
 * Результат: debug/trace-turn-<ts>.ndjson (сырые строки) + отчёт в stdout.
 * Код возврата 1 — если найдена утечка в content, done без содержимого или
 * отсутствие вызова инструмента при 18 tools.
 */

'use strict';

const http = require('http');
const https = require('https');
const fs = require('fs');
const path = require('path');

function arg(name, def) {
    const i = process.argv.indexOf('--' + name);
    return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
}

const url = arg('url', 'http://127.0.0.1:18080/api/chat');
const payloadFile = arg('payload', 'debug/probe_tools.json');
const modelOverride = arg('model', '');
const timeoutSec = parseInt(arg('timeout', '900'), 10);
const token = arg('token', process.env.LB_API_TOKEN || process.env.CPPWORKER_API_TOKEN || '');

const payload = JSON.parse(fs.readFileSync(payloadFile, 'utf8'));
if (modelOverride) payload.model = modelOverride;
payload.stream = true;
const body = Buffer.from(JSON.stringify(payload), 'utf8');

const u = new URL(url);
const lib = u.protocol === 'https:' ? https : http;

const REASON_MARKERS = ['<think', '</think', '<thinking', '</thinking', '<reasoning', '</reasoning',
    '<analysis', '</analysis', '<|channel', '<channel|', '<|think', '<think|'];
const SERVICE_TOKENS = ['<end_of_turn>', '<start_of_turn>', '<|end|>', '<|eot_id|>', '<|start_header_id|>'];
const CALL_MARKERS = ['<tool_call>', '</tool_call>', '<tool_call|>', '<|tool_call>', '<|tool_call|>', '[TOOL_CALLS]', '"tool_calls"', '"tool_call"', '<|python_tag|>'];

const ts = new Date().toISOString().replace(/[:.]/g, '-');
const outFile = path.join('debug', `trace-turn-${ts}.ndjson`);
const raw = fs.createWriteStream(outFile, { flags: 'w' });

const started = Date.now();
let buf = '';
let n = 0;
let content = '';
let thinking = '';
const toolCalls = [];
const leaks = [];
let doneSeen = false;
let doneReason = null;
let doneBeforeContent = false;
let firstChunkAt = null;
let firstContentAt = null;
let bodyChunks = 0;
let heartbeatChunks = 0;

function secs(ms) { return (ms / 1000).toFixed(2) + 's'; }

function handleLine(line) {
    if (!line.trim()) return;
    n++;
    raw.write(line + '\n');
    const now = Date.now();
    if (firstChunkAt === null) firstChunkAt = now;

    let o;
    try { o = JSON.parse(line); } catch (e) {
        leaks.push('НЕ-JSON строка #' + n + ': ' + line.slice(0, 120));
        console.log(`  #${n} @${secs(now - started)} [не-JSON] ${line.slice(0, 120)}`);
        return;
    }

    if (o.error) {
        console.log(`  #${n} @${secs(now - started)} [error] ${String(o.error).slice(0, 160)}`);
        leaks.push('error-чанк: ' + String(o.error).slice(0, 160));
    }
    if (o.keepalive) { heartbeatChunks++; return; }

    const msg = o.message || {};
    const c = typeof msg.content === 'string' ? msg.content : '';
    const th = typeof msg.thinking === 'string' ? msg.thinking
        : (typeof msg.reasoning === 'string' ? msg.reasoning : '');

    if (c) {
        bodyChunks++;
        if (firstContentAt === null) firstContentAt = now;
        content += c;
        for (const m of REASON_MARKERS) if (c.includes(m)) leaks.push('reasoning-маркер в content: ' + JSON.stringify(c.slice(0, 80)));
        for (const m of CALL_MARKERS) if (c.includes(m)) leaks.push('маркер вызова в content: ' + JSON.stringify(c.slice(0, 80)));
        if (/"name"\s*:/.test(c) || /"parameters"\s*:/.test(c) || /"arguments"\s*:/.test(c)) {
            leaks.push('сырой JSON вызова в content: ' + JSON.stringify(c.slice(0, 80)));
        }
        for (const t of SERVICE_TOKENS) if (c.includes(t)) leaks.push('служебный токен в content: ' + JSON.stringify(c.slice(0, 80)));
        console.log(`  #${n} @${secs(now - started)} [content +${c.length}] ${JSON.stringify(c.slice(0, 70))}`);
    }
    if (th) {
        thinking += th;
        console.log(`  #${n} @${secs(now - started)} [thinking +${th.length}] ${JSON.stringify(th.slice(0, 60))}`);
    }
    if (Array.isArray(msg.tool_calls) && msg.tool_calls.length) {
        for (const tc of msg.tool_calls) {
            const fn = tc.function || {};
            let args = fn.arguments;
            const kind = typeof args;
            toolCalls.push({ name: fn.name, argsKind: kind, args });
            console.log(`  #${n} @${secs(now - started)} [tool_call] ${fn.name} args(${kind})=${JSON.stringify(args).slice(0, 160)}`);
        }
    }
    if (o.done === true) {
        doneSeen = true;
        doneReason = o.done_reason || o.doneReason || null;
        if (firstContentAt === null && toolCalls.length === 0) doneBeforeContent = true;
        console.log(`  #${n} @${secs(now - started)} [done] reason=${doneReason} eval_count=${o.eval_count}`);
    }
}

const req = lib.request({
    hostname: u.hostname,
    port: u.port,
    path: u.pathname + u.search,
    method: 'POST',
    headers: Object.assign({ 'Content-Type': 'application/json', 'Content-Length': body.length },
        token ? { 'X-API-Token': token } : {}),
}, (res) => {
    console.log(`HTTP ${res.statusCode} ct=${res.headers['content-type']} @${secs(Date.now() - started)}`);
    res.setEncoding('utf8');
    res.on('data', (chunk) => {
        buf += chunk;
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
            const line = buf.slice(0, i);
            buf = buf.slice(i + 1);
            handleLine(line);
        }
    });
    res.on('end', () => {
        if (buf.trim()) handleLine(buf);
        finish();
    });
});
req.setTimeout(timeoutSec * 1000, () => { req.destroy(new Error('timeout ' + timeoutSec + 's')); });
req.on('error', (e) => { console.error('ERR ' + e.message); leaks.push('transport: ' + e.message); finish(); });
req.end(body);

function finish() {
    raw.end();
    const totalMs = Date.now() - started;
    console.log('\n===== РАЗБОР ХОДА =====');
    console.log(`файл сырых чанков : ${outFile}`);
    console.log(`всего             : ${totalMs} мс, строк ${n} (content-чанков ${bodyChunks}, keepalive ${heartbeatChunks})`);
    console.log(`TTFT (1-й чанк)   : ${firstChunkAt ? secs(firstChunkAt - started) : 'нет'}`);
    console.log(`1-й content       : ${firstContentAt ? secs(firstContentAt - started) : 'НЕТ (стрима content не было)'}`);
    console.log(`content           : ${content.length} символов`);
    console.log(`thinking          : ${thinking.length} символов`);
    console.log(`tool_calls        : ${toolCalls.length}` + (toolCalls.length ? ' → ' + toolCalls.map((t) => t.name + '(' + t.argsKind + ')').join(', ') : ''));
    console.log(`done              : ${doneSeen ? doneReason : 'НЕ ПРИШЁЛ'}`);

    const problems = [];
    if (leaks.length) problems.push(`УТЕЧКИ В CONTENT (${leaks.length})`);
    if (doneSeen && doneBeforeContent) problems.push('done:true ПРИШЁЛ ДО любого контента (клиент покажет «COMPLETE», ничего не дождавшись)');
    if (doneSeen && !doneReason) problems.push('done без done_reason');
    if (bodyChunks === 0 && toolCalls.length === 0) problems.push('в ответе нет ни content, ни tool_calls');
    if (toolCalls.length === 0 && Array.isArray(payload.tools) && payload.tools.length > 0 && bodyChunks > 0) {
        problems.push('tools переданы, но вызова нет — текст ответа уедет клиенту как финальный ответ');
    }
    for (const t of toolCalls) {
        let a = t.args;
        if (typeof a === 'string') { try { a = JSON.parse(a); } catch (e) { /* строка */ } }
        if (typeof a !== 'object' || a === null || Array.isArray(a)) {
            problems.push(`аргументы ${t.name} не объект (${t.argsKind}) — Cline не сможет выполнить вызов`);
        }
    }
    if (leaks.length) {
        console.log('\nУТЕЧКИ:');
        leaks.slice(0, 10).forEach((l) => console.log('  - ' + l));
    }
    console.log('\n' + (problems.length ? '❌ ПРОБЛЕМЫ: ' + problems.join('; ') : '✅ ход корректен: стрим был, вызов в tool_calls, утечек нет'));
    process.exit(problems.length ? 1 : 0);
}
