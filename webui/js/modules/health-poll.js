// health-poll.js — R690 (2026-11-22): ЦИКЛ ОБНОВЛЕНИЯ СТРАНИЦЫ «СОСТОЯНИЕ».
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ МОДУЛЬ. До R690 health.html открывался новой вкладкой
// (`target="_blank"`), поэтому вопроса «а надо ли ему опрашивать API прямо
// сейчас» не существовало: страница была видна ровно тогда, когда её открыли, и
// её собственный setTimeout-цикл (5 с) не мешал никому. Теперь тот же файл живёт
// в iframe панели, а у монитора на этом месте уже есть правило R493:
//
//   метрики нужны только когда оператор их видит.
//
// Правило складывается из четырёх поводов, и все четыре приходят СНАРУЖИ
// (страница их не выводит сама):
//   * страница панели закрыта (ушли на «Дашборд») — postMessage ollamalegion-pause;
//   * вкладка браузера скрыта — visibilitychange собственного документа;
//   * сессия истекла — панель гасит каналы (session-expiry.js → app.js);
//   * авто-обновление на паузе или страница закрыта — те же правила, что у монитора.
// Кнопка «Пауза» на самой странице — пятый, ЛОКАЛЬНЫЙ повод: он принадлежит
// оператору и не должен сниматься чужим сообщением.
//
// ПОЧЕМУ ВЫНЕСЕНО ИЗ health.html. Цикл — это ровно та часть страницы, у которой
// есть поведение, а не разметка: «на паузе не стрелять», «после возврата
// обновить СРАЗУ, а не через полный интервал», «два таймера не заводить».
// Проверять это в HTML-строке нельзя, поэтому логика живёт здесь, без DOM и без
// сети (см. webui/tests/health-spa-r690.test.js — там таймеры подменены).
//
// КОНТРАКТ (его читает health.html и тест):
//   var poll = HealthPoll.create({ load: fn, interval: fn[, setTimer, clearTimer] });
//   poll.start()                 — включить цикл (первый запрос делает вызывающий);
//   poll.stop()                  — снять таймер (страница выгружается/закрыта);
//   poll.setPaused(reason, on)   — reason: 'user' | 'remote' | 'hidden' | 'auth';
//   poll.isPaused()              — эффективная пауза (любой из поводов);
//   poll.isPausedBy(reason)      — состояние ОДНОГО повода (для кнопки страницы);
//   poll.refreshNow()            — немедленный запрос (ручная кнопка/сообщение);
//   poll.stats()                 — счётчики для диагностики и теста.
//
// ПРАВИЛО ПЕРЕХОДА (главное): запрос при снятии паузы делается РОВНО на переходе
// «было на паузе → снова работаем». На старте (start) запроса нет — его делает
// вызывающий, иначе первое открытие страницы давало бы два запроса подряд.
(function () {
    'use strict';

    var REASONS = ['user', 'remote', 'hidden', 'auth'];
    // Нижняя граница интервала: защита от 0/NaN из select'а и от случайного
    // «опрашиваем в цикле событий».
    var MIN_INTERVAL_MS = 250;
    var DEFAULT_INTERVAL_MS = 5000;

    function create(opts) {
        opts = opts || {};
        var load = opts.load;
        if (typeof load !== 'function') {
            throw new Error('HealthPoll.create: load обязан быть функцией');
        }
        var intervalOf = (typeof opts.interval === 'function')
            ? opts.interval
            : function () { return DEFAULT_INTERVAL_MS; };
        // Таймеры подменяемы: тест гоняет цикл без ожидания реального времени.
        var setTimer = (typeof opts.setTimer === 'function')
            ? opts.setTimer
            : function (fn, ms) { return setTimeout(fn, ms); };
        var clearTimer = (typeof opts.clearTimer === 'function')
            ? opts.clearTimer
            : function (id) { clearTimeout(id); };
        var onError = (typeof opts.onError === 'function') ? opts.onError : function () {};

        var timer = null;
        var running = false;
        var pausedBy = { user: false, remote: false, hidden: false, auth: false };
        var stats = { ticks: 0, loads: 0, resumes: 0, suppressed: 0 };

        function isPaused() {
            for (var i = 0; i < REASONS.length; i++) {
                if (pausedBy[REASONS[i]]) return true;
            }
            return false;
        }

        function cancel() {
            if (timer !== null) {
                clearTimer(timer);
                timer = null;
            }
        }

        function doLoad() {
            stats.loads++;
            try {
                load();
            } catch (e) {
                // Ошибка запроса не должна убивать цикл: иначе страница молча
                // перестаёт обновляться (одна неудача сети = «замершие» данные).
                onError(e);
            }
        }

        function arm() {
            if (timer !== null || !running || isPaused()) return;
            var ms = Number(intervalOf());
            if (!isFinite(ms) || ms < MIN_INTERVAL_MS) ms = MIN_INTERVAL_MS;
            timer = setTimer(function onTick() {
                timer = null;
                if (!running) return;
                stats.ticks++;
                // Пауза могла включиться между постановкой таймера и его срабатыванием.
                if (isPaused()) {
                    stats.suppressed++;
                    return;
                }
                doLoad();
                arm();
            }, ms);
        }

        /**
         * setPaused — выбрать/снять ОДИН повод паузы.
         *
         * Поводы независимы: «оператор нажал паузу» (user) не снимается сообщением
         * панели, а «вкладка скрыта» (hidden) не снимается возвратом на страницу
         * в панели. Эффективная пауза — ИЛИ по всем поводам.
         */
        function setPaused(reason, on) {
            if (REASONS.indexOf(reason) === -1) return false;
            var next = !!on;
            var wasPaused = isPaused();
            pausedBy[reason] = next;
            var nowPaused = isPaused();
            if (nowPaused) {
                cancel();
                return true;
            }
            if (wasPaused && running) {
                // R690: ВОЗВРАТ НА СТРАНИЦУ ОБНОВЛЯЕТ ДАННЫЕ СРАЗУ. Ждать полный
                // интервал (5 с по умолчанию) нельзя: оператор вернулся смотреть
                // состояние, и первые 5 секунд видел бы данные с момента ухода.
                stats.resumes++;
                doLoad();
            }
            arm();
            return true;
        }

        function start() {
            running = true;
            // Первый запрос делает вызывающий (boot страницы): здесь только
            // расписание, иначе первое открытие давало бы два запроса подряд.
            arm();
            return true;
        }

        function stop() {
            running = false;
            cancel();
            return true;
        }

        /** refreshNow — запрос по требованию (кнопка «Обновить», сообщение панели). */
        function refreshNow() {
            cancel();
            doLoad();
            arm();
            return true;
        }

        return {
            start: start,
            stop: stop,
            setPaused: setPaused,
            isPaused: isPaused,
            isPausedBy: function (reason) { return !!pausedBy[reason]; },
            isRunning: function () { return running; },
            refreshNow: refreshNow,
            stats: function () {
                return {
                    ticks: stats.ticks,
                    loads: stats.loads,
                    resumes: stats.resumes,
                    suppressed: stats.suppressed,
                    paused: isPaused(),
                    running: running,
                    scheduled: timer !== null,
                    reasons: {
                        user: pausedBy.user,
                        remote: pausedBy.remote,
                        hidden: pausedBy.hidden,
                        auth: pausedBy.auth
                    }
                };
            }
        };
    }

    window.HealthPoll = {
        create: create,
        REASONS: REASONS.slice(),
        MIN_INTERVAL_MS: MIN_INTERVAL_MS,
        DEFAULT_INTERVAL_MS: DEFAULT_INTERVAL_MS
    };
})();
