// gguf-kv-fact-r213.test.js — R213 (2026-10-21).
//
// Живая жалоба оператора: «везде в WebUI настройках стоит квантизация q4_0, а в
// отображении загруженных параметров — f16».
//
// Причина была не в модели: рендер карточки печатал ДОГАДКУ как факт
// (`value: kv !== null ? kv : 'f16'`), потому что /api/v1/gguf/backends не
// отдавал kv_cache_type загруженного инстанса вовсе. ровно так же врал список
// файлов в настройках («kv=f16»).
//
// Тест фиксирует контракт R213:
//   1) балансер отдаёт kv_cache_type в /api/v1/gguf/backends;
//   2) карточка НЕ подставляет f16 — при отсутствии факта показывает прочерк или
//      значение профиля с явной пометкой;
//   3) список файлов в настройках не печатает «kv=f16» при отсутствии значения.
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const renderJs = fs.readFileSync(path.join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
const paramsJs = fs.readFileSync(path.join(__dirname, 'cppworker-params.js'), 'utf8');
const goHandler = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'api', 'gguf_backends_handler.go'), 'utf8');

check('балансер отдаёт факт kv_cache_type загруженной модели', function () {
  assert.ok(/KvCacheType\s+string\s+`json:"kv_cache_type,omitempty"`/.test(goHandler),
    'ggufBgModelInfo без поля kv_cache_type — карточка снова начнёт выдумывать значение');
  assert.ok(/KvCacheType:\s+m\.KvCacheType/.test(goHandler),
    'поле объявлено, но не заполняется из метрик — значение всегда пустое');
});

check('карточка модели не подставляет f16 вместо неизвестного значения', function () {
  assert.ok(!/value:\s*kv\s*!==\s*null\s*\?\s*String\(kv\)\s*:\s*'f16'/.test(renderJs),
    'найден старый враный фолбэк `: \'f16\'` — оператор снова увидит f16 при q4_0 в настройках');
  assert.ok(/let kvValue = '—';/.test(renderJs),
    'нет честного прочерка по умолчанию для неизвестного KV-cache');
});

check('карточка берёт факт инстанса и показывает источник', function () {
  assert.ok(/const kvFact = pick\(factOf\(m, \['kv_cache_type', 'kvCacheType'\]\)/.test(renderJs),
    'чип kv не читает факт инстанса (kv_cache_type)');
  assert.ok(/kvValue = kvProfile \+ ' \(профиль\)';/.test(renderJs),
    'при отсутствии факта значение профиля должно показываться С ПОМЕТКОЙ «профиль», а не как факт');
  assert.ok(/tone: kvTone/.test(renderJs), 'чип kv потерял тон (warn при неизвестном значении)');
});

check('список файлов в настройках не печатает kv=f16 при отсутствии значения', function () {
  assert.ok(!/'kv=f16'/.test(paramsJs),
    'cppworker-params.js всё ещё печатает kv=f16 как значение по умолчанию');
  assert.ok(/'kv=—'/.test(paramsJs), 'нет честного прочерка kv=—');
});

console.log('\nOK: ' + checks + ' проверок пройдено');
