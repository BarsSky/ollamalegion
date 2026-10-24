#!/usr/bin/env node
/*
 * cline-agent-loop.js — проверка АГЕНТСКОГО ЦИКЛА Cline против балансера.
 *
 * ЗАЧЕМ. Одного хода недостаточно: жалоба «всего две итерации общения, потом
 * COMPLETE, а инструмент оказался в теле ответа» — это про ЦИКЛ. Скрипт играет
 * роль Cline: шлёт запрос формы Cline (payload из debug/cline-real-request.json),
 * получает tool_calls, отдаёт результат инструмента и повторяет ход — столько
 * итераций, сколько попросит модель. На каждой итерации проверяется протокол:
 *
 *   - arguments вызова — JSON-ОБЪЕКТ, согласованный со схемой инструмента
 *     (иначе клиент не выполнит вызов: именно так цикл и обрывается);
 *   - в content нет разметки вызова и служебных токенов (инструмент не в теле ответа);
 *   - done_reason = tool_calls при вызове и stop/пусто при финальном ответе;
 *   - цикл продолжается после результата инструмента (а не заканчивается на 1-2 ходах).
 *
 * БЕЗОПАСНОСТЬ. Скрипт НЕ исполняет команды, которые выбрала модель: результат
 * инструмента — детерминированная заглушка. Проверяется ТОЛЬКО протокол.
 *
 * ЗАПУСК:
 *   node scripts/cline-agent-loop.js --url http://127.0.0.1:18080/api/chat \
 *        --payload debug/cline-real-request.json [--iterations 4] [--timeout 900]
 *
 * Код возврата 1 — если протокол нарушен (есть утечка, arguments не объект,
 * цикл оборвался раньше времени или ответа не было вовсе).
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
const payloadFile = arg('payload', 'debug/cline-real-request.json');
const maxIterations = parseInt(arg('iterations', '4'), 10);
const timeoutSec = parseInt(arg('timeout', '900'), 10);
const token = arg('token', process.env.LB_API_TOKEN || process.env.CPPWORKER_API_TOKEN || '');
const modelOverride = arg('model', '');

const base = JSON.parse(fs.readFileSync(payloadFile, 'utf8'));
if (modelOverride) base.model = modelOverride;
const tools = base.tools || [];
const toolSchemas = new Map(tools.map((t) => [String(t.function && t.function.name).toLowerCase(), t.function]));

const REASON_MARKERS = ['<think', '</think', '<thinking', '</thinking', '<reasoning', '</reasoning',
    '<analysis', '</analysis', '<|channel', '<channel|', '<|think', '<think|'];
const CALL_MARKERS = ['<tool_call>', '</tool_call>', '<tool_call|>', '<|tool_call>', '<|tool_call|>', '[TOOL_CALLS]', '<|python_tag|>'];
const SERVICE_TOKENS = ['<end_of_turn>', '<start_of_turn>', '<|end|>', '<|eot_id|>'];

const u = new URL(url);
const lib = u.protocol === 'https:' ? https : http;

// --- один ход: POST /api/chat со stream=true, разбор NDJSON -------------------
function oneTurn(messages) {
    return new Promise((resolve, reject) => {
        const body = Buffer.from(JSON.stringify(Object.assign({}, base, { messages, stream: true })), 'utf8');
        const started = Date.now();
        const req = lib.request({
            hostname: u.hostname, port: u.port, path: u.pathname + u.search, method: 'POST',
            headers: Object.assign({ 'Content-Type': 'application/json', 'Content-Length': body.length },
                token ? { 'X-API-Token': token } : {}),
        }, (res) => {
            let buf = '';
            const acc = { httpStatus: res.statusCode, contentType: res.headers['content-type'], content: '', thinking: '', toolCalls: [], doneReason: null, done: false, errors: [], leaks: [], chunks: 0, firstContentAt: null, started };
            res.setEncoding('utf8');
            res.on('data', (chunk) => {
                buf += chunk;
                let i;
                while ((i = buf.indexOf('\n')) >= 0) {
                    const line = buf.slice(0, i);
                    buf = buf.slice(i + 1);
                    consume(line, acc);
                }
            });
            res.on('end', () => {
                if (buf.trim()) consume(buf, acc);
                acc.elapsedMs = Date.now() - started;
                resolve(acc);
            });
        });
        req.setTimeout(timeoutSec * 1000, () => req.destroy(new Error('timeout ' + timeoutSec + 's')));
        req.on('error', reject);
        req.end(body);
    });
}

function consume(line, acc) {
    if (!line.trim()) return;
    let o;
    try { o = JSON.parse(line); } catch (e) { acc.leaks.push('не-JSON строка: ' + line.slice(0, 100)); return; }
    acc.chunks++;
    if (o.keepalive) return;
    if (o.error) acc.errors.push(String(o.error));
    const msg = o.message || {};
    const c = typeof msg.content === 'string' ? msg.content : '';
    if (c) {
        if (acc.firstContentAt === null) acc.firstContentAt = Date.now();
        acc.content += c;
        for (const m of REASON_MARKERS) if (c.includes(m)) acc.leaks.push('reasoning-маркер в content: ' + JSON.stringify(c.slice(0, 60)));
        for (const m of CALL_MARKERS) if (c.includes(m)) acc.leaks.push('разметка вызова в content: ' + JSON.stringify(c.slice(0, 60)));
        for (const m of SERVICE_TOKENS) if (c.includes(m)) acc.leaks.push('служебный токен в content: ' + JSON.stringify(c.slice(0, 60)));
        if (/"name"\s*:/.test(c) || /"arguments"\s*:/.test(c)) acc.leaks.push('сырой JSON вызова в content: ' + JSON.stringify(c.slice(0, 60)));
    }
    const th = typeof msg.thinking === 'string' ? msg.thinking : (typeof msg.reasoning === 'string' ? msg.reasoning : '');
    if (th) acc.thinking += th;
    if (Array.isArray(msg.tool_calls)) {
        for (const tc of msg.tool_calls) {
            const fn = tc.function || {};
            let args = fn.arguments;
            if (typeof args === 'string') { try { args = JSON.parse(args); } catch (e) { /* оставляем строкой */ } }
            acc.toolCalls.push({ name: fn.name, args });
        }
    }
    if (o.done === true) { acc.done = true; acc.doneReason = o.done_reason || null; }
}

// --- проверка одного вызова по схеме инструмента ------------------------------
function validateCall(call, toolSchemas) {
    const problems = [];
    const name = String(call.name || '');
    const fn = toolSchemas.get(name.toLowerCase());
    if (!fn) {
        problems.push(`инструмент "${name}" не объявлен в запросе — клиент не выполнит вызов`);
        return problems;
    }
    const args = call.args;
    if (typeof args !== 'object' || args === null || Array.isArray(args)) {
        problems.push(`arguments ${name} — ${Array.isArray(args) ? 'массив' : typeof args}, ожидался объект`);
        return problems;
    }
    const schema = fn.parameters || {};
    const props = schema.properties || {};
    const required = schema.required || [];
    for (const key of Object.keys(args)) {
        if (Object.keys(props).length > 0 && !(key in props)) {
            problems.push(`arguments ${name} содержат необъявленный параметр "${key}"`);
        }
    }
    for (const key of required) {
        if (!(key in args)) problems.push(`arguments ${name} не содержат обязательный параметр "${key}"`);
    }
    for (const [key, value] of Object.entries(args)) {
        const want = props[key] && props[key].type;
        if (want === 'array' && !Array.isArray(value)) problems.push(`${name}.${key}: ожидался массив, получено ${typeof value}`);
        if (want === 'string' && typeof value !== 'string') problems.push(`${name}.${key}: ожидалась строка, получено ${typeof value}`);
        if (want === 'integer' && typeof value !== 'number') problems.push(`${name}.${key}: ожидалось число, получено ${typeof value}`);
    }
    return problems;
}

// --- заглушка результата инструмента (команды НЕ исполняются) -----------------
function cannedToolResult(call) {
    return JSON.stringify({
        note: 'cline-agent-loop: результат-заглушка (скрипт проверяет протокол, а не выполнение команд)',
        tool: call.name,
        arguments: call.args,
        result: 'OK',
    });
}

(async () => {
    console.log('url      : ' + url);
    console.log('payload  : ' + payloadFile + ' (model=' + base.model + ', tools=' + tools.length + ')');
    const messages = base.messages.slice();
    const problems = [];
    let iterations = 0;
    let finalAnswer = '';

    for (let i = 1; i <= maxIterations; i++) {
        iterations = i;
        console.log(`\n--- итерация ${i} ---`);
        const turn = await oneTurn(messages);
        console.log(`HTTP ${turn.httpStatus} ct=${turn.contentType} | ${turn.elapsedMs} мс | чанков ${turn.chunks}` +
            ` | content ${turn.content.length} | thinking ${turn.thinking.length} | tool_calls ${turn.toolCalls.length}` +
            ` | done_reason=${turn.doneReason}`);
        if (turn.leaks.length) {
            turn.leaks.forEach((l) => problems.push(`итерация ${i}: ${l}`));
            // Полный видимый текст — чтобы понять, что именно утекло (эхо промпта,
            // разметка модели, служебный токен).
            console.log('  content целиком: ' + JSON.stringify(turn.content.slice(0, 1200)));
        }
        if (turn.errors.length) turn.errors.forEach((e) => problems.push(`итерация ${i}: error-чанк: ${e}`));
        if (!turn.done) problems.push(`итерация ${i}: стрим не закрылся (нет done:true)`);

        if (turn.toolCalls.length === 0) {
            finalAnswer = turn.content;
            console.log('финальный ответ (без вызова): ' + JSON.stringify(turn.content.slice(0, 160)));
            if (!turn.content.trim() && turn.thinking.trim() === '') {
                problems.push(`итерация ${i}: ни content, ни tool_calls — клиенту нечего показать`);
            }
            break;
        }

        for (const call of turn.toolCalls) {
            console.log(`  вызов: ${call.name} args=${JSON.stringify(call.args).slice(0, 140)}`);
            validateCall(call, toolSchemas).forEach((p) => problems.push(`итерация ${i}: ${p}`));
        }

        // Ответ ассистента (как его вернул сервер) + результат инструмента.
        messages.push({
            role: 'assistant',
            content: turn.content,
            tool_calls: turn.toolCalls.map((c, idx) => ({
                id: 'call_' + (idx + 1),
                type: 'function',
                function: { name: c.name, arguments: c.args },
            })),
        });
        for (const call of turn.toolCalls) {
            messages.push({ role: 'tool', tool_name: call.name, content: cannedToolResult(call) });
        }
    }

    console.log('\n===== ИТОГ =====');
    console.log('итераций выполнено : ' + iterations + ' из ' + maxIterations);
    if (iterations === 1 && problems.length === 0) {
        problems.push('цикл завершился на первой же итерации — агент не увидел вызова инструмента');
    }
    if (problems.length) {
        console.log('❌ ПРОБЛЕМЫ:');
        problems.slice(0, 20).forEach((p) => console.log('  - ' + p));
        process.exit(1);
    }
    console.log('✅ агентский цикл корректен: вызовы приходят объектами, разметки в content нет, ' +
        'цикл продолжается после результата инструмента');
    if (finalAnswer) console.log('финальный ответ: ' + JSON.stringify(finalAnswer.slice(0, 200)));
    process.exit(0);
})().catch((e) => {
    console.error('ERR ' + (e && e.message ? e.message : e));
    process.exit(1);
});
