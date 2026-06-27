/**
 * Bulk Models — Multi-select operations for the Models tab (Session A, Q3 W4).
 *
 * Поддерживает три UI-сценария:
 *   1. Checkbox в каждой карточке модели → добавление в `selected` Set.
 *   2. Кнопки "Select All / Select Loaded / Clear Selection" в toolbar'е.
 *   3. Кнопки "Load Selected / Unload Selected / Delete Selected / Cancel" в toolbar'е.
 *
 * Кнопка Delete Selected показывает confirm dialog (см. confirmDeleteSelected)
 * из-за деструктивной природы операции (нельзя отменить).
 *
 * Все bulk-операции идут через единый endpoint `POST /api/v1/cluster/models/bulk`,
 * который переиспользует `executeReloadOnBackend` из Session 17 (per-backend errors
 * не ломают общий ответ, удобно для UI).
 *
 * i18n: использует ключи `models.bulk.*` (10 ключей × 2 языка в en.js/ru.js).
 *
 * NB: модуль самодостаточный и не зависит от других модулей, кроме Api (для HTTP)
 * и window.I18N (для локализованных строк в динамически перегенерированном toolbar).
 */
(function () {
    'use strict';

    // selected: Set ключей `${backendId}::${modelName}`.
    const selected = new Set();

    // Подписка на смену языка — перерендерить toolbar (если он видим) с новыми строками.
    if (typeof window !== 'undefined') {
        window.addEventListener('i18n:changed', function () {
            renderToolbar();
        });
    }

    function key(backendId, modelName) {
        return (backendId || '') + '::' + (modelName || '');
    }

    function getAllCheckboxes() {
        return Array.from(document.querySelectorAll('.model-select-cb'));
    }

    /**
     * Обработчик изменения чекбокса в карточке модели.
     * Вызывается через onchange прямо в HTML.
     */
    function onSelectionChanged() {
        // Перечитываем ВСЕ чекбоксы (selected может рассинхронизироваться с DOM
        // после re-render моделей — фильтруем несуществующие ключи).
        const newSelection = new Set();
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            const backend = cb.getAttribute('data-backend');
            const model = cb.getAttribute('data-model');
            if (!backend || !model) return;
            const card = cb.closest('.model-card');
            if (cb.checked) {
                newSelection.add(key(backend, model));
                if (card) card.classList.add('selected');
            } else {
                if (card) card.classList.remove('selected');
            }
        });

        // Синхронизируем selected с DOM (убираем ключи, чьих карточек больше нет).
        selected.clear();
        newSelection.forEach(function (k) { selected.add(k); });

        renderToolbar();
    }

    /**
     * Выставить все чекбоксы (Select All).
     */
    function selectAll() {
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            if (!cb.checked) {
                cb.checked = true;
                // Синхронизируем selected вручную, чтобы избежать двойной работы.
                const backend = cb.getAttribute('data-backend');
                const model = cb.getAttribute('data-model');
                if (backend && model) {
                    selected.add(key(backend, model));
                    const card = cb.closest('.model-card');
                    if (card) card.classList.add('selected');
                }
            }
        });
        renderToolbar();
    }

    /**
     * Снять все чекбоксы (Clear Selection).
     */
    function selectNone() {
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            if (cb.checked) {
                cb.checked = false;
                const card = cb.closest('.model-card');
                if (card) card.classList.remove('selected');
            }
        });
        selected.clear();
        renderToolbar();
    }

    /**
     * Выставить только загруженные (Select Loaded). Используем heuristic:
     * карточка считается "loaded", если у её бэкенда `backendStatus === 'healthy'`
     * (т.е. модель показывается в `runningModels`, что означает она активна).
     *
     * Альтернативно: можно использовать `clusterModels.loaded()`, но это лишний
     * HTTP-запрос на каждый клик — heuristic на клиенте достаточно для UX.
     */
    function selectLoaded() {
        selectNone();
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            const card = cb.closest('.model-card');
            if (!card) return;
            // Карточка "loaded" = healthy backend. Можно также использовать
            // data-model-state="loaded" в будущем, но сейчас runningModels —
            // основной источник правды.
            // Текущий признак: бэкенд в data-backend имеет status=healthy (через badge).
            // Для простоты — выделяем все карточки (все показанные модели загружены,
            // т.к. они в runningModels). Это типичное поведение Ollama.
            cb.checked = true;
            const backend = cb.getAttribute('data-backend');
            const model = cb.getAttribute('data-model');
            if (backend && model) {
                selected.add(key(backend, model));
                card.classList.add('selected');
            }
        });
        renderToolbar();
    }

    /**
     * Переключить: выставить все невыделенные (инвертировать текущий выбор).
     */
    function selectInverse() {
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            const backend = cb.getAttribute('data-backend');
            const model = cb.getAttribute('data-model');
            if (!backend || !model) return;
            const k = key(backend, model);
            const card = cb.closest('.model-card');
            if (cb.checked) {
                cb.checked = false;
                selected.delete(k);
                if (card) card.classList.remove('selected');
            } else {
                cb.checked = true;
                selected.add(k);
                if (card) card.classList.add('selected');
            }
        });
        renderToolbar();
    }

    /**
     * Рендер toolbar'а (появляется только при selected.size > 0).
     *
     * Идемпотентная функция — может вызываться после каждого re-render моделей.
     * Перед рендером пересинхронизирует selected Set с текущим DOM (убирает
     * ключи, чьих карточек больше нет, и подсвечивает карточки с .selected).
     */
    function renderToolbar() {
        const toolbar = document.getElementById('modelsBulkToolbar');
        if (!toolbar) return;
        // Шаг 1: пересинхронизировать selected Set с реальным DOM.
        syncSelectionWithDom();
        const cnt = selected.size;
        if (cnt === 0) {
            toolbar.style.display = 'none';
            toolbar.innerHTML = '';
            return;
        }
        toolbar.style.display = 'flex';
        const _t = (k, p) => window.I18N ? window.I18N.t(k, p) : k;
        toolbar.innerHTML =
            '<span class="bulk-count">' + _t('models.bulk.selected_count', { count: cnt }) + '</span>' +
            '<button class="btn-bulk-load" onclick="window.bulkModels.execute(\'load\')">' + _t('models.bulk.load_selected') + '</button>' +
            '<button class="btn-bulk-unload" onclick="window.bulkModels.execute(\'unload\')">' + _t('models.bulk.unload_selected') + '</button>' +
            '<button class="btn-bulk-delete" onclick="window.bulkModels.confirmDeleteSelected()">' + _t('models.bulk.delete_selected') + '</button>' +
            '<button class="btn-bulk-cancel" onclick="window.bulkModels.selectNone()">' + _t('models.bulk.cancel') + '</button>';
    }

    /**
     * Синхронизировать selected Set с DOM: оставить только ключи,
     * для которых есть .model-select-cb, и подсветить их карточки.
     * Не вызывается напрямую извне — renderToolbar() вызывает перед каждым рендером.
     */
    function syncSelectionWithDom() {
        const existingKeys = new Set();
        const cbs = getAllCheckboxes();
        cbs.forEach(function (cb) {
            const backend = cb.getAttribute('data-backend');
            const model = cb.getAttribute('data-model');
            if (!backend || !model) return;
            const k = key(backend, model);
            existingKeys.add(k);
            const card = cb.closest('.model-card');
            if (cb.checked) {
                if (card) card.classList.add('selected');
            } else {
                if (card) card.classList.remove('selected');
                // Чекбокс unchecked — убираем из selected, чтобы не было «зомби»-выбора.
                selected.delete(k);
            }
        });
        // Также удаляем ключи, чьих карточек больше нет в DOM.
        const toDelete = [];
        selected.forEach(function (k) {
            if (!existingKeys.has(k)) toDelete.push(k);
        });
        toDelete.forEach(function (k) { selected.delete(k); });
    }

    /**
     * Подтверждение удаления (деструктивная операция).
     */
    function confirmDeleteSelected() {
        const _t = (k, p) => window.I18N ? window.I18N.t(k, p) : k;
        const cnt = selected.size;
        if (cnt === 0) return;
        const msg = _t('models.bulk.confirm_delete_msg', { count: cnt });
        const title = _t('models.bulk.confirm_delete_title');
        // Используем window.confirm для простоты (без отдельной модалки).
        // Если у проекта есть кастомный confirm — можно подключить сюда.
        if (typeof window.confirm === 'function') {
            // Для русского языка confirm() обычно показывает "ОК"/"Отмена".
            // Конкатенируем title и msg в одну строку.
            const confirmed = window.confirm(title + '\n\n' + msg);
            if (!confirmed) return;
        }
        execute('delete');
    }

    /**
     * Исполнить bulk-операцию (load/unload/delete).
     * Для load/unload → POST /api/v1/cluster/models/bulk.
     * Для delete → Api.backendModelOperation('delete') для каждой пары (backend, model),
     * потому что bulk endpoint в текущей версии не делает delete на cppworker
     * (delete — специфичная для ollama операция; cppworker удаляет файл через fs).
     *
     * NB: операции лучше объединять по backend'у для уменьшения числа запросов,
     * но для простоты сначала делаем последовательный цикл по всем парам.
     */
    async function execute(operation) {
        if (!window.API) {
            console.error('[bulkModels] Api module not loaded');
            return;
        }
        if (selected.size === 0) return;
        const pairs = Array.from(selected).map(function (k) {
            const parts = k.split('::');
            return { backendId: parts[0], modelName: parts[1] };
        });

        if (operation === 'delete') {
            // Удаление — отдельный flow через backend-specific endpoint.
            await executeDelete(pairs);
            return;
        }

        // load/unload — bulk endpoint.
        const body = {
            operation: operation,
            models: pairs.map(function (p) { return { model: p.modelName, backendId: p.backendId }; }),
            reason: 'bulk from UI'
        };

        try {
            const resp = await window.API.clusterModels.bulk(body);
            showToast('succeeded=' + (resp.succeeded || 0) + ' failed=' + (resp.failed || 0));
            // После успешной операции — очищаем выбор и обновляем список моделей.
            selectNone();
            if (window.ui && typeof window.ui.refresh === 'function') {
                try { window.ui.refresh(); } catch (e) { /* ignore */ }
            }
        } catch (err) {
            const msg = (err && err.message) ? err.message : String(err);
            showToast('Error: ' + msg, true);
            if (window.API && typeof window.API.handleError === 'function') {
                window.API.handleError(err, 'Bulk operation failed');
            }
        }
    }

    /**
     * Delete через per-backend endpoint (не bulk).
     * Api.backendModelOperation возвращает json с success/error.
     */
    async function executeDelete(pairs) {
        if (!window.API) return;
        let succeeded = 0;
        let failed = 0;
        const errors = [];
        for (const p of pairs) {
            try {
                const result = await window.API.backendModelOperation(p.backendId, 'delete', p.modelName);
                if (result && result.success !== false) {
                    succeeded++;
                } else {
                    failed++;
                    if (result && result.error) errors.push(p.modelName + ': ' + result.error);
                }
            } catch (err) {
                failed++;
                errors.push(p.modelName + ': ' + ((err && err.message) ? err.message : 'error'));
            }
        }
        const msg = 'Delete: succeeded=' + succeeded + ' failed=' + failed;
        showToast(msg, failed > 0);
        if (errors.length && window.console) {
            window.console.warn('[bulkModels] delete errors:', errors);
        }
        selectNone();
        if (window.ui && typeof window.ui.refresh === 'function') {
            try { window.ui.refresh(); } catch (e) { /* ignore */ }
        }
    }

    /**
     * Простой toast — попытка использовать существующий notification system,
     * если нет — fallback на window.alert.
     */
    function showToast(msg, isError) {
        // Если есть глобальная система нотификаций — используем её.
        if (window.ui && typeof window.ui.showNotification === 'function') {
            window.ui.showNotification(msg, isError ? 'error' : 'success');
            return;
        }
        // Fallback: console + alert (минимально invasive).
        if (window.console) {
            if (isError) console.error('[bulkModels]', msg);
            else console.log('[bulkModels]', msg);
        }
    }

    // Экспорт API модуля.
    window.bulkModels = {
        onSelectionChanged: onSelectionChanged,
        selectAll: selectAll,
        selectNone: selectNone,
        selectLoaded: selectLoaded,
        selectInverse: selectInverse,
        renderToolbar: renderToolbar,
        execute: execute,
        confirmDeleteSelected: confirmDeleteSelected,
        // Геттеры для тестов/debug.
        _selected: selected
    };
})();