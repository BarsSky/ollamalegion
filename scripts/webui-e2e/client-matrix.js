// client-matrix.js — R146 (2026-10-16): проверка отсутствия деградации для
// реальных клиентских профилей через балансер.
//
// ЗАЧЕМ. Пункт «проверить отсутствие деградации для Cline/deepseek harness/hermes».
// Клиенты ходят двумя разными поверхностями и в двух режимах:
//   * OpenAI-совместимая  /v1/chat/completions  (deepseek harness, pi-ai, hermes);
//   * Ollama-нативная     /api/chat            (Cline, Open WebUI);
//   * stream=false и stream=true;
//   * со своими tools[] и без них.
//
// ЧТО ПРОВЕРЯЕТСЯ, и почему именно так:
//   1. ПАРИТЕТ ПРОМПТА. Воркер хранит фактический промпт последнего запроса
//      (GET /api/v1/cppworker/debug/last-prompt). Если через балансер воркер видит
//      ДРУГОЙ промпт (длиннее/с tools), клиент получает другой ответ и другую
//      цену по токенам — это и есть деградация. Живой случай, ради которого
//      проверка появилась: безусловная инъекция image-инструмента добавляла
//      ~201 токен преамбулы на КАЖДЫЙ запрос (R141).
//   2. ЦЕЛОСТНОСТЬ ФОРМЫ ОТВЕТА. object=chat.completion (а не .chunk) в не-стриме,
//      наличие usage и duration_ms, терминальный чанк в стриме. Живой случай: R142.
//   3. ИДЕНТИЧНОСТЬ УЧЁТА ТОКЕНОВ: prompt_tokens/completion_tokens через балансер
//      должны совпадать с прямым вызовом (при одинаковом промпте).
//
// Запуск: node scripts/webui-e2e/client-matrix.js [--model имя] [--client-url ...]
// Код возврата: 0 — все проверки прошли, 1 — есть провалы.

const API_TOKEN = 'changeme-bundled-with-agent-token';
// R150: помощник выбора снапшота промпта (покрыт тестами в tests/params-compare.test.js)
const pc = require('./lib/params-compare.js');
const WORKER = process.env.WORKER_URL || 'http://127.0.0.1:18092';
const BALANCER = process.env.CLIENT_URL || 'http://127.0.0.1:18080';

const argv = process.argv.slice(2);
function argVal(name, dflt) {
    const i = argv.indexOf('--' + name);
    return i >= 0 && argv[i + 1] ? argv[i + 1] : dflt;
}
const MODEL = argVal('model', 'gemma-4-E4B-it-Q4_K_M');

const H = { 'X-API-Token': API_TOKEN, 'Content-Type': 'application/json' };

const DUMMY_TOOL = {
    type: 'function',
    function: {
        name: 'get_weather',
        description: 'Узнать погоду в городе',
        parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] },
    },
};

let failures = 0;
const results = [];

function check(ok, label, detail) {
    if (!ok) failures++;
    results.push({ ok, label, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'}  ${label}${detail ? ' — ' + detail : ''}`);
}

// R150 (2026-10-16): воркеров может быть НЕСКОЛЬКО (мастер + донор), и запрос через
// балансер обслуживает любая из копий модели. Паритет промпта обязан сверяться с
// тем воркером, который РЕАЛЬНО ответил, иначе сравниваются промпты разных запросов.
// Список задаётся так: --workers "http://127.0.0.1:18092,http://192.0.2.10:18092".
const WORKERS = argVal('workers', WORKER).split(',').map(s => s.trim()).filter(Boolean);

async function promptFrom(workerURL) {
    try {
        const r = await fetch(workerURL + '/api/v1/cppworker/debug/last-prompt', { headers: H });
        if (r.status !== 200) return null;
        const j = await r.json();
        j.__worker = workerURL; // чтобы в отчёте было видно, ЧЕЙ это снапшот
        return j;
    } catch (e) {
        return null;
    }
}

// allPrompts — снапшоты промпта со всех воркеров (недоступные пропускаем).
async function allPrompts() {
    const out = [];
    for (const w of WORKERS) {
        const p = await promptFrom(w);
        if (p) out.push(p);
    }
    return out;
}

// withMarker — вписать уникальную метку в текст запроса.
//
// R150: метка нужна, чтобы найти ИМЕННО свой снапшот промпта у воркера (при двух
// копиях модели и параллельных запросах «самый свежий» может оказаться чужим).
// Метка одинакова для прямого вызова и для вызова через балансер, поэтому паритет
// промпта остаётся сравнением равного с равным.
function withMarker(body, marker) {
    const clone = JSON.parse(JSON.stringify(body));
    if (Array.isArray(clone.messages)) {
        for (let i = clone.messages.length - 1; i >= 0; i--) {
            if (clone.messages[i] && clone.messages[i].role === 'user') {
                clone.messages[i].content = marker + ' ' + String(clone.messages[i].content || '');
                break;
            }
        }
    }
    return clone;
}

// mkMarker — метка запроса. ОДНА на пару «напрямую + через балансер»: иначе тексты
// запросов различаются, и сравнение перестаёт быть сравнением равного с равным
// (живой случай: метки разной длины/состава дали равные символы при разном числе
// токенов — 36 против 37).
function mkMarker() {
    return 'probe-r150-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 7);
}

// Один запрос: возвращает статус, время, сырой текст, разобранный JSON и (для
// стрима) число чанков + признак терминального чанка.
async function call(base, path, body, marker, pickMode, readOnlyWorker) {
    const t0 = Date.now();
    marker = marker || mkMarker();
    const marked = withMarker(body, marker);
    let r;
    try {
        r = await fetch(base + path, { method: 'POST', headers: H, body: JSON.stringify(marked) });
    } catch (e) {
        // R146: сетевой сбой (в т.ч. UND_ERR_BODY_TIMEOUT на зависшем стриме) не
        // должен убивать всю матрицу — это ДАННЫЕ о конкретном случае.
        return { status: 0, ms: Date.now() - t0, text: 'FETCH ERROR: ' + e.message, json: null, chunks: 0, terminal: false, lp: null, marker };
    }
    const streaming = body.stream === true;
    let text = '';
    let chunks = 0;
    let terminal = false;
    try {
        if (streaming) {
            const reader = r.body.getReader();
            const dec = new TextDecoder();
            while (true) {
                const { done, value } = await reader.read();
                if (done) break;
                text += dec.decode(value, { stream: true });
                chunks++;
                if (text.includes('[DONE]') || text.includes('"done":true')) terminal = true;
            }
        } else {
            text = await r.text();
        }
    } catch (e) {
        text += '\nSTREAM ERROR: ' + e.message;
    }
    let json = null;
    try { json = JSON.parse(text); } catch (e) { /* стрим — не JSON целиком */ }
    // R150: промпт берём у ТОГО воркера, который ответил, и ТОЛЬКО если в нём видна
    // наша метка (иначе это снапшот чужого запроса). Нет метки -> lp = null, и
    // матрица честно скажет «паритет не проверен», а не сравнит чужие запросы.
    const snaps = readOnlyWorker ? [await promptFrom(readOnlyWorker)].filter(Boolean) : await allPrompts();
    const lp = pc.pickPrompt(snaps, marker, pickMode || 'first');
    return { status: r.status, ms: Date.now() - t0, text, json, chunks, terminal, lp, marker };
}

function usageOf(r) {
    if (r.json && r.json.usage) return r.json.usage;
    if (r.json && typeof r.json.prompt_eval_count === 'number') {
        return { prompt_tokens: r.json.prompt_eval_count, completion_tokens: r.json.eval_count };
    }
    // Стрим: ищем usage в последнем чанке (OpenAI) или eval_count (Ollama NDJSON).
    const m = r.text.match(/"prompt_tokens"\s*:\s*(\d+)[\s\S]*?"completion_tokens"\s*:\s*(\d+)/);
    if (m) return { prompt_tokens: Number(m[1]), completion_tokens: Number(m[2]) };
    const o = r.text.match(/"prompt_eval_count"\s*:\s*(\d+)[\s\S]*?"eval_count"\s*:\s*(\d+)/);
    if (o) return { prompt_tokens: Number(o[1]), completion_tokens: Number(o[2]) };
    return null;
}

const CASES = [
    {
        name: 'OpenAI не-стрим, БЕЗ tools (deepseek harness/pi-ai)',
        path: '/v1/chat/completions', tools: false,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, max_tokens: 6 }),
        shape: 'openai-nonstream',
    },
    {
        name: 'OpenAI не-стрим, СО СВОИМИ tools (агент с инструментами)',
        path: '/v1/chat/completions', tools: true,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, max_tokens: 6, tools: [DUMMY_TOOL] }),
        shape: 'openai-nonstream',
    },
    {
        name: 'OpenAI стрим, БЕЗ tools (hermes/harness streaming)',
        path: '/v1/chat/completions', tools: false,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: true, max_tokens: 6 }),
        shape: 'openai-stream',
    },
    {
        name: 'Ollama /api/chat не-стрим, БЕЗ tools (Cline/OpenWebUI)',
        path: '/api/chat', tools: false,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 6 } }),
        shape: 'ollama-nonstream',
    },
    {
        name: 'Ollama /api/chat стрим, БЕЗ tools',
        path: '/api/chat', tools: false,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: true, options: { num_predict: 6 } }),
        shape: 'ollama-stream',
    },
    {
        name: 'Ollama /api/chat не-стрим, СО СВОИМИ tools',
        path: '/api/chat', tools: true,
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 6 }, tools: [DUMMY_TOOL] }),
        shape: 'ollama-nonstream',
    },
    // R147 (2026-10-16): ПРЕФИКСНАЯ нативная поверхность. Живой факт: обычный
    // /api/chat идёт общим прокси-потоком и инструмент НЕ объявляет (промпт
    // байт-в-байт), а /ollama/api/chat нормализуется в LlamaCppRouter и объявляет
    // (+144 токена, в логе injected:true). Клиенту, которому нужны картинки по
    // нативной поверхности, нужен именно префиксный путь — см.
    // docs/features/image-generation.ru.md.
    {
        name: 'Ollama /ollama/api/chat не-стрим, СО СВОИМИ tools (префиксный путь)',
        path: '/ollama/api/chat', tools: true,
        // Префикс /ollama/ — удобство БАЛАНСЕРА (нормализация в LlamaCppRouter); у
        // самого воркера такого пути нет (живой 404). Поэтому прямой вызов для
        // сравнения идёт на обычный /api/chat с тем же телом.
        directPath: '/api/chat',
        body: () => ({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 6 }, tools: [DUMMY_TOOL] }),
        shape: 'ollama-nonstream',
    },
];

// reportUsage — учёт токенов: промпт сверяем СТРОГО, генерацию — сообщаем.
//
// Почему не строго по completion_tokens: генерация стохастична, и два последовательных
// запроса с ОДИНАКОВЫМ промптом дают разное число токенов (живой пример: 2 против 4
// при совпадающем промпте). Требовать равенства — значит объявлять дефектом
// семплирование. Промпт же при одинаковом входе детерминирован, поэтому расхождение
// в prompt_tokens — это реальный признак, что балансер изменил запрос.
function reportUsage(direct, via, promptAugmented) {
    const du = usageOf(direct), vu = usageOf(via);
    if (!du || !vu) {
        console.log('  INFO  usage недоступен в одном из ответов — учёт токенов не сверялся');
        return;
    }
    if (promptAugmented) {
        console.log('  INFO  prompt_tokens не сверяем: балансер объявил свой инструмент, промпт законно больше');
        return;
    }
    check(du.prompt_tokens === vu.prompt_tokens, 'prompt_tokens совпадает с прямым вызовом',
        `напрямую ${du.prompt_tokens}, через балансер ${vu.prompt_tokens}` +
        (du.prompt_tokens !== vu.prompt_tokens ? ` (разница ${vu.prompt_tokens - du.prompt_tokens})` : ''));
    if (du.completion_tokens !== vu.completion_tokens) {
        console.log(`  INFO  completion_tokens различаются (${du.completion_tokens} против ` +
            `${vu.completion_tokens}) — генерация стохастична, это не дефект учёта`);
    }
}

(async () => {
    console.log(`Модель: ${MODEL}`);
    console.log(`Прямо: ${WORKER}   Через балансер: ${BALANCER}\n`);

    for (const c of CASES) {
        console.log(`=== ${c.name} ===`);
        // ОДНА метка на оба вызова: тексты должны быть идентичны, иначе различие
        // токенов будет нашим собственным, а не свойством балансера.
        const marker = mkMarker();
        // Прямой вызов обслуживает КОНКРЕТНЫЙ воркер — читаем его снапшот; вызов
        // через балансер мог обслужить любую копию — берём самый свежий с меткой
        // (иначе можно взять снапшот прямого вызова: символы совпадут, а токены
        // разойдутся на преамбулу инструментов — живой дефект).
        const direct = await call(WORKER, c.directPath || c.path, c.body(), marker, 'first', WORKER);
        const via = await call(BALANCER, c.path, c.body(), marker, 'newest');

        // R150: показываем, ЧЕЙ промпт сверяем — при нескольких копиях модели ответ
        // может дать любая, и это ключевая информация при разборе.
        if (via.lp) {
            console.log('  INFO  через балансер обслужил: ' + (via.lp.__worker || '?') +
                (via.lp.__worker !== WORKER ? ' (не тот воркер, что прямой вызов)' : ''));
        }

        check(direct.status === 200 && via.status === 200, 'оба вызова прошли',
            `напрямую ${direct.status}, через балансер ${via.status}`);

        let promptAugmented = false; // балансер добавил СВОЙ инструмент (фича, не деградация)

        // 1. Паритет промпта.
        //
        // РАЗНЫЕ ОЖИДАНИЯ ДЛЯ РАЗНЫХ КЛИЕНТОВ — это принципиально.
        //   * Клиент БЕЗ tools: промпт обязан дойти байт-в-байт. Любое расхождение
        //     — деградация (живой случай: +201 токен преамбулы инструментов, R141).
        //   * Клиент СО СВОИМИ tools: балансер на OpenAI-пути добавляет наш
        //     image-инструмент — это ФИЧА (R84/R86), а не деградация, и в режиме
        //     auto она применяется именно к таким клиентам. Поэтому расхождение
        //     здесь печатается как INFO с числами, а FAIL только если промпт
        //     оказался КОРОЧЕ прямого (значит потеряли содержимое клиента).
        //     Отключается фича целиком: LB_IMAGE_TOOL=off.
        if (direct.lp && via.lp) {
            check(!!direct.lp.has_tools === !!via.lp.has_tools,
                'наличие инструментов не подменяется',
                `напрямую has_tools=${direct.lp.has_tools}, через балансер has_tools=${via.lp.has_tools}`);
            const dChars = direct.lp.prompt_chars, vChars = via.lp.prompt_chars;
            const dTok = direct.lp.prompt_tokens, vTok = via.lp.prompt_tokens;
            if (c.tools) {
                check(typeof dChars === 'number' && typeof vChars === 'number' && vChars >= dChars,
                    'промпт клиента не потерян (наша инъекция допустима)',
                    `символов: напрямую ${dChars}, через балансер ${vChars}` +
                    (vChars !== dChars ? ` (+${vChars - dChars}: объявлен image-инструмент)` : ''));
                if (vChars !== dChars) {
                    promptAugmented = true;
                    console.log(`  INFO  балансер добавил инструмент генерации изображений: ` +
                        `${dTok} → ${vTok} токенов промпта (фича R84/R86; LB_IMAGE_TOOL=off выключает)`);
                }
            } else {
                check(dChars === vChars, 'промпт доходит БЕЗ изменений',
                    `символов: напрямую ${dChars}, через балансер ${vChars}`);
                if (typeof dTok === 'number' && typeof vTok === 'number') {
                    check(dTok === vTok, 'токенов в промпте столько же',
                        `напрямую ${dTok}, через балансер ${vTok}${dTok !== vTok ? ' (разница ' + (vTok - dTok) + ')' : ''}`);
                }
            }
        } else {
            console.log('  WARN  не удалось прочитать last-prompt воркера — паритет промпта не проверен');
        }

        // 2. Целостность формы ответа.
        if (c.shape === 'openai-nonstream') {
            const o = via.json && via.json.object;
            check(o === 'chat.completion', 'object = chat.completion (не .chunk)',
                'получено ' + JSON.stringify(o));
            check(!!(via.json && via.json.usage), 'usage присутствует',
                JSON.stringify(via.json && via.json.usage));
            check(typeof (via.json && via.json.duration_ms) === 'number', 'duration_ms присутствует',
                'получено ' + JSON.stringify(via.json && via.json.duration_ms));
            reportUsage(direct, via, promptAugmented);
        } else if (c.shape === 'openai-stream') {
            check(via.chunks > 0 && via.terminal, 'стрим дошёл целиком',
                `чанков ${via.chunks}, терминальный ${via.terminal}`);
            check(/\bdata:\s*\{/.test(via.text), 'формат SSE сохранён', 'есть data:-чанки');
        } else if (c.shape === 'ollama-nonstream') {
            check(!!(via.json && via.json.message), 'ответ Ollama-формы (message)', 'поля: ' +
                (via.json ? Object.keys(via.json).join(',') : 'нет JSON'));
            reportUsage(direct, via, promptAugmented);
        } else if (c.shape === 'ollama-stream') {
            check(via.chunks > 0, 'стрим дошёл', `чанков ${via.chunks}`);
            check(/"done":true/.test(via.text), 'есть терминальный чанк done:true', '');
        }
        console.log('');
    }

    console.log('=== ИТОГ ===');
    console.log(`  проверок: ${results.length}, провалов: ${failures}`);
    if (failures) {
        console.log('  НЕ ПРОШЛИ:');
        results.filter(r => !r.ok).forEach(r => console.log(`   - ${r.label}: ${r.detail}`));
    }
    process.exit(failures ? 1 : 0);
})();
