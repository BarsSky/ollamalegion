// gguf-reexport-help.js — слой ПОЯСНЕНИЙ блока «Ре-экспорт GGUF» (R746).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Справка и подпись выбранной модели добавили ~80 строк, и
// боевой `gguf-reexport.js` перевалил за 1500 НЕПУСТЫХ строк — страж R628
// (`tests/structure_ratchet_r628_test.go`) запрещает такие файлы и требует резать
// на слои сразу, а не растить исключения. Здесь — только текст и разметка
// пояснений: ни запросов, ни состояния задач, ни побочных эффектов.
//
// КАК ПОДКЛЮЧАЕТСЯ. Главный модуль берёт `window.GgufReexportHelp` через ленивый
// резолвер (как слой задач `window.GgufReexportJobs`): нет файла — блок работает
// без справки и без строки «выбрано», но не падает.
//
// Зависимости фабрики (передаются из главного модуля, чтобы здесь не дублировать
// i18n/экранирование):
//   t(key, vars)   — перевод;
//   esc(text)      — экранирование HTML;
//   currentModel() — выбранная модель списка (объект сервиса) или null.
(function () {
    'use strict';

    /**
     * create — фабрика слоя пояснений.
     *
     * @param {{t:Function, esc:Function, currentModel:Function}} deps
     */
    function create(deps) {
        var t = deps.t;
        var esc = deps.esc;
        var currentModel = deps.currentModel;

        /**
         * helpHTML — содержимое сворачиваемой справки.
         *
         * Список целей берётся из `capabilities` сервиса, поэтому справка не может
         * разойтись с реальностью: недоступная цель объясняет причину (например,
         * «образ собран без torch»), а не прячется.
         *
         * @param {Object} caps — ответ GET /api/reexport/capabilities
         */
        function helpHTML(caps) {
            var targets = ((caps || {}).targets || []);
            var targetsHtml = targets.map(function (tg) {
                var label = esc(tg.label || tg.id || '');
                var mark = tg.available
                    ? '<span class="rx-badge rx-badge-patchable">' + esc(t('reexport.help_available')) + '</span>'
                    : '<span class="rx-badge rx-badge-unreadable">' + esc(t('reexport.help_unavailable')) + '</span>';
                var reason = (!tg.available && tg.reason) ? '<div class="rx-step-hint">' + esc(tg.reason) + '</div>' : '';
                return '<li>' + label + ' ' + mark + reason + '</li>';
            }).join('');
            var flow = [1, 2, 3, 4, 5].map(function (n) {
                return '<li>' + esc(t('reexport.help_flow_' + n)) + '</li>';
            }).join('');
            var faq = [1, 2, 3, 4].map(function (n) {
                return '<li>' + esc(t('reexport.help_faq_' + n)) + '</li>';
            }).join('');
            return '<div class="rx-help-body">' +
                '<p>' + esc(t('reexport.help_what')) + '</p>' +
                '<div class="rx-help-title">' + esc(t('reexport.help_targets_title')) + '</div>' +
                '<ul class="rx-help-list" id="rxHelpTargets">' + targetsHtml + '</ul>' +
                '<div class="rx-help-title">' + esc(t('reexport.help_flow_title')) + '</div>' +
                '<ol class="rx-help-list" id="rxHelpFlow">' + flow + '</ol>' +
                '<div class="rx-help-title">' + esc(t('reexport.help_limits_title')) + '</div>' +
                '<p>' + esc(t('reexport.help_limits')) + '</p>' +
                '<div class="rx-help-title">' + esc(t('reexport.help_faq_title')) + '</div>' +
                '<ul class="rx-help-list" id="rxHelpFaq">' + faq + '</ul>' +
                '<div class="rx-step-hint">' + esc(t('reexport.help_docs')) + '</div>' +
                '</div>';
        }

        /**
         * selectedText — «выбрано: <файл>» либо подсказка, что выбрать нечего.
         *
         * Живая причина: выбор модели был виден только по едва заметному фону
         * строки, и оператор не понимал, с каким файлом работают шаги 2-3.
         */
        function selectedText() {
            var m = currentModel();
            if (m) {
                return '<i class="fas fa-check" aria-hidden="true"></i> ' +
                    esc(t('reexport.selected_prefix')) + ' <code>' + esc(m.name || m.path) + '</code>';
            }
            return '<span style="color:var(--text-muted);">' + esc(t('reexport.selected_none')) + '</span>';
        }

        /**
         * loadNote — подпись шага 5: на каком узле пойдёт проверка загрузки.
         *
         * Пустой id = бэкенд не выбран: честно говорим это, а не грузим «куда-то».
         */
        function loadNote(backendID) {
            var id = String(backendID || '');
            if (!id) return t('reexport.load_node_missing');
            return t('reexport.load_node_pre', { name: id });
        }

        /** scopeNote — с каким томом/типом моделей работает блок. */
        function scopeNote() {
            return t('reexport.scope_note');
        }

        return { helpHTML: helpHTML, selectedText: selectedText, loadNote: loadNote, scopeNote: scopeNote };
    }

    window.GgufReexportHelp = { create: create };
})();
