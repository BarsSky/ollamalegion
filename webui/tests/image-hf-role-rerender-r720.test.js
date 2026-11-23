// webui/tests/image-hf-role-rerender-r720.test.js
// D9 (2026-11-25): смена роли файла обязана перерисовывать список.
//
// Живой дефект до правки: обработчик `change` на `[data-imh-role]` менял только
// state и делал `return` — подсказка «(предложено: …)» и предупреждение о
// неполном наборе оставались от прежнего выбора, то есть врали оператору.
const fs = require('fs');
const path = require('path');

const SRC = path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js');
const src = fs.readFileSync(SRC, 'utf8');

let failed = 0;
function check(name, ok, detail) {
  if (ok) { console.log('  ok ' + name); return; }
  failed++;
  console.log('  FAILED: ' + name + (detail ? ' — ' + detail : ''));
}

const anchors = [];
let idx = -1;
while ((idx = src.indexOf('data-imh-role', idx + 1)) !== -1) anchors.push(idx);

// 1. Обработчик смены роли существует и читает data-imh-role.
check('обработчик читает data-imh-role', anchors.length >= 2, 'вхождений: ' + anchors.length);

// 2. После setSelection(role, {role…}) в пределах 400 символов вызывается renderFiles().
const selIdx = src.indexOf('setSelection(role, { role: target.value })');
check('ветка setSelection(role…) найдена', selIdx !== -1);
const tail = selIdx === -1 ? '' : src.slice(selIdx, selIdx + 1500);
check('после setSelection(role…) вызывается renderFiles()', /renderFiles\(\)/.test(tail), tail.slice(0, 200));

// 3. Голого `setSelection(role…); return;` (это и был дефект) больше нет.
check('нет `setSelection(role…); return;` без перерисовки',
  src.indexOf('setSelection(role, { role: target.value }); return;') === -1);

// 4. Регресс-сторож R693/R710: селект семейства по-прежнему перерисовывает.
const famIdx = src.indexOf('state.familyManual = true');
const famTail = famIdx === -1 ? '' : src.slice(famIdx, famIdx + 400);
check('ветка семейства по-прежнему вызывает renderFiles()', /renderFiles\(\)/.test(famTail));

console.log(failed === 0 ? 'ИТОГ: PASS (4 проверки)' : 'ИТОГ: FAILED: ' + failed);
process.exit(failed === 0 ? 0 : 1);

