// scripts/webui-layout-chip-check.js — R118.17 (2026-10-15).
//
// ЖИВАЯ ПРОВЕРКА В БРАУЗЕРЕ: страница GGUF Models на работающем стенде рисует чип
// «раскладка» с точными слоями по устройствам.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СКРИПТ, А НЕ PLAYWRIGHT-СПЕК. В этом окружении Playwright не
// может создать страницу: браузер запускается, но CDP-соединение через
// `--remote-debugging-pipe` (named pipe) не поднимается — `browserContext.newPage`
// висит до таймаута (проверено: PW 1.49, chromium установлен заново). Здесь тот же
// браузер запускается с `--remote-debugging-port` (TCP) и мы говорим с ним по CDP
// напрямую, без пайпов.
//
// Запуск:
//   node scripts/webui-layout-chip-check.js
//   WEBUI_URL=http://127.0.0.1:18083 WEBUI_API_TOKEN=<токен> node scripts/webui-layout-chip-check.js
//
// Требуется запущенный WebUI (контейнер ol-stack-webui, порт 18083) и загруженная
// модель на бэкенде — иначе чипа нет, и скрипт честно скажет об этом.

'use strict';

const { spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const BASE = process.env.WEBUI_URL || 'http://127.0.0.1:18083';
const TOKEN = process.env.WEBUI_API_TOKEN || 'changeme-bundled-with-agent-token';
const PORT = Number(process.env.CDP_PORT || 9223);

function findHeadlessShell() {
    const root = path.join(os.homedir(), 'AppData', 'Local', 'ms-playwright');
    const dirs = fs.readdirSync(root).filter((d) => d.startsWith('chromium_headless_shell-')).sort();
    for (const d of dirs.reverse()) {
        const exe = path.join(root, d, 'chrome-win', 'headless_shell.exe');
        if (fs.existsSync(exe)) return exe;
    }
    throw new Error('headless_shell.exe не найден в ' + root);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function cdp(ws, method, params, id) {
    return new Promise((resolve, reject) => {
        const onMsg = (data) => {
            const msg = JSON.parse(data.toString());
            if (msg.id === id) {
                ws.removeEventListener('message', onMsg);
                if (msg.error) reject(new Error(method + ': ' + JSON.stringify(msg.error)));
                else resolve(msg.result);
            }
        };
        ws.addEventListener('message', onMsg);
        ws.send(JSON.stringify({ id, method, params: params || {} }));
        setTimeout(() => reject(new Error(method + ': timeout')), 30000);
    });
}

(async () => {
    let child;
    let ws;
    try {
        const exe = findHeadlessShell();
        const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-profile-'));
        child = spawn(exe, [
            '--headless=new',
            `--remote-debugging-port=${PORT}`,
            `--user-data-dir=${profile}`,
            '--no-first-run',
            '--no-default-browser-check',
            '--disable-gpu',
            '--remote-allow-origins=*',
            'about:blank',
        ], { stdio: 'ignore' });

        // Ждём, пока поднимется HTTP-эндпоинт CDP.
        let target = null;
        for (let i = 0; i < 60 && !target; i++) {
            await sleep(500);
            try {
                const resp = await fetch(`http://127.0.0.1:${PORT}/json/list`);
                const list = await resp.json();
                target = list.find((t) => t.type === 'page');
            } catch (e) { /* ещё не поднялся */ }
        }
        if (!target) throw new Error('CDP не ответил на порту ' + PORT);

        ws = new WebSocket(target.webSocketDebuggerUrl);
        await new Promise((res, rej) => {
            ws.addEventListener('open', res, { once: true });
            ws.addEventListener('error', rej, { once: true });
        });

        let id = 1;
        await cdp(ws, 'Page.enable', {}, id++);
        await cdp(ws, 'Runtime.enable', {}, id++);
        // Токен и «мастер пройден» — до загрузки страницы.
        await cdp(ws, 'Page.addScriptToEvaluateOnNewDocument', {
            source: `try{localStorage.setItem('apiToken', ${JSON.stringify(TOKEN)});` +
                `localStorage.setItem('ollamalegion_wizard_done','1');` +
                `localStorage.setItem('ollamalegion_backend_type','llama_cpp');}catch(e){}`,
        }, id++);

        await cdp(ws, 'Page.navigate', { url: BASE + '/#gguf' }, id++);
        await sleep(9000); // SPA: ждём загрузки данных бэкендов/моделей

        // Убираем мастер настройки, если он всплыл, и читаем текст страницы.
        const prep = await cdp(ws, 'Runtime.evaluate', {
            expression: `(() => {
                const m = document.getElementById('setupWizardModal');
                if (m && m.parentNode) m.parentNode.removeChild(m);
                return document.body.innerText;
            })()`,
            returnByValue: true,
        }, id++);
        const text = prep.result.value || '';

        // Ищем строку с чипом раскладки и его title.
        const chip = await cdp(ws, 'Runtime.evaluate', {
            expression: `(() => {
                const els = Array.from(document.querySelectorAll('[title]'));
                const hits = els.filter(e => /раскладка/i.test(e.textContent || ''));
                if (!hits.length) return { found: false };
                const best = hits.find(e => /CUDA|RPC|CPU/.test(e.textContent || '')) || hits[0];
                return { found: true, text: (best.textContent || '').trim(), title: best.getAttribute('title') || '' };
            })()`,
            returnByValue: true,
        }, id++);
        const info = chip.result.value || { found: false };

        console.log('=== живая проверка чипа «раскладка» в браузере ===');
        console.log('страница        :', BASE + '/#gguf');
        console.log('объём текста    :', text.length, 'символов');
        if (!info.found) {
            console.log('чип «раскладка» : НЕ найден');
            const models = /Загруженные модели|loaded/i.test(text);
            console.log('модели на странице:', models ? 'есть блок, но чипа нет' : 'нет загруженных моделей');
            console.log('вердикт         : проверка не выполнена (нужна загруженная модель с раскладкой)');
            process.exitCode = 2;
            return;
        }
        console.log('чип найден      :', info.text);
        console.log('подсказка (title):', info.title || '(нет)');

        const okDevice = /CUDA\d+/.test(info.text) || /RPC\d+/.test(info.text);
        const okRange = /\d+\s*[–-]\s*\d+/.test(info.text) || /CPU\s*\(\d+\)/.test(info.text);
        const okExact = /точный список из лога llama\.cpp/.test(info.title);
        console.log('устройства      :', okDevice ? 'PASS' : 'FAIL');
        console.log('диапазоны слоёв :', okRange ? 'PASS' : 'FAIL');
        console.log('подписан фактом :', okExact ? 'PASS' : 'FAIL (в подсказке нет ссылки на лог llama.cpp)');
        process.exitCode = okDevice && okRange ? 0 : 1;
    } catch (e) {
        console.error('ERR:', e.message);
        process.exitCode = 1;
    } finally {
        try { if (ws) ws.close(); } catch (e) {}
        try { if (child) child.kill(); } catch (e) {}
    }
})();
