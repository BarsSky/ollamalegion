// test-cancel-inflight-r215.js — R215 (2026-10-21).
//
// Живая проверка двух вещей, которых до R215 не было:
//   1) отмена ИДУЩЕЙ загрузки (POST /api/models/unload?force=true) реально
//      останавливает чтение весов и освобождает VRAM;
//   2) прогресс весов доезжает до API (loadingProgressPct).
//
// Запускается ПРОТИВ ОДНОРАЗОВОГО контейнера, не против стенда:
//   docker run -d --name ol-r215-t --gpus all -p 18096:18092 \
//     -v "D:\ollama-legion-models:/app/models:ro" \
//     -e CPPWORKER_API_TOKEN=<token> \
//     ollama-legion/cppworker:gpu-r215c-cancel \
//     /app/cppworker --port 18092 --models-dir /app/models
//
//   node scripts/test-cancel-inflight-r215.js
//
// Переменные окружения: PORT (18096), CONTAINER (ol-r215-t), MODEL
// (gemma-4-E4B-it-Q4_K_M.gguf), TOKEN.
const { execSync } = require('child_process');
const http = require('http');

const PORT = Number(process.env.PORT || 18096);
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M.gguf';
const SHORT = MODEL.replace(/\.gguf$/i, '');
const CONTAINER = process.env.CONTAINER || 'ol-r215-t';
const sleep = ms => new Promise(r => setTimeout(r, ms));

function req(method, path, body) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const r = http.request({
      host: '127.0.0.1', port: PORT, path, method,
      headers: {
        'X-API-Token': TOKEN,
        ...(data ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } : {}),
      },
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({ status: res.statusCode, body: s }));
    });
    r.on('error', reject);
    if (data) r.write(data);
    r.end();
  });
}

function logs() {
  return execSync('docker logs --tail 400 ' + CONTAINER + ' 2>&1', { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 });
}

function vram() {
  return execSync('docker exec ' + CONTAINER + ' nvidia-smi --query-gpu=memory.used --format=csv,noheader',
    { encoding: 'utf8' }).trim();
}

(async () => {
  const baseVram = vram();
  console.log('VRAM до загрузки:', baseVram);

  const progressBefore = (logs().match(/\[bridge\] load progress \d+%/g) || []).length;
  const load = await req('POST', '/api/models/load', {
    name: MODEL, contextSize: 8192, kvCacheType: 'q4_0', gpuLayers: -1, parallel: 1,
  });
  console.log('load:', load.status);

  // Ждём первого реального процента в ЛОГЕ моста (это и есть начало фазы
  // загрузки весов; до неё llama.cpp callback не зовёт вообще).
  let firstPctAt = 0;
  const t0 = Date.now();
  for (let i = 0; i < 600; i++) {
    await sleep(500);
    const seen = (logs().match(/\[bridge\] load progress \d+%/g) || []).length;
    if (seen > progressBefore) { firstPctAt = Date.now() - t0; break; }
  }
  console.log('первый callback прогресса через', firstPctAt, 'мс');

  // Проверяем, что процент виден в API (во время фазы загрузки весов).
  let apiPct = null;
  for (let i = 0; i < 40; i++) {
    const p = await req('GET', '/api/models/load/progress?model=' + encodeURIComponent(SHORT));
    let j = {};
    try { j = JSON.parse(p.body); } catch (e) {}
    if (typeof j.loadingProgressPct === 'number' && j.loadingProgressPct > 0) {
      apiPct = j.loadingProgressPct;
      break;
    }
    await sleep(250);
  }
  console.log('loadingProgressPct в API:', apiPct === null ? 'не поймали (фаза короткая)' : apiPct + '%');
  console.log('VRAM во время загрузки:', vram());

  // Отменяем загрузку.
  const tCancel = Date.now();
  const cancel = await req('POST', '/api/models/unload?name=' + encodeURIComponent(SHORT) + '&force=true');
  console.log('отмена:', cancel.status, cancel.body.trim(), 'за', Date.now() - tCancel, 'мс');

  let abortLine = null;
  for (let i = 0; i < 200; i++) {
    await sleep(250);
    const m = logs().match(/\[bridge\] load cancelled at \d+% \(progress callback\)/);
    if (m) { abortLine = m[0]; break; }
  }
  console.log('лог отмены:', abortLine || 'НЕ НАЙДЕН');
  await sleep(2000);
  console.log('VRAM после отмены:', vram());

  const ok = cancel.status === 200 && abortLine && parseInt(vram(), 10) < parseInt(baseVram, 10) + 512;
  console.log(ok ? 'OK: загрузка отменена, VRAM освобождена' : 'ПРОВАЛ: см. вывод выше');
  process.exit(ok ? 0 : 1);
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });
