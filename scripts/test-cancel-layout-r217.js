// R217: отмена загрузки ПРИ АКТИВНОЙ РАСКЛАДКЕ (donor). Проверяем, что после
// abort освобождаются буферы и на доноре, а узел не остаётся «занятым».
//
// Запуск: node scripts/test-cancel-layout-r217.js
// Требует: мастер-воркер на 18092, донор 192.0.2.10:50052 в раскладке.
const { execSync } = require('child_process');
const http = require('http');

const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'Qwen3.8-27B-UD-Q4_K_M.gguf';
const SHORT = MODEL.replace(/\.gguf$/i, '');
const MASTER_CONTAINER = 'ol-stack-cppworker-gpu';
const DONOR_SSH = 'legion2';
const DONOR_CONTAINER = 'cppworker-node2';
const sleep = ms => new Promise(r => setTimeout(r, ms));

function req(method, path, body) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const r = http.request({
      host: '127.0.0.1', port: 18092, path: path, method,
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

function vram(container, viaSsh) {
  try {
    const cmd = 'docker exec ' + container + ' nvidia-smi --query-gpu=memory.used --format=csv,noheader';
    const out = viaSsh
      ? execSync(`ssh ${DONOR_SSH} "${cmd}"`, { encoding: 'utf8', maxBuffer: 4 << 20 })
      : execSync(cmd, { encoding: 'utf8', maxBuffer: 4 << 20 });
    return out.trim();
  } catch (e) {
    return 'ERR ' + e.message;
  }
}

(async () => {
  console.log('VRAM master/donor до:', vram(MASTER_CONTAINER, false), '/', vram(DONOR_CONTAINER, true));

  const load = await req('POST', '/api/models/load', {
    name: MODEL, contextSize: 8192, kvCacheType: 'q4_0', gpuLayers: -2, parallel: 1,
  });
  console.log('load:', load.status, JSON.parse(load.body || '{}').status || '');

  let pct = 0, waited = 0;
  for (let i = 0; i < 200; i++) {
    await sleep(2000); waited += 2;
    const p = await req('GET', '/api/models/load/progress?model=' + encodeURIComponent(SHORT));
    let j = {};
    try { j = JSON.parse(p.body); } catch (e) {}
    if (j.state === 'loaded') { console.log('модель успела загрузиться за', waited, 'с — отмена не проверится'); return; }
    if (typeof j.loadingProgressPct === 'number' && j.loadingProgressPct > 0) {
      pct = j.loadingProgressPct;
      console.log('прогресс', pct + '%', 'через', waited, 'с');
      break;
    }
  }
  if (!pct) { console.log('прогресса не дождались за', waited, 'с'); }

  const t0 = Date.now();
  const cancel = await req('POST', '/api/models/unload?name=' + encodeURIComponent(SHORT) + '&force=true');
  console.log('отмена:', cancel.status, cancel.body.trim(), 'за', Date.now() - t0, 'мс');

  await sleep(3000);
  console.log('VRAM master/donor после:', vram(MASTER_CONTAINER, false), '/', vram(DONOR_CONTAINER, true));

  const peers = await req('GET', '/api/v1/cppworker/rpc-peers');
  console.log('rpc-peers:', peers.body.trim());
  const models = await req('GET', '/api/models');
  const mj = JSON.parse(models.body || '{}');
  console.log('rpc_layout:', JSON.stringify(mj.rpc_layout || null), 'rpc_servers_active:', JSON.stringify(mj.rpc_servers_active || null));
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });
