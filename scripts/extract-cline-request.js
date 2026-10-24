#!/usr/bin/env node
/*
 * extract-cline-request.js — достать НАСТОЯЩИЙ запрос Cline из его лога.
 *
 * ЗАЧЕМ. Проверять балансер на «похожем» payload бессмысленно: имена и схемы
 * инструментов задают и промпт, и разбор ответа. В логе Cline CLI
 * (~/.cline/data/logs/cline.log) тела запросов сохраняются целиком
 * (поле requestBodyValues у ошибок AI_APICallError). Скрипт вытаскивает
 * последнее такое тело и кладёт его в debug/cline-real-request.json,
 * подменяя только последнее сообщение пользователя на задачу.
 *
 * ЗАПУСК:
 *   node scripts/extract-cline-request.js ["задача для агента"]
 */

'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');

const logPath = path.join(os.homedir(), '.cline', 'data', 'logs', 'cline.log');
const task = process.argv[2] || 'List the files in the current working directory and tell me how many there are.';
const outPath = path.join('debug', 'cline-real-request.json');

const text = fs.readFileSync(logPath, 'utf8');

// Последнее вхождение ключа — самый свежий запрос.
const key = '"requestBodyValues":';
const at = text.lastIndexOf(key);
if (at < 0) {
    console.error('в логе нет requestBodyValues — запрос не сохранился (лог: ' + logPath + ')');
    process.exit(1);
}

// Значение начинается с '{' — берём сбалансированный по скобкам фрагмент.
const start = text.indexOf('{', at + key.length);
let depth = 0, inStr = false, esc = false, end = -1;
for (let i = start; i < text.length; i++) {
    const c = text[i];
    if (inStr) {
        if (esc) { esc = false; } else if (c === '\\') { esc = true; } else if (c === '"') { inStr = false; }
        continue;
    }
    if (c === '"') { inStr = true; continue; }
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
}
if (end < 0) {
    console.error('не удалось найти конец объекта requestBodyValues');
    process.exit(1);
}

// В JSON-строке лога объект экранирован — декодируем через JSON.parse обёртки.
let body;
const frag = text.slice(start, end);
try {
    body = JSON.parse(frag);
} catch (e) {
    try {
        body = JSON.parse(JSON.parse('"' + frag.replace(/"/g, '\\"') + '"'));
    } catch (e2) {
        console.error('не удалось разобрать тело запроса: ' + e.message);
        process.exit(1);
    }
}

const messages = Array.isArray(body.messages) ? body.messages : [];
const lastUser = messages.map((m, i) => (m.role === 'user' ? i : -1)).filter((i) => i >= 0).pop();
if (lastUser !== undefined) messages[lastUser] = { role: 'user', content: task };

const out = {
    model: body.model,
    messages,
    tools: body.tools,
    tool_choice: body.tool_choice,
    max_output_tokens: body.max_output_tokens,
    options: body.options,
    stream: true,
};
fs.writeFileSync(outPath, JSON.stringify(out, null, 2));
console.log('сохранено: ' + outPath);
console.log('model: ' + out.model + ' | tools: ' + (out.tools || []).length + ' | messages: ' + out.messages.length);
console.log('инструменты: ' + (out.tools || []).map((t) => t.function && t.function.name).filter(Boolean).join(', '));
console.log('задача: ' + task);
