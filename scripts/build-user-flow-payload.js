#!/usr/bin/env node
/*
 * build-user-flow-payload.js — собрать payload из РЕАЛЬНОЙ сессии Cline.
 *
 * ЗАЧЕМ. Проверять надо не «похожий» запрос, а тот, на котором оператор увидел
 * дефект: в сохранённой сессии Cline (data/sessions/<id>/<id>.messages.json) видно,
 * что в content уехал обрывок JSON вызова. Скрипт переносит сообщения сессии в
 * формат Ollama /api/chat (system из последнего реального запроса + роли
 * user/assistant/tool) и кладёт результат в debug/cline-user-flow.json.
 *
 * ЗАПУСК:
 *   node scripts/build-user-flow-payload.js [sessionId]
 */

'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');

const sessionsDir = path.join(os.homedir(), '.cline', 'data', 'sessions');
let sessionId = process.argv[2];
if (!sessionId) {
    const dirs = fs.readdirSync(sessionsDir, { withFileTypes: true })
        .filter((d) => d.isDirectory())
        .map((d) => ({ id: d.name, mtime: fs.statSync(path.join(sessionsDir, d.name)).mtimeMs }))
        .sort((a, b) => b.mtime - a.mtime);
    sessionId = dirs.length ? dirs[0].id : null;
}
if (!sessionId) {
    console.error('сессии Cline не найдены');
    process.exit(1);
}

const messagesFile = path.join(sessionsDir, sessionId, `${sessionId}.messages.json`);
const session = JSON.parse(fs.readFileSync(messagesFile, 'utf8'));

// Системный промпт и определения инструментов берём из последнего реального
// запроса Cline (их же использует trace-cline-turn).
const base = JSON.parse(fs.readFileSync(path.join('debug', 'cline-real-request.json'), 'utf8'));
const systemMsg = (base.messages || []).find((m) => m.role === 'system');

function textOf(content) {
    if (typeof content === 'string') return content;
    if (!Array.isArray(content)) return '';
    const parts = [];
    for (const p of content) {
        if (p.type === 'text' && p.text) parts.push(p.text);
        // Результат инструмента: Cline хранит его как отдельную часть.
        if (p.type === 'tool_result') parts.push(textOf(p.content) || 'ok');
    }
    return parts.join('\n');
}

function toolCallsOf(content) {
    if (!Array.isArray(content)) return null;
    const calls = content.filter((p) => p.type === 'tool_use').map((p) => ({
        function: { name: p.name, arguments: p.input || {} },
    }));
    return calls.length ? calls : null;
}

const out = { messages: [] };
if (systemMsg) out.messages.push({ role: 'system', content: systemMsg.content });

for (const m of session.messages || []) {
    if (m.role === 'user') {
        out.messages.push({ role: 'user', content: textOf(m.content) || String(m.content || '') });
        continue;
    }
    if (m.role === 'assistant') {
        const msg = { role: 'assistant', content: textOf(m.content) };
        const calls = toolCallsOf(m.content);
        if (calls) msg.tool_calls = calls;
        out.messages.push(msg);
        continue;
    }
    if (m.role === 'tool' || m.role === 'tool_result') {
        out.messages.push({ role: 'tool', tool_name: m.name || '', content: textOf(m.content) || 'ok' });
    }
}

const payload = Object.assign({}, base, { messages: out.messages, stream: true });
const outPath = path.join('debug', 'cline-user-flow.json');
fs.writeFileSync(outPath, JSON.stringify(payload, null, 2));

console.log('сессия     : ' + sessionId);
console.log('сообщений  : ' + payload.messages.length + ' (system=' + (systemMsg ? 1 : 0) + ')');
console.log('инструментов: ' + (payload.tools || []).length);
console.log('сохранено  : ' + outPath);
for (const [i, m] of payload.messages.entries()) {
    const txt = (m.content || '').replace(/\s+/g, ' ').slice(0, 120);
    console.log(`  [${i + 1}] ${m.role}${m.tool_calls ? ' +tool_calls(' + m.tool_calls.length + ')' : ''}: ${txt}`);
}
