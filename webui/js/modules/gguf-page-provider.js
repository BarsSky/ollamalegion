/**
 * gguf-page-provider.js — провайдер обновления для страницы «GGUF модели».
 *
 * ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ, А НЕ СТРОКА В gguf-renderer-refresh.js: тот модуль —
 * часть рендерера GGUF и грузится в его собственном порядке; здесь же только
 * регистрация в общем реестре (data-refresh.js), чтобы кнопка/индикатор в шапке
 * приложения обновляли и список GGUF, а не только cluster state.
 *
 * ЧТО ОБНОВЛЯЕТ: список локальных моделей выбранного бэкенда и панель деталей
 * (те же функции, что и у внутренних поллингов рендерера — правила не
 * дублируются, второй источник правды не появляется).
 *
 * Экспорт: window.GgufPageProvider = { register }.
 */
(function () {
    'use strict';

    var registered = false;

    function register() {
        if (registered) return true;
        if (!window.DataRefresh || typeof window.DataRefresh.register !== 'function') return false;
        if (!window.GgufModule) return false;
        registered = true;
        window.DataRefresh.register('gguf', function () {
            var jobs = [];
            if (typeof window.GgufModule.refreshBackends === 'function') {
                jobs.push(Promise.resolve(window.GgufModule.refreshBackends()));
            }
            if (typeof window.GgufModule.refreshDetail === 'function') {
                jobs.push(Promise.resolve(window.GgufModule.refreshDetail()));
            }
            if (typeof window.GgufModule.refreshActiveDownloads === 'function') {
                jobs.push(Promise.resolve(window.GgufModule.refreshActiveDownloads()));
            }
            return Promise.all(jobs);
        });
        return true;
    }

    window.GgufPageProvider = { register: register };

    // Модуль страницы может загрузиться позже этого файла, поэтому пробуем и на
    // DOMContentLoaded: регистрация идемпотентна.
    if (typeof document !== 'undefined' && document.addEventListener) {
        document.addEventListener('DOMContentLoaded', register);
    }
})();
