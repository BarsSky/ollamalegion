// scripts/webui-layout-chip-browser.js — R118.22 (2026-10-15).
//
// ЧИП «РАСКЛАДКА» В РЕАЛЬНОМ БРАУЗЕРНОМ DOM.
//
// Браузер здесь физически не может открыть сетевой сокет: проверено тремя
// способами — chromium.launch() (CDP через pipe висит), сырые CDP-команды по TCP
// (Page.enable: timeout) и connectOverCDP (подключение работает, но page.goto к
// 127.0.0.1:18083 не проходит, а fetch из страницы даёт TypeError: Failed to fetch).
// Поэтому «открыть живую страницу WebUI и прочитать чип» тут нельзя никаким
// инструментом.
//
// Что делается вместо: настоящий Chromium (Playwright connectOverCDP), в него
// загружаются НАСТОЯЩИЕ модули WebUI с диска, им подаются ЖИВЫЕ данные стенда
// (их забирает Node — у него сеть есть), а текст чипа читается ОБРАТНО ИЗ DOM.
// Проверяется рендер-конвейер в реальном браузере; за кадром остаётся только
// сетевой путь страницы.
//
// Запуск: node scripts/webui-layout-chip-browser.js

'use strict';

const { spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const LB = process.env.LB_URL || 'http://127.0.0.1:18081';
const WORKER = process.env.CPPWORKER_URL || 'http://127.0.0.1:18092';
const TOKEN = process.env.WEBUI_API_TOKEN || 'changeme-bundled-with-agent-token';
const PORT = Number(process.env.CDP_PORT || 9223);
const REPO = path.resolve(__dirname, '..');

function findChromium() {
    const root = path.join(os.homedir(), 'AppData', 'Local', 'ms-playwright');
    const dirs = fs.readdirSync(root).filter((d) => /^chromium-\d+$/.test(d)).sort();
    for (const d of dirs.reverse()) {
        for (const rel of ['chrome-win/chrome.exe', 'chrome-win/headless_shell.exe']) {
            const exe = path.join(root, d, rel);
            if (fs.existsSync(exe)) return exe;
        }
    }
    throw new Error('chromium не найден в ' + root);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function getJSON(url) {
    const resp = await fetch(url, { headers: { 'X-API-Token': TOKEN } });
    if (!resp.ok) throw new Error(url + ' -> HTTP ' + resp.status);
    return resp.json();
}

function buildState(backend, workerModels, runtime) {
    const link = backend.rpcLink || null;
    const linkAuto = backend.rpcLinkAuto || null;
    const guard = backend.rpcLayoutGuard || null;
    const map = {};
    for (const m of (runtime.loaded_models || [])) map[m.name] = m;
    return {
        rpcLayout: {
            active: !!workerModels.rpc_enabled,
            degraded: !!(guard && guard.degraded),
            degrade_reason: (guard && guard.reason) || '',
            nodes: Array.isArray(workerModels.rpc_servers_active) ? workerModels.rpc_servers_active : [],
            remote_nodes: workerModels.vram_remote_nodes || 0,
            link_mbps: (link && link.mbps) || 0,
            link_auto_mbps: (linkAuto && linkAuto.mbps) || 0,
            link_auto_node: (linkAuto && linkAuto.node) || '',
        },
        runtimeModels: map,
    };
}

(async () => {
    const { chromium } = require('playwright');

    const backends = await getJSON(LB + '/api/v1/backends');
    const backend = (backends.backends || []).find((b) => b.role === 'main' && (b.loadedModels || []).length);
    if (!backend) {
        console.log('нет бэкенда с загруженными моделями — проверять нечего');
        process.exitCode = 2;
        return;
    }
    const workerModels = await getJSON(WORKER + '/api/models');
    const runtime = await getJSON(LB + '/api/v1/gguf/backends/' + backend.id + '/proxy/api/v1/cppworker/config/runtime');
    const state = buildState(backend, workerModels, runtime);

    const exe = findChromium();
    const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'legion-dom-'));
    const child = spawn(exe, [
        '--headless=new', '--remote-debugging-port=' + PORT, '--user-data-dir=' + profile,
        '--no-first-run', '--no-default-browser-check', '--disable-gpu', '--remote-allow-origins=*',
        'about:blank',
    ], { stdio: 'ignore' });

    let browser;
    try {
        for (let i = 0; i < 40; i++) {
            try { const r = await fetch('http://127.0.0.1:' + PORT + '/json/version'); if (r.ok) break; } catch (e) {}
            await sleep(500);
        }
        browser = await chromium.connectOverCDP('http://127.0.0.1:' + PORT);
        const ctx = browser.contexts()[0] || await browser.newContext();
        const page = await ctx.newPage();
        const errors = [];
        page.on('pageerror', (e) => errors.push(String(e)));

        await page.setContent('<!doctype html><html><body><div id="gguf"></div></body></html>');
        await page.addScriptTag({ content: [
            'window.GgufModule = { state: { runtimeModels: {}, rpcLayout: null }, _: function(k){return k;},',
            '  stripGGUF: function(n){return n;}, formatFileSize: function(){return "-";},',
            '  formatVramMB: function(n){return String(n);}, showToast: function(){},',
            '  refreshDetailPane: function(){}, refreshDetailPanel: function(){} };',
            'window.Utils = { escapeHtml: function(s){ return s === null || s === undefined ? "" : String(s); } };',
            'window.I18N = { t: function(k){ return k; } };',
        ].join('\n') });
        await page.addScriptTag({ path: path.join(REPO, 'webui/js/modules/gguf-renderer-refresh.js') });
        await page.addScriptTag({ path: path.join(REPO, 'webui/js/modules/gguf-renderer-detail-render.js') });

        const result = await page.evaluate((st) => {
            const M = window.GgufModule;
            if (typeof M.resolveDeviceLayers !== 'function') return { error: 'resolveDeviceLayers не загрузился' };
            M.state.runtimeModels = st.runtimeModels;
            M.state.rpcLayout = st.rpcLayout;
            M.resolveDeviceLayers(M.state);
            const names = Object.keys(M.state.runtimeModels);
            if (!names.length) return { error: 'нет моделей в живых данных' };
            const chips = [];
            for (const name of names) {
                const flags = M.buildLayoutFlags(M.state.runtimeModels[name], M.state.rpcLayout);
                for (const f of flags) {
                    if (f.label !== 'раскладка') continue;
                    const el = document.createElement('span');
                    el.className = 'chip';
                    el.setAttribute('title', f.title || '');
                    el.textContent = f.label + ': ' + f.value;
                    document.getElementById('gguf').appendChild(el);
                    chips.push({ name: name, value: f.value, title: f.title || '' });
                }
            }
            return { dom: document.getElementById('gguf').innerText, chips: chips };
        }, state);

        console.log('=== чип «раскладка» в реальном браузерном DOM ===');
        console.log('браузер     :', exe.split(path.sep).slice(-3).join('/'));
        console.log('бэкенд      :', backend.id, '| rpc активна:', state.rpcLayout.active,
            '| узлы:', state.rpcLayout.nodes.join(',') || '—');
        console.log('ошибки JS   :', errors.length ? errors.join('; ') : 'нет');
        if (result.error) {
            console.log('вердикт     : НЕ ВЫПОЛНЕНО —', result.error);
            process.exitCode = 1;
        } else {
            console.log('текст из DOM:');
            console.log('  ' + String(result.dom).replace(/\n/g, '\n  '));
            let fail = 0;
            for (const c of result.chips) {
                const okDev = /CUDA\d+|RPC\d+/.test(c.value);
                const okRange = /\d+\s*[–-]\s*\d+/.test(c.value) || /CPU\s*\(\d+\)/.test(c.value);
                const okExact = /точный список из лога llama\.cpp/.test(c.title);
                console.log('  ' + c.name + ': устройства=' + (okDev ? 'PASS' : 'FAIL') +
                    ' диапазоны=' + (okRange ? 'PASS' : 'FAIL') + ' факт=' + (okExact ? 'PASS' : 'FAIL'));
                if (!okDev || !okRange) fail++;
            }
            process.exitCode = fail ? 1 : 0;
        }
    } catch (e) {
        console.error('ERR:', e.message);
        process.exitCode = 1;
    } finally {
        try { if (browser) await browser.close(); } catch (e) {}
        try { child.kill(); } catch (e) {}
    }
})();
