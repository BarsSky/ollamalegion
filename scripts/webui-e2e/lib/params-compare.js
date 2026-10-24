// scripts/webui-e2e/lib/params-compare.js — R148 (2026-10-16).
//
// ЧИСТАЯ логика инструмента проверки параметров, вынесенная из params-e2e.js,
// чтобы её можно было покрыть тестами. Здесь собраны ровно те места, где
// инструмент УЖЕ ВРАЛ на живом стенде (каждый пункт — реальный дефект):
//
//   1. uptimeSeconds — uptime воркера приходил строкой ("1h51m9.76s"), и проверка
//      «процесс не перезапускался» сравнивала строки ЛЕКСИКОГРАФИЧЕСКИ:
//      "1h51m58s" >= "1h51m9s" давало false ('5' < '9'), и исправный воркер
//      объявлялся перезапущенным.
//   2. selectLoadedRow — пока модель грузится, балансер отдаёт ПЛЕЙСХОЛДЕР
//      (state=loading, context_size=0, n_layers=0). Сравнение с ним давало ложные
//      FAIL сразу после применения параметров.
//   3. mapRuntimeRow — факты берутся из loaded_models; раньше инструмент читал
//      ключ models/data, список был пуст, и он МОЛЧА уходил на запасной источник
//      (воркерский /api/models), где нет gpu_layers/flash_attn/use_mmap — и
//      объявлял это «воркер не сообщает».
//   4. compareWanted/normalize — два поля имеют другую природу, чем в карточке:
//      numGpuLayers=-1 это «все слои (авто)» (воркер сообщает фактическое число,
//      memfit может понизить его), flashAttn — три-состояние (-1 авто / 0 / 1).
//      Прямое сравнение давало и ложный PASS (flashAttn), и ложный FAIL
//      (numGpuLayers), и молчаливый PASS на профиле.
'use strict';

// uptimeSeconds — "1h51m9.765022814s" -> секунды. null, если разобрать нельзя.
function uptimeSeconds(v) {
    if (v === undefined || v === null) return null;
    if (typeof v === 'number') return v;
    const s = String(v).trim();
    const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(s);
    if (!m || (!m[1] && !m[2] && !m[3])) return null;
    return (Number(m[1] || 0) * 3600) + (Number(m[2] || 0) * 60) + Number(m[3] || 0);
}

// baseName — имя без расширения .gguf: файл на диске и инстанс называются по-разному.
function baseName(v) {
    return String(v === undefined || v === null ? '' : v).replace(/\.gguf$/, '');
}

// selectLoadedRow — выбрать в списке runtime-конфига строку ЗАГРУЖЕННОГО инстанса.
// Предпочитаем state=loaded с непустым окном; плейсхолдер загрузки не годится.
//
// R149 (2026-10-16): третий аргумент — backendID, к которому относится проверка.
// ЖИВОЙ СЛУЧАЙ: когда одну и ту же модель держат ДВЕ копии (мастер и донор),
// балансер отдаёт в loaded_models обе строки, и инструмент брал первую попавшуюся.
// Сравнение факта ДОНОРА с параметрами, применёнными в карточке МАСТЕРА, дало
// ложный FAIL (contextPerSeq 8192 против 4096, parallel 1 против 2). Теперь при
// известном backendID выбирается его строка, а неоднозначность (несколько копий)
// возвращается наружу, чтобы её можно было показать оператору, а не прятать.
function selectLoadedRow(list, model, backendID) {
    const base = baseName(model);
    let matches = (list || []).filter(m => baseName(m && m.name) === base);
    const wanted = String(backendID || '').trim();
    if (wanted) {
        const own = matches.filter(m => String(m.backend || '') === wanted);
        if (own.length) matches = own;
    }
    const row = matches.find(m => String(m.state || '').toLowerCase() === 'loaded' && Number(m.context_size) > 0)
        || matches.find(m => String(m.state || '').toLowerCase() === 'loaded')
        || null;
    const loadedBackends = Array.from(new Set(matches
        .filter(m => String(m.state || '').toLowerCase() === 'loaded')
        .map(m => String(m.backend || '?'))));
    if (!row) {
        const states = matches.map(m => String(m.state || '?')).join(',') || 'нет записи';
        return { row: null, states: states, backends: loadedBackends, ambiguous: false, backendMatched: false };
    }
    const rowBackend = String(row.backend || '');
    return {
        row: row,
        states: String(row.state || '?'),
        backend: rowBackend,
        backends: loadedBackends,
        ambiguous: loadedBackends.length > 1,
        // R151: совпала ли копия с ЗАПРОШЕННЫМ бэкендом. Если нет — сравнение не
        // авторитетно, и инструмент обязан сказать это прямо, а не выдать вердикт по
        // чужой копии. Живой случай: просили копию мастера, факт пришёл от донора,
        // значения совпали случайно, и проверка «прошла».
        backendMatched: !wanted || rowBackend === wanted
    };
}

// mapRuntimeRow — строка runtime-конфига -> факты для сверки (camelCase карточки).
function mapRuntimeRow(row) {
    if (!row) return null;
    return {
        contextLength: row.context_size,
        contextPerSeq: row.context_per_seq,
        numGpuLayers: row.gpu_layers,
        // nLayers — чтобы отличить «все слои на GPU» от частичной выгрузки.
        nLayers: row.n_layers,
        // Раскладка слоёв по устройствам: объясняет, что именно ушло в CPU.
        deviceLayers: row.device_layers_exact || row.device_layers,
        batchSize: row.batch_size,
        parallel: row.parallel,
        kvCacheType: row.kv_cache_type,
        // Сырое три-состояние воркера (-1 авто / 0 выкл / 1 вкл).
        flashAttnRaw: row.flash_attn,
        flashAttn: row.flash_attn,
        useMmap: row.use_mmap,
        enableReasoning: row.reasoning_enabled
    };
}

// normalize — привести значение к сравнимому виду с учётом природы поля.
function normalize(field, v) {
    if (v === undefined || v === null) return null;
    if (field === 'flashAttn') {
        // Три-состояние: -1 = авто. Приводить -1 к true нельзя — это давало
        // ЛОЖНЫЙ PASS на запрос flashAttn=true, хотя инстанс сообщал «авто».
        if (typeof v === 'number') return v < 0 ? null : v !== 0;
        const s = String(v).toLowerCase();
        if (s === 'auto' || s === '-1') return null;
        if (s === 'true' || s === '1') return true;
        if (s === 'false' || s === '0') return false;
        return v;
    }
    if (field === 'useMmap' || field === 'enableReasoning') {
        if (typeof v === 'boolean') return v;
        if (typeof v === 'number') return v !== 0;
        const s = String(v).toLowerCase();
        if (s === 'true' || s === '1') return true;
        if (s === 'false' || s === '0') return false;
        return v;
    }
    const n = Number(v);
    return Number.isFinite(n) ? n : v;
}

// compareWanted — что просили против того, что применилось.
//
// Три категории, и их нельзя смешивать (иначе инструмент врёт):
//   * diffs       — воркер сообщил ДРУГОЕ значение: дефект применения;
//   * unreported  — воркер не сообщил/сообщил «авто»: пробел наблюдаемости;
//   * adaptations — значение применено, но АДАПТИРОВАНО под память (memfit),
//                   с числами: не FAIL и не молчание.
function compareWanted(wantedObj, factObj) {
    const diffs = [];
    const unreported = [];
    const adaptations = [];
    for (const [k, want] of Object.entries(wantedObj)) {
        const got = normalize(k, factObj[k]);
        const exp = normalize(k, want);
        if (k === 'numGpuLayers' && typeof exp === 'number' && exp < 0 &&
            typeof got === 'number' && got >= 0) {
            // Ветка только для ФАКТА загрузки (неотрицательное число слоёв).
            // Если и в факте -1 (сравниваем профиль) — обычное совпадение.
            const n = factObj.nLayers;
            if (typeof n === 'number' && n > 0 && got >= n) continue; // все слои на GPU
            let split = '';
            if (Array.isArray(factObj.deviceLayers) && factObj.deviceLayers.length) {
                split = ' [' + factObj.deviceLayers.map(d => `${d.device}: ${d.count}`).join(', ') + ']';
            }
            adaptations.push(`numGpuLayers: запрошено ${exp} (все слои)` +
                (typeof n === 'number' && n > 0 ? `, на GPU ушло ${got} из ${n}` : `, на GPU ушло ${got}`) +
                ` — адаптация под свободную память (partial_offload)${split}`);
            continue;
        }
        if (k === 'flashAttn' && got === null && exp !== null) {
            unreported.push(`flashAttn (просили ${JSON.stringify(exp)}, воркер сообщает ` +
                `${JSON.stringify(factObj.flashAttnRaw)} = «авто» — не подтверждение)`);
            continue;
        }
        if (got === null) { unreported.push(`${k} (ждём ${JSON.stringify(exp)})`); continue; }
        if (got !== exp) diffs.push(`${k}: применилось ${JSON.stringify(got)}, ждали ${JSON.stringify(exp)}`);
    }
    return { diffs: diffs, unreported: unreported, adaptations: adaptations };
}

// pickNewestPrompt — самый свежий снапшот промпта, снятый ПОСЛЕ начала запроса.
//
// R150 (2026-10-16). ЗАЧЕМ. Одну модель могут держать несколько копий (мастер и
// донор), и запрос через балансер обслуживает ЛЮБАЯ из них. Сверять паритет
// промпта по одному заранее выбранному воркеру нельзя: если ответил другой,
// сравниваются промпты РАЗНЫХ запросов. Поэтому читаем last-prompt у всех воркеров
// и берём тот, чей timestamp не раньше начала нашего запроса (и самый поздний из
// таких). Пусто -> ни один воркер не подтвердил запрос, и это честный «не знаю»,
// а не повод выдать вердикт.
function pickNewestPrompt(snapshots, sinceMs) {
    let best = null;
    let bestAt = -1;
    for (const s of (snapshots || [])) {
        if (!s || !s.timestamp) continue;
        const at = Date.parse(s.timestamp);
        if (!Number.isFinite(at)) continue;
        if (at + 2000 < sinceMs) continue; // снапшот старше нашего запроса (запас 2 с на рассинхрон часов)
        if (at > bestAt) { bestAt = at; best = s; }
    }
    return best;
}

// pickPromptWithMarker — снапшот промпта, который ПРИНАДЛЕЖИТ нашему запросу.
//
// R150. «Самый свежий снапшот» оказался ненадёжным: при нескольких копиях модели и
// параллельных запросах окно в пару секунд пропускает снапшот ЧУЖОГО запроса (живой
// случай: сверяли 80 символов против 345 — это был промпт предыдущего кейса с
// инструментами). Поэтому в текст запроса добавляется уникальная метка, и промпт
// берётся только тот, где эта метка реально видна (воркер отдаёт начало и конец
// промпта — prompt_head/prompt_tail).
// pickPrompt — снапшот промпта НАШЕГО запроса (по метке) с выбором способа отбора.
//
// R150. Метка одинакова для пары «напрямую + через балансер» (тексты обязаны быть
// идентичны), поэтому одна лишь метка не различает два вызова. Различаем способом:
//   * mode='first'  — прямой вызов: его обслужил КОНКРЕТНЫЙ воркер, читаем сразу
//     после ответа, поэтому годится первый снапшот с меткой;
//   * mode='newest' — вызов через балансер: обслужить могла любая копия, и нужен
//     САМЫЙ СВЕЖИЙ снапшот с меткой (живой дефект: брали снапшот прямого вызова —
//     символы совпадали, а токены различались на 144, то есть на преамбулу
//     инструментов, которую добавил балансер).
function pickPrompt(snapshots, marker, mode) {
    const m = String(marker || '').trim();
    if (!m) return null;
    const hits = (snapshots || []).filter(s => s &&
        (String(s.prompt_head || '').includes(m) || String(s.prompt_tail || '').includes(m)));
    if (!hits.length) return null;
    if (mode === 'newest') {
        return hits.reduce((a, b) => (Date.parse(b.timestamp || 0) > Date.parse(a.timestamp || 0) ? b : a));
    }
    return hits[0];
}

function pickPromptWithMarker(snapshots, marker) {
    return pickPrompt(snapshots, marker, 'first');
}

module.exports = {
    uptimeSeconds: uptimeSeconds,
    baseName: baseName,
    selectLoadedRow: selectLoadedRow,
    mapRuntimeRow: mapRuntimeRow,
    normalize: normalize,
    compareWanted: compareWanted,
    pickNewestPrompt: pickNewestPrompt,
    pickPromptWithMarker: pickPromptWithMarker,
    pickPrompt: pickPrompt
};
