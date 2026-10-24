// R217: сгенерировать CMD-скрипт для обновления контейнера донора на новый
// образ cppworker, сохранив ВСЁ окружение, порты и монтирования со старого
// контейнера (он был запущен docker run, без compose-файла).
//
// Использование:
//   ssh legion2 'docker inspect cppworker-node2' | Out-File -Encoding utf8 _diag/donor-full.json
//   node scripts/make-donor-update-cmd.js _diag/donor-full.json <new-image> [old-name]
//
// Пишет deployments/donor/update.cmd. См. docs/donor-update.md.
const fs = require('fs');

const inspectPath = process.argv[2] || '_diag/donor-full.json';
const raw = fs.readFileSync(inspectPath, 'utf8');
const c = JSON.parse(raw.slice(raw.indexOf('[')))[0];
const NEW_IMAGE = process.argv[3] || process.env.NEW_IMAGE || 'ollama-legion/cppworker:gpu-r216c-reasoning';
const OLD_NAME = process.argv[4] || process.env.OLD_NAME || 'cppworker-node2';
const KEEP_NAME = OLD_NAME + '-old';
const OUT = process.env.OUT_CMD || 'deployments/donor/update.cmd';

// Переменные, которые приходят из базового образа nvidia/cuda, а не заданы
// оператором. Их не повторяем: они и так в образе, а PATH дублировать вредно.
const imageBaked = new Set([
  'PATH', 'NVARCH', 'NVIDIA_REQUIRE_CUDA', 'NVIDIA_VISIBLE_DEVICES',
  'NVIDIA_DRIVER_CAPABILITIES', 'CPPWORKER_CUDA_ARCH_BUILT', 'CPPWORKER_LLAMA_BUILD_BAKED',
]);
const env = (c.Config.Env || []).filter(e => {
  const k = e.split('=')[0];
  return !imageBaked.has(k);
});

const binds = (c.HostConfig.Binds || []).map(b => {
  const idx = b.indexOf(':');
  if (idx < 0) return b;
  const src = b.slice(0, idx);
  const dst = b.slice(idx);
  if (!src.includes('/run/desktop/mnt/host/')) return b;
  // Нормализуем ТОЛЬКО host-часть: путь контейнера обязан остаться с прямыми
  // слэшами (/app/models), иначе Docker создаст другое имя внутри контейнера.
  return src.replace('/run/desktop/mnt/host/c/', 'C:\\').replace(/\//g, '\\') + dst;
});

const lines = [];
lines.push('@echo off');
lines.push('setlocal');
lines.push('REM R217: обновление донора на ' + NEW_IMAGE);
lines.push('REM Старый контейнер ПЕРЕИМЕНОВЫВАЕТСЯ и ОСТАНАВЛИВАЕТСЯ (не удаляется):');
lines.push('REM остановка освобождает порты, а сам контейнер остаётся для отката.');
// R219-fix: ПОРЯДОК ВАЖЕН. Скрипт не должен трогать работающий контейнер до
// переименования — иначе при повторном запуске он удалял текущий рабочий узел
// (живой инцидент 2026-10-21: `docker rm -f cppworker-node2` снёс рабочий
// контейнер, и rename после этого падал «no such container»).
lines.push('REM Откат: docker rm -f ' + OLD_NAME + ' ^& docker rename ' + KEEP_NAME + ' ' + OLD_NAME + ' ^& docker start ' + OLD_NAME);
lines.push('REM Если рабочего контейнера нет — нечего обновлять, выходим с ошибкой.');
lines.push('docker inspect ' + OLD_NAME + ' >nul 2>&1 || (echo ERROR: container ' + OLD_NAME + ' not found & exit /b 2)');
lines.push('REM Освобождаем имя бэкапа ТОЛЬКО после проверки, что рабочий контейнер на месте.');
lines.push('docker rm -f ' + KEEP_NAME + ' >nul 2>&1');
lines.push('docker rename ' + OLD_NAME + ' ' + KEEP_NAME + '');
lines.push('docker stop ' + KEEP_NAME + ' >nul 2>&1');
lines.push('docker run -d --name ' + OLD_NAME + ' ^');
lines.push('  --gpus all --runtime=nvidia --restart unless-stopped ^');
lines.push('  --shm-size 64m ^');
for (const p of Object.keys(c.HostConfig.PortBindings || {})) {
  const hostPort = c.HostConfig.PortBindings[p][0].HostPort;
  lines.push('  -p ' + hostPort + ':' + p.replace('/tcp', '') + ' ^');
}
for (const b of binds) {
  lines.push('  -v "' + b + '" ^');
}
for (const e of env) {
  lines.push('  -e "' + e.replace(/"/g, '') + '" ^');
}
lines.push('  ' + NEW_IMAGE);
lines.push('endlocal');
lines.push('exit /b 0');

fs.writeFileSync(OUT, lines.join('\r\n') + '\r\n', 'utf8');
console.log('written ' + OUT + ' for ' + env.length + ' env vars, binds=' + binds.length + ', image=' + NEW_IMAGE);
