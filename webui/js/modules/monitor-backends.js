/**
 * Monitor Backends Module
 * Backend table rendering with sorting and filtering
 * Used by: monitor.html
 * Dependencies: MonitorApp (MA)
 */

const MonitorBackends = (() => {
    'use strict';

    const MA = window.MonitorApp || {};
    let currentSort = { column: 'id', asc: true };
    let currentFilter = '';

    // ===== R185 (2026-10-20): «архитектура такого-то бэкенда не поддерживается» =====
    //
    // ЖИВОЙ РАЗБОР. cppworker был собран под CUDA_ARCH=86;120, а карта — Quadro
    // RTX 4000 (sm_75). Загрузка модели «проходила», первое GPU-ядро убивало
    // процесс (no kernel image is available), балансер повторял загрузку, а UI
    // показывал только «не грузится» — причины оператор не видел.
    //
    // Что делаем: спрашиваем у воркера /api/info (через прокси балансера) и, если
    // cuda_arch_ok=false, выводим ПРЯМО НАД ТАБЛИЦЕЙ явное уведомление: какой
    // бэкенд, под что собран образ, что карта не поддерживается и что делать.
    // Опрос редкий (TTL), чтобы не дёргать воркер на каждой перерисовке.
    const archFacts = {};   // backendId -> { ok: bool, warning: string, built: string }
    const archProbeAt = {}; // backendId -> ms последнего опроса
    const ARCH_PROBE_TTL_MS = 60000;

    function backendTypeOf(b) {
        return String((b && (b.backendType || b.backend_type || b.type)) || '').toLowerCase();
    }

    // archProbeList — каких бэкендов не хватает в кэше (или пора переспросить).
    function archProbeList(backends) {
        const need = [];
        (backends || []).forEach(function (b) {
            if (!b || !b.id) return;
            if (backendTypeOf(b) !== 'llama_cpp') return; // у ollama/image архитектур нет
            const f = archFacts[b.id];
            if (f && f.ok) return; // совместимо — переспрашивать незачем
            if (Date.now() - (archProbeAt[b.id] || 0) < ARCH_PROBE_TTL_MS) return;
            archProbeAt[b.id] = Date.now();
            need.push(b.id);
        });
        return need;
    }

    // rememberArchInfo — сложить факты из /api/info. Старый образ полей не отдаёт:
    // тогда считаем «совместимо» и не тревожим оператора ложной тревогой.
    function rememberArchInfo(backendId, info) {
        if (!backendId || !info || typeof info !== 'object') return;
        if (info.cuda_arch_ok === undefined || info.cuda_arch_ok === null) {
            archFacts[backendId] = { ok: true, warning: '', built: '' };
            return;
        }
        const built = Array.isArray(info.cuda_arch_built) ? info.cuda_arch_built.join(';') :
            (info.cuda_arch_built ? String(info.cuda_arch_built) : '');
        archFacts[backendId] = {
            ok: info.cuda_arch_ok !== false,
            warning: String(info.cuda_arch_warning || ''),
            built: built
        };
    }

    // t() с русским фолбэком — как в остальном этом модуле: страница должна
    // показывать причину даже если i18n-модуль ещё не загрузился.
    function archT(key, fallback) {
        const i18n = (typeof window !== 'undefined') ? window.I18N : null;
        if (i18n && typeof i18n.t === 'function') {
            const s = i18n.t(key);
            if (s && s !== key) return s;
        }
        return fallback;
    }

    // archBannerHtml — чистая функция (тест: monitor-arch-r185.test.js).
    function archBannerHtml(rows) {
        const esc = (MA && typeof MA.esc === 'function') ? MA.esc : function (s) { return String(s == null ? '' : s); };
        const list = (rows || []).filter(function (r) { return r && r.backend; });
        if (!list.length) return '';
        const title = archT('monitor.arch.title', 'Архитектура бэкенда не поддерживается');
        const hint = archT('monitor.arch.hint',
            'Загрузка моделей ОТКЛОНЕНА — повторять бесполезно: пересоберите образ с CUDA_ARCH, ' +
            'включающим архитектуру карты (например 75;86;120), и перезапустите воркер.');
        return list.map(function (r) {
            const detail = r.warning ? r.warning : 'архитектура образа и карты не совпадают';
            const built = r.built ? (' образ собран под sm ' + r.built + '.') : '';
            const text = title + ': ' + esc(r.backend) + '. ' + esc(detail) + '.' + built + ' ' + hint;
            return '<div class="arch-warning" data-backend="' + esc(r.backend) + '" role="alert" ' +
                'title="' + esc(detail) + '" style="display:flex;gap:8px;align-items:flex-start;' +
                'margin:0 0 10px;padding:10px 12px;border-radius:6px;' +
                'background:rgba(239,68,68,0.12);border:1px solid rgba(239,68,68,0.45);' +
                'color:#f87171;font-size:var(--font-size-sm);line-height:1.45">' +
                '<span aria-hidden="true">⛔</span><span>' + text + '</span></div>';
        }).join('');
    }

    // renderArchBanner — перерисовать уведомление из накопленных фактов.
    function renderArchBanner(backends) {
        const rows = [];
        (backends || []).forEach(function (b) {
            const f = b && b.id ? archFacts[b.id] : null;
            if (f && f.ok === false) rows.push({ backend: b.id, warning: f.warning, built: f.built });
        });
        const html = archBannerHtml(rows);
        const host = (typeof document !== 'undefined' && document.getElementById) ? document.getElementById('backendsArchWarning') : null;
        if (host) {
            host.innerHTML = html;
            host.style.display = html ? '' : 'none';
        }
        return html;
    }

    // probeBackendArch — доопросить /api/info у тех бэкендов, чьё состояние протухло.
    function probeBackendArch(backends) {
        const api = (typeof window !== 'undefined') ? window.GgufApi : null;
        if (!api || typeof api.requestViaBackend !== 'function') return [];
        const ids = archProbeList(backends);
        ids.forEach(function (id) {
            Promise.resolve(api.requestViaBackend(id, '/api/info')).then(function (info) {
                rememberArchInfo(id, info);
            }).catch(function () {
                // Воркер недоступен / старый образ: молчим. Архитектуру в этом
                // случае назвать нельзя, а ложная тревога хуже молчания.
            }).then(function () {
                renderArchBanner(backends);
            });
        });
        return ids;
    }


    // ===== R118 (2026-10-14): роль бэкенда в раскладке слоёв =====
    //
    // ТРЕБОВАНИЕ ОПЕРАТОРА: «явное управление от балансера через WEBUI, чтобы можно
    // было в панели настроить режим для бэкенда, указав кто донор, а кто главный
    // или же самодостаточный». Здесь это и есть: селектор в строке узла.
    //
    // Значение уходит в PUT /api/v1/backends/{id}/role (балансер). Роли хранит и
    // применяет балансер: главному он сам пришлёт rpcEnabled + список адресов
    // доноров, поэтому оператору не нужно править .env и заходить на машины.
    // У image-воркеров раскладки нет — у них селектора не показываем.
    function roleSelect(b) {
        b = b || {};
        const type = String(b.backendType || b.backend_type || b.type || '').toLowerCase();
        if (type && type !== 'llama_cpp') return '';
        const t = function (key, fallback) {
            return (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function')
                ? window.I18N.t(key) : fallback;
        };
        const role = String(b.role || '');
        const opts = [
            { v: '', t: t('monitor.role.none', '— не задана —') },
            { v: 'standalone', t: t('monitor.role.standalone', 'самодостаточный') },
            { v: 'main', t: t('monitor.role.main', 'главный (раскладка)') },
            { v: 'donor', t: t('monitor.role.donor', 'донор (узел)') }
        ];
        return '<div style="margin-top:2px">' +
            '<select class="backend-role-select" data-backend="' + MA.esc(b.id) + '" ' +
                'title="' + MA.esc(t('monitor.role.title', 'Роль бэкенда в раскладке слоёв')) + '">' +
                opts.map(function (o) {
                    return '<option value="' + o.v + '"' + (o.v === role ? ' selected' : '') + '>' +
                        MA.esc(o.t) + '</option>';
                }).join('') +
            '</select></div>' + rpcToggle(b);
    }

    // ===== R229 (2026-10-22): тумблер раскладки RPC принадлежит ОПЕРАТОРУ =====
    //
    // ЖИВОЙ ДЕФЕКТ. Оператор выключал раскладку главного (rpcEnabled=false), воркер
    // применял, а балансерский гард EnsureRPCLayout через ~15 с возвращал
    // rpcEnabled=true: роль главного + живой донор означают «раскладка включена».
    // В панели это выглядело как «тумблер сам включается обратно», и следующая
    // загрузка снова уходила на донора.
    //
    // Теперь выбор уходит в PUT /api/v1/backends/{id}/rpc-layout, хранится в записи
    // бэкенда (переживает рестарт) и побеждает роль; «авто» возвращает решение
    // балансеру. Донору тумблер не показываем: донор — источник слоёв, свою
    // раскладку он не держит.
    function rpcToggle(b) {
        b = b || {};
        const type = String(b.backendType || b.backend_type || b.type || '').toLowerCase();
        if (type && type !== 'llama_cpp') return '';
        const role = String(b.role || '');
        if (role === 'donor') return '';
        const t = function (key, fallback) {
            return (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function')
                ? window.I18N.t(key) : fallback;
        };
        const mode = String(b.rpcEnabledOverride || 'auto');
        // Чекбокс показывает ЖЕЛАНИЕ: в «авто» — то, что решит роль.
        const checked = mode === 'on' ? true : (mode === 'off' ? false : role === 'main');
        const state = mode === 'on' ? t('monitor.rpc.state.on', 'вкл оператором')
            : (mode === 'off' ? t('monitor.rpc.state.off', 'выкл оператором')
                : t('monitor.rpc.state.auto', 'авто (по ролям)'));
        const applyTitle = t('monitor.rpc.apply.title',
            'Применить выбор: включить/выключить раскладку. Балансер не будет его перезаписывать');
        const autoTitle = t('monitor.rpc.auto.title', 'Вернуть раскладку под управление ролями');
        return '<div style="margin-top:2px;display:flex;gap:4px;align-items:center">' +
            '<label class="backend-rpc-toggle" title="' + MA.esc(applyTitle) + '">' +
                '<input type="checkbox" class="backend-rpc-enabled" data-backend="' + MA.esc(b.id) + '"' +
                    (checked ? ' checked' : '') + '> RPC' +
            '</label>' +
            '<button class="backend-rpc-apply" data-backend="' + MA.esc(b.id) + '" title="' +
                MA.esc(applyTitle) + '">' + MA.esc(t('monitor.rpc.apply', 'Применить')) + '</button>' +
            '<button class="backend-rpc-auto" data-backend="' + MA.esc(b.id) + '" title="' +
                MA.esc(autoTitle) + '">' + MA.esc(t('monitor.rpc.auto', 'авто')) + '</button>' +
            '<span class="backend-rpc-state" style="font-size:var(--font-size-xs);color:var(--text-secondary)">' +
                MA.esc(state) + '</span>' +
        '</div>';
    }

    // Навешивает обработчики на селекторы ролей после перерисовки таблицы.
    function bindRoleSelectors() {
        const tbody = document.querySelector('#backendsTable tbody');
        if (!tbody || typeof tbody.querySelectorAll !== 'function') return;
        tbody.querySelectorAll('.backend-role-select').forEach(function (sel) {
            sel.addEventListener('change', function () {
                const id = sel.getAttribute('data-backend');
                const role = sel.value;
                const api = window.Api || window.BalancerApi;
                if (!api || typeof api.setBackendRole !== 'function') {
                    console.warn('[monitor] Api.setBackendRole недоступен');
                    return;
                }
                api.setBackendRole(id, role).then(function (ok) {
                    if (window.Utils && Utils.showToast) {
                        Utils.showToast('Роль «' + (role || 'не задана') + '» применена: ' + id, ok ? 'success' : 'error');
                    }
                }).catch(function (err) {
                    if (window.Utils && Utils.showToast) {
                        Utils.showToast('Не удалось сохранить роль: ' +
                            (err && err.message ? err.message : err), 'error');
                    }
                });
            });
        });
        bindRpcToggles(tbody);
    }

    // R229 (2026-10-22): обработчики тумблера раскладки RPC.
    //
    // «Применить» отправляет выбор оператора (on/off), «авто» — возвращает решение
    // балансеру по ролям. Оба проходят через Api.setBackendRPCLayout и показывают
    // ответ сервера: важно видеть не «нажал», а «применилось» (воркер мог не
    // ответить — тогда приходит warning).
    function bindRpcToggles(tbody) {
        if (!tbody || typeof tbody.querySelectorAll !== 'function') return;
        const apiOf = function () { return window.Api || window.BalancerApi; };
        const toast = function (text, kind) {
            if (window.Utils && Utils.showToast) Utils.showToast(text, kind);
        };
        const send = function (id, mode, okText) {
            const api = apiOf();
            if (!api || typeof api.setBackendRPCLayout !== 'function') {
                console.warn('[monitor] Api.setBackendRPCLayout недоступен');
                return;
            }
            api.setBackendRPCLayout(id, mode).then(function (res) {
                const ok = !!(res && res.ok !== false);
                const warning = res && res.body && res.body.warning ? (': ' + res.body.warning) : '';
                toast(ok ? (okText + ': ' + id + warning) : ('RPC не применилось: ' + id + warning), ok ? 'success' : 'error');
            }).catch(function (err) {
                toast('Не удалось сохранить RPC: ' + (err && err.message ? err.message : err), 'error');
            });
        };
        tbody.querySelectorAll('.backend-rpc-apply').forEach(function (btn) {
            btn.addEventListener('click', function () {
                const id = btn.getAttribute('data-backend');
                // Чекбокс ищем в той же строке; data-атрибут — запасной путь для
                // таблиц, где строка не <tr> (тесты и компактные раскладки).
                let box = null;
                const row = btn.closest ? btn.closest('tr') : null;
                if (row && typeof row.querySelector === 'function') {
                    box = row.querySelector('.backend-rpc-enabled');
                }
                if (!box && typeof tbody.querySelector === 'function') {
                    box = tbody.querySelector('.backend-rpc-enabled[data-backend="' + id + '"]');
                }
                send(id, box && box.checked ? 'on' : 'off',
                    box && box.checked ? 'RPC включено' : 'RPC выключено');
            });
        });
        tbody.querySelectorAll('.backend-rpc-auto').forEach(function (btn) {
            btn.addEventListener('click', function () {
                send(btn.getAttribute('data-backend'), 'auto', 'RPC снова по ролям');
            });
        });
    }

    // ===== R133 (2026-10-16): состояние узла раскладки у САМОГО бэкенда =====
    //
    // Роль в селекторе — это ПРИКАЗ. Он бесполезен, если рядом не видно
    // исполнения: поднялся ли rpc-server, слушается ли порт и доступен ли он
    // С БАЛАНСЕРА (порт мог не опубликоваться при запуске контейнера — тогда
    // процесс живёт, а мастер до него не дойдёт). До R133 донор не отчитывался
    // вовсе, поэтому «выбрал донора — ничего не произошло» было неотличимо от
    // «всё работает».
    //
    // Данные — из GET /api/v1/backends → rpcNode (балансер складывает туда то,
    // что воркер сообщает в /api/models: enabled/running/listening + проверку
    // доступности с балансера).
    function rpcNodeBadge(b) {
        b = b || {};
        const node = b.rpcNode || null;
        if (!node) return '';
        const t = function (key, fallback, params) {
            return (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function')
                ? window.I18N.t(key, params) : fallback;
        };
        const port = node.port ? (':' + node.port) : '';
        const reason = node.reason ? MA.esc(node.reason) : '';
        const style = 'display:inline-block;margin-top:2px;font-size:var(--font-size-xs);padding:0 4px;border-radius:3px;border:1px solid ';
        // Порт показываем РЯДОМ с подписью, а не внутри неё: подпись переводится,
        // а номер порта — это факт, который оператор сверяет с compose/фаерволом.
        const withPort = function (text) { return port ? (text + ' ' + port) : text; };
        // Донор: узел обязана поднять эта машина — показываем расхождение.
        if (node.wanted) {
            if (node.running && node.listening && node.reachable) {
                return '<div style="' + style + '#22c55e55;color:#22c55e" title="' + reason + '">' +
                    MA.esc(withPort(t('monitor.rpcnode.up', 'узел раскладки поднят'))) + '</div>';
            }
            if (node.running && !node.reachable) {
                return '<div style="' + style + '#f9731655;color:#f97316" title="' + reason + '">' +
                    MA.esc(withPort(t('monitor.rpcnode.unreachable', 'узел раскладки недоступен с балансера'))) + '</div>';
            }
            return '<div style="' + style + '#ef444455;color:#ef4444" title="' + reason + '">' +
                MA.esc(withPort(t('monitor.rpcnode.down', 'узел раскладки не поднят'))) + '</div>';
        }
        // Не донор: узел мог поднять оператор руками — это факт, а не требование.
        if (node.running) {
            return '<div style="' + style + '#64748b55;color:var(--text-secondary)" title="' + reason + '">' +
                MA.esc(withPort(t('monitor.rpcnode.manual', 'узел раскладки поднят вручную'))) + '</div>';
        }
        return '';
    }

    // vramRemoteSource (R132): откуда вклад узлов раскладки в бюджет у
    // СПРАВОЧНЫХ путей — none | cache | probed. «0 МБ узлов» и «узлы не
    // измерялись» снаружи выглядели одинаково; теперь это разные подписи.
    function vramRemoteSourceNote(b) {
        b = b || {};
        const src = String(b.vramRemoteSource || '');
        if (!src) return '';
        const t = function (key, fallback) {
            return (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function')
                ? window.I18N.t(key) : fallback;
        };
        const map = {
            none: t('monitor.layout.source_none', 'узлы раскладки не измерялись'),
            cache: t('monitor.layout.source_cache', 'узлы раскладки — из последнего замера'),
            probed: t('monitor.layout.source_probed', 'узлы раскладки измерены')
        };
        const text = map[src];
        if (!text) return '';
        return '<div style="font-size:var(--font-size-xs);color:var(--text-muted)">' + MA.esc(text) + '</div>';
    }

    // ===== Sorting =====
    function sort(backends, column, asc) {
        currentSort = { column, asc };
        return [...backends].sort((a, b) => {
            let va, vb;
            switch (column) {
                case 'id': va = a.id || ''; vb = b.id || ''; break;
                case 'status': va = a.status || ''; vb = b.status || ''; break;
                case 'gpu': va = (a.gpu && a.gpu.usagePercent) || 0; vb = (b.gpu && b.gpu.usagePercent) || 0; break;
                case 'vram': va = (a.vram && a.vram.usagePercent) || 0; vb = (b.vram && b.vram.usagePercent) || 0; break;
                case 'cpu': va = (a.system && a.system.cpuUsagePercent) || 0; vb = (b.system && b.system.cpuUsagePercent) || 0; break;
                case 'ram': va = a.memoryUsagePercent || 0; vb = b.memoryUsagePercent || 0; break;
                case 'active': va = a.activeRequests || 0; vb = b.activeRequests || 0; break;
                case 'rps': va = (a.ollama && a.ollama.requestsPerSecond) || 0; vb = (b.ollama && b.ollama.requestsPerSecond) || 0; break;
                case 'score': va = a.score || 0; vb = b.score || 0; break;
                case 'uptime': va = a.lastSeen || ''; vb = b.lastSeen || ''; break;
                default: va = a.id || ''; vb = b.id || '';
            }

            if (typeof va === 'string') {
                return asc ? va.localeCompare(vb) : vb.localeCompare(va);
            }
            return asc ? va - vb : vb - va;
        });
    }

    // ===== Filtering =====
    function filter(backends, query) {
        currentFilter = query.toLowerCase();
        if (!currentFilter) return backends;
        return backends.filter(b => {
            const id = (b.id || '').toLowerCase();
            const status = (b.status || '').toLowerCase();
            // R-Image Phase 5: у image-бэкенда модели лежат в backend.image.models -
            // учитываем и их, иначе поиск по имени SD-модели ничего не находил.
            const imageNames = (b.image && Array.isArray(b.image.models))
                ? b.image.models.map(function (im) { return im.name || ''; })
                : [];
            const models = ((b.models && b.models.length ? b.models : imageNames) || []).join(' ').toLowerCase();
            return id.includes(currentFilter) || status.includes(currentFilter) || models.includes(currentFilter);
        });
    }

    // ===== Table Rendering =====
    function renderTable(backends) {
        const filtered = filter(backends, currentFilter);
        const sorted = sort(filtered, currentSort.column, currentSort.asc);
        const tbody = document.querySelector('#backendsTable tbody');
        if (!tbody) return;

        if (!sorted.length) {
            var noDataText = (typeof window !== 'undefined' && window.I18N) ? window.I18N.t('monitor.common.noData') : 'Нет данных';
            // colspan = 13 (12 + новая колонка Loading). Если в таблице другой # столбцов,
            // используем безопасное значение 13.
            tbody.innerHTML = '<tr><td colspan="13" style="color:var(--text-secondary);text-align:center;padding:16px">' + noDataText + '</td></tr>';
            return;
        }

        tbody.innerHTML = sorted.map(b => renderRow(b)).join('');
        bindSortHeaders();
        // R118: селекторы роли бэкенда в раскладке — навешиваем после перерисовки.
        bindRoleSelectors();
        // R185: уведомление о несовместимой CUDA-архитектуре (опрос — с TTL).
        renderArchBanner(sorted);
        probeBackendArch(sorted);
    }

    function renderRow(b) {
        const mr = b.maxConcurrentRequests || 10;
        const a = b.activeRequests || 0;
        const gu = (b.gpu && b.gpu.usagePercent != null) ? b.gpu.usagePercent : 0;
        // Round 16 (2026-07-10): VRAM% — поле vramUsagePercent (computed в balancer),
        // не b.vram.usagePercent. Старое имя читалось как 0 → колонка "VRAM" пустая.
        const vu = (b.vramUsagePercent != null) ? b.vramUsagePercent :
                   ((b.vram && b.vram.usagePercent != null) ? b.vram.usagePercent : 0);
        const cu = (b.system && b.system.cpuUsagePercent != null) ? b.system.cpuUsagePercent : 0;
        const ru = (b.memoryUsagePercent != null) ? b.memoryUsagePercent : ((b.system && b.system.memoryUsagePercent != null) ? b.system.memoryUsagePercent : 0);
        const sc = (b.score || b.weight || 0).toFixed(2);
        const scs = b.status === 'healthy' || b.status === 'active' || b.status === 'ready' ? 'badge-green' :
                    (b.status === 'error' || b.status === 'unhealthy' ? 'badge-red' :
                    (b.status === 'ollama_unavailable' ? 'badge-orange' : 'badge-yellow'));
        const up = b.lastSeen ? MA.fmtDur ? MA.fmtDur(Date.now() - new Date(b.lastSeen).getTime()) : '-' : '-';
        const rps = (b.ollama && b.ollama.requestsPerSecond != null) ? b.ollama.requestsPerSecond : 0;
        // Round 16: hasAgent + agentPort + agentId для visibility (после Round 12 dedup)
        const hasAgent = b.hasAgent ? '<span class="badge" style="background:rgba(34,197,94,0.12);color:#22c55e" title="' + MA.esc(b.agentId || 'agent') + '">●</span>' : '<span class="badge" style="opacity:0.4">○</span>';

        // ==== R-Image Phase 5 (2026-10-02): image-бэкенд (image_cpp) ====
        // У него нет ни ollama, ни llamaCpp: воркер живёт на своём порту
        // (imagePort), а модели лежат в backend.image.models. Без этой ветки
        // строка выглядела как Ollama-бэкенд с 0 моделей, а бейджа типа не было.
        const btType = b.backendType || b.backend_type || b.type || '';
        const isImage = String(btType).toLowerCase() === 'image_cpp'
            || String(btType).toLowerCase() === 'imagecpp'
            || String(btType).toLowerCase() === 'image.cpp';
        const imageBlock = b.image || {};
        const imagePort = parseInt(b.imagePort !== undefined ? b.imagePort : b.image_port, 10) || 0;
        const imageModels = Array.isArray(imageBlock.models) ? imageBlock.models : [];
        let typeBadge = '';
        if (window.Utils && typeof window.Utils.getBackendTypeBadge === 'function') {
            typeBadge = ' ' + window.Utils.getBackendTypeBadge(b);
        } else if (isImage) {
            typeBadge = ' <span class="badge" style="background:#4ade8020;border:1px solid #4ade80;color:#4ade80;font-size:var(--font-size-xs);padding:0 4px;border-radius:3px">🎨 image.cpp</span>';
        } else if (btType === 'llama_cpp') {
            typeBadge = ' <span class="badge" style="background:#ff6d0020;border:1px solid #ff6d00;color:#ff6d00;font-size:var(--font-size-xs);padding:0 4px;border-radius:3px">🦒 llama.cpp</span>';
        } else if (btType === 'ollama' || !btType) {
            typeBadge = ' <span class="badge" style="background:#1a73e820;border:1px solid #1a73e8;color:#1a73e8;font-size:var(--font-size-xs);padding:0 4px;border-radius:3px">🦙 Ollama</span>';
        }
        const imageBadges = isImage
            ? ' <span class="badge" style="background:rgba(74,222,128,0.12);color:#4ade80" title="' +
              MA.esc(window.I18N ? I18N.t('image.worker_port') : 'image.worker_port') + '">🔌 ' +
              (imagePort > 0 ? imagePort : MA.esc(window.I18N ? I18N.t('image.port_unset') : 'image.port_unset')) + '</span>'
            : '';

        // ==== Loading column (Issue: «отображение загрузки в мониторе») ====
        // Поддерживается два источника:
        //   1) b.loadingModels: [{name, state, loadingStartedAt, loadingSizeBytes, error}] — массив объектов
        //   2) b.loadingModelCount: число (если API отдаёт только счётчик)
        //   3) b.llamaCppMetrics.loadingModels: альтернативный путь (через metrics-broker)
        const loadingArr = (b.loadingModels && Array.isArray(b.loadingModels) && b.loadingModels.length > 0)
            ? b.loadingModels
            : ((b.llamaCppMetrics && Array.isArray(b.llamaCppMetrics.loadingModels)) ? b.llamaCppMetrics.loadingModels : []);
        const loadingCount = (b.loadingModelCount != null)
            ? b.loadingModelCount
            : loadingArr.length;
        let loadingCell;
        if (loadingCount > 0 && loadingArr.length === 0) {
            // Только счётчик известен
            loadingCell = '<td class="col-right"><span class="badge" title="' +
                (window.I18N ? I18N.t('gguf.col_loading_count', { count: loadingCount }) : ('Loading: ' + loadingCount)) +
                '"><i class="fas fa-spinner fa-spin"></i> ' + loadingCount + '</span></td>';
        } else if (loadingArr.length > 0) {
            // Полный список с elapsed-таймером
            const cells = loadingArr.slice(0, 3).map(function (lm) {
                const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
                const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
                const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
                const elapsedLabel = elapsedSec < 60
                    ? elapsedSec + 's'
                    : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
                if (lm.state === 'error') {
                    return '<span class="badge badge-red" title="' + MA.esc(lm.error || 'error') + '">' +
                        '<i class="fas fa-times-circle"></i> ' + MA.esc(lm.name) +
                    '</span>';
                }
                return '<span class="badge" style="background:rgba(74,158,255,0.12);color:#4a9eff;border-color:rgba(74,158,255,0.3)" title="' +
                    (window.I18N ? I18N.t('gguf.loading_indicator') : 'Loading') + ': ' + MA.esc(lm.name) + '">' +
                    '<i class="fas fa-spinner fa-spin"></i> ' + MA.esc(lm.name) + ' ' + elapsedLabel +
                '</span>';
            }).join(' ');
            const more = loadingArr.length > 3 ? (' +' + (loadingArr.length - 3)) : '';
            loadingCell = '<td class="col-right">' + cells + more + '</td>';
        } else {
            loadingCell = '<td class="col-right" style="color:var(--text-secondary)">-</td>';
        }

        // Hidden metrics tooltip data
        const powerLimit = b.gpu && b.gpu.powerLimit ? `Power: ${b.gpu.powerLimit}W` : '';
        const gpuClock = b.gpu && b.gpu.clock ? `Clock: ${b.gpu.gpuClock || b.gpu.clock}MHz` : '';
        const memClock = b.gpu && b.gpu.memClock ? `Mem: ${b.gpu.memClock}MHz` : '';
        const diskInfo = b.system && b.system.diskUsed != null ? `Disk: ${(b.system.diskUsed/1024).toFixed(1)}/${(b.system.diskTotal/1024).toFixed(0)}G` : '';
        const netInfo = b.system && b.system.networkRX != null ? `Net: ↓${formatBytes(b.system.networkRX)}/s ↑${formatBytes(b.system.networkTX)}/s` : '';
        const tooltip = [powerLimit, gpuClock, memClock, diskInfo, netInfo].filter(Boolean).join(' | ');

        // === Round 32 #4 (2026-08-10): actions cell с кнопкой Unload ===
        // Раньше (до фикса) в monitor не было action buttons — пользователь видел
        // бэйджи с именами моделей но не мог их выгрузить. Теперь для каждой
        // загруженной модели показывается inline кнопка "✕" (compact variant,
        // не занимает много места) с data-атрибутами для delegated handler'а.
        //
        // Клик вызывает MonitorApp.unloadModel(backendId, modelName) →
        // GgufApi.manageModel(backendId, 'unload', modelName) → POST /api/v1/backends/{id}/models
        // (тот же путь что GGUF page использует для unload, но 30s timeout — достаточно).
        const modelsSource = (b.models && b.models.length) ? b.models : imageModels.map(function (im) { return im.name || im.Name || ''; });
        const modelsArr = modelsSource.slice(0, 3);
        const modelBadges = modelsArr.map(function(m) {
            // R-Image Phase 5: для image-модели кнопку unload НЕ рисуем -
            // GgufApi.manageModel шлёт ollama/cppworker-путь, которого
            // image-воркер не понимает (управление - на странице «Изображения»).
            if (isImage) {
                const im = imageModels.filter(function(x) { return (x.name || x.Name) === m; })[0] || {};
                const st = String(im.state || '').toLowerCase();
                const col = (st === 'loaded') ? '#4ade80' : (st === 'loading') ? '#f0ad4e' : (st === 'error' || st === 'failed') ? '#ef4444' : 'var(--text-secondary)';
                return '<span class="badge" style="background:rgba(74,222,128,0.10);color:' + col + ';border-color:' + col + '">🎨 ' +
                    MA.esc(m) + (st ? ' <span style="opacity:0.8">(' + MA.esc(st) + ')</span>' : '') + '</span>';
            }
            return '<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2)">' +
                MA.esc(m) +
                // Compact unload button: ✕ (8px шрифт, hover-effect через CSS)
                ' <button class="monitor-unload-btn" data-backend="' + MA.esc(b.id) + '" data-model="' + MA.esc(m) + '" ' +
                'title="' + MA.esc((window.I18N ? I18N.t('gguf.unload_model', 'Unload') : 'Unload')) + ' ' + MA.esc(m) + '" ' +
                'style="background:transparent;border:none;color:inherit;cursor:pointer;padding:0 2px;font-size:var(--font-size-xs);line-height:1;opacity:0.7;">' +
                '<i class="fas fa-times"></i></button>' +
            '</span>';
        }).join(' ');
        const moreModels = modelsSource.length > 3 ? (' +' + (modelsSource.length - 3)) : '';

        return `<tr data-backend="${MA.esc(b.id)}" title="${MA.esc(tooltip)}">
            <td><strong>${MA.esc(b.id)}</strong> ${hasAgent}${imageBadges}${typeBadge}${roleSelect(b)}${rpcNodeBadge(b)}${vramRemoteSourceNote(b)}</td>
            <td><span class="badge ${scs}">${b.status}</span></td>
            <td>${bar(gu)} ${gu.toFixed(0)}%</td>
            <td>${bar(vu)} ${vu.toFixed(0)}%</td>
            <td>${bar(cu)} ${cu.toFixed(0)}%</td>
            <td>${bar(ru)} ${ru.toFixed(0)}%</td>
            <td class="col-right">${a}/${mr}</td>
            <td class="col-right">${rps > 0 ? rps.toFixed(1) : '-'}</td>
            <td class="col-right">${sc}</td>
            <td>${modelBadges}${moreModels}</td>
            <td class="col-right">${up}</td>
            ${loadingCell}
        </tr>`;
    }

    function bar(value) {
        const p = Math.max(0, Math.min(100, value || 0));
        const c = p >= 80 ? 'var(--danger)' : p >= 50 ? 'var(--warning)' : 'var(--success)';
        return `<span class="bar-track"><span class="bar-fill" style="width:${p}%;background:${c}"></span></span>`;
    }

    function formatBytes(bytes) {
        if (!bytes || bytes === 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
    }

    // ===== Sort Header Binding =====
    function bindSortHeaders() {
        document.querySelectorAll('#backendsTable th[data-sort]').forEach(th => {
            th.style.cursor = 'pointer';
            th.onclick = () => {
                const col = th.getAttribute('data-sort');
                const asc = currentSort.column !== col ? true : !currentSort.asc;
                const tbody = document.querySelector('#backendsTable tbody');
                if (!tbody) return;
                const rows = Array.from(tbody.querySelectorAll('tr'));
                const backends = rows.map(tr => {
                    const id = tr.getAttribute('data-backend');
                    return MA.lastData && MA.lastData.cluster && MA.lastData.cluster.backends ?
                        MA.lastData.cluster.backends.find(b => b.id === id) : null;
                }).filter(Boolean);
                renderTable(backends);
            };
        });
    }

    // ===== Filter Input =====
    function setupFilter(inputId) {
        const input = document.getElementById(inputId);
        if (!input) return;
        input.addEventListener('input', (e) => {
            currentFilter = e.target.value;
            if (MA.lastData && MA.lastData.cluster && MA.lastData.cluster.backends) {
                renderTable(MA.lastData.cluster.backends);
            }
        });
    }

    // ===== Unload Handler (Round 32 #4, 2026-08-10) =====
    // Delegated click handler для кнопок .monitor-unload-btn в таблице.
    // При клике — вызывает GgufApi.manageModel(backendId, 'unload', modelName)
    // (тот же path что GGUF page использует) и показывает toast о результате.
    //
    // Round 32 #4 fix: раньше (до этого fix'а) в Monitor'е НЕ БЫЛО кнопок unload
    // — пользователь видел бэйджи с именами моделей но не мог их выгрузить, приходилось
    // идти на GGUF page → выбрать backend → клик Unload там. Теперь unload доступен
    // прямо из Monitor'а одним кликом.
    function bindUnloadHandlers() {
        const tbody = document.querySelector('#backendsTable tbody');
        if (!tbody || tbody._ggufUnloadBound) return;
        tbody._ggufUnloadBound = true;
        tbody.addEventListener('click', function(e) {
            const btn = e.target.closest('.monitor-unload-btn');
            if (!btn) return;
            e.preventDefault();
            e.stopPropagation();
            const backendId = btn.getAttribute('data-backend');
            const modelName = btn.getAttribute('data-model');
            if (!backendId || !modelName) return;
            if (!window.GgufApi || !window.GgufApi.manageModel) {
                if (window.showToast) window.showToast('GgufApi not available', 'error');
                return;
            }
            // Visual feedback: disable + show spinner пока запрос в полёте
            btn.disabled = true;
            const origHtml = btn.innerHTML;
            btn.innerHTML = '<i class="fas fa-spinner fa-spin"></i>';
            if (window.showToast) window.showToast('Unloading ' + modelName + '...', 'info');
            window.GgufApi.manageModel(backendId, 'unload', modelName).then(function(result) {
                if (result && result.success === false) {
                    if (window.showToast) window.showToast('Unload failed: ' + (result.error || 'unknown'), 'error');
                } else {
                    if (window.showToast) window.showToast('Unloaded ' + modelName, 'success');
                    // Force refresh монитора — 30s poll слишком медленный.
                    if (MA && typeof MA.refresh === 'function') MA.refresh();
                }
            }).catch(function(err) {
                if (window.showToast) window.showToast('Unload error: ' + (err && err.message || err), 'error');
            }).finally(function() {
                btn.disabled = false;
                btn.innerHTML = origHtml;
            });
        });
    }

    // ===== Public API =====
    return {
        renderTable,
        sort,
        filter,
        setupFilter,
        bindUnloadHandlers,
        // R118: для теста селектора роли бэкенда (monitor-role-r118.test.js).
        renderRow,
        roleSelect,
        // R133: состояние узла раскладки и источник вклада узлов — для тестов.
        rpcNodeBadge,
        vramRemoteSourceNote,
        // R185: уведомление «архитектура не поддерживается» — для теста
        // (monitor-arch-r185.test.js).
        archBannerHtml,
        rememberArchInfo,
        renderArchBanner,
        archProbeList,
        getCurrentSort: () => currentSort,
        getCurrentFilter: () => currentFilter
    };
})();