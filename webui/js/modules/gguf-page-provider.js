/**
 * gguf-page-provider.js — провайдер обновления для страницы «GGUF модели».
 *
 * ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ, А НЕ СТРОКА В gguf-renderer-refresh.js: тот модуль —
 * часть рендерера GGUF и грузится в его собственном порядке; здесь же только
 * регистрация в общем реестре (data-refresh.js), чтобы кнопка/индикатор в шапке
 * приложения обновляли и список GGUF, а не только cluster state.
 *
 * ЧТО ОБНОВЛЯЕТ: список бэкендов и локальных моделей, панель деталей и активные
 * загрузки (те же функции, что и у внутренних поллингов рендерера — правила не
 * дублируются, второй источник правды не появляется).
 *
 * R493 (2026-11-17) — ДВА ДЕФЕКТА, которые здесь закрыты.
 *
 * 1. РЕГИСТРАЦИЯ ЗАВИСЕЛА ОТ ПОРЯДКА СКРИПТОВ. `register()` возвращал false,
 *    если `window.GgufModule` ещё не существует, и больше не вызывался ни разу
 *    (единственная повторная попытка — DOMContentLoaded). Порядок defer-скриптов
 *    сейчас правильный, но это хрупко: любая перестановка тегов молча оставляла
 *    страницу GGUF без провайдера — «кнопка в шапке не обновляет GGUF», ровно
 *    как в жалобе оператора. Теперь провайдер резолвит функции В МОМЕНТ ВЫЗОВА:
 *    регистрация не зависит от того, кто загрузился первым.
 *
 * 2. ПЕРЕРИСОВКА ПОВЕРХ ВВОДА. Провайдера теперь вызывает не только кнопка, но
 *    и авто-обновление (раз в REFRESH_INTERVAL, см. app.js →
 *    startPeriodicRefresh). `refreshDetail` перерисовывает панель целиком,
 *    поэтому тик посреди ввода в поле «Параметры модели» стирал бы набранное.
 *    Пока фокус внутри панели деталей — детали не перерисовываем (список
 *    бэкендов и загрузки обновляем: они не мешают вводу).
 *
 * Экспорт: window.GgufPageProvider = { register, _isTypingInDetail }.
 */
(function () {
    'use strict';

    var registered = false;

    /** Панель деталей (её и перерисовывает refreshDetail). */
    function detailPanel() {
        if (typeof document === 'undefined' || !document.getElementById) return null;
        return document.getElementById('ggufDetailPanel');
    }

    /**
     * _isTypingInDetail — оператор редактирует что-то внутри панели деталей.
     *
     * Проверяем именно фокус (activeElement), а не «в панели есть input»: поля
     * там есть всегда, а перерисовка мешает только во время ввода.
     */
    function _isTypingInDetail() {
        var panel = detailPanel();
        var active = (typeof document !== 'undefined') ? document.activeElement : null;
        if (!panel || !active || typeof panel.contains !== 'function') return false;
        if (!panel.contains(active)) return false;
        var tag = String(active.tagName || '').toUpperCase();
        return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || active.isContentEditable === true;
    }

    function register() {
        if (registered) return true;
        if (!window.DataRefresh || typeof window.DataRefresh.register !== 'function') return false;
        registered = true;
        window.DataRefresh.register('gguf', function () {
            // Функции рендерера берём в момент вызова: страница могла ещё не
            // загрузиться на момент регистрации (см. дефект 1 в шапке файла).
            var M = window.GgufModule || {};
            var jobs = [];
            if (typeof M.refreshBackends === 'function') {
                jobs.push(Promise.resolve(M.refreshBackends()));
            }
            // R688 (2026-11-22): {auto:true} — проход автоматический. Данные
            // (state) обновляются как обычно, но DOM панели деталей не
            // пересобирается, пока оператор заполняет форму: иначе набранные
            // параметры загрузки стирались каждым тиком. Решение и перенос
            // значений — в js/modules/gguf-detail-form-state.js.
            if (typeof M.refreshDetail === 'function' && !_isTypingInDetail()) {
                jobs.push(Promise.resolve(M.refreshDetail({ auto: true })));
            }
            if (typeof M.refreshActiveDownloads === 'function') {
                jobs.push(Promise.resolve(M.refreshActiveDownloads()));
            }
            return Promise.all(jobs);
        });
        return true;
    }

    window.GgufPageProvider = { register: register, _isTypingInDetail: _isTypingInDetail };

    // Регистрируемся сразу (файл мог загрузиться ПОСЛЕ DOMContentLoaded —
    // например, при динамической подгрузке модулей) и повторяем на
    // DOMContentLoaded: к тому моменту оживает реестр data-refresh.js.
    // Обе попытки идемпотентны.
    register();
    if (typeof document !== 'undefined' && document.addEventListener) {
        document.addEventListener('DOMContentLoaded', register);
    }
})();
