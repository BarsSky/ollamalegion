// nav-burger.js — R134 (2026-10-16): доступ к сайдбару на узком окне.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «нет вкладки агенты». Проверка показала, что вкладка в
// разметке ЕСТЬ (data-page="agents"), но на ширине ≤768px CSS уводит сайдбар за
// край экрана (responsive.css: .sidebar { transform: translateX(-100%) }), а
// ОТКРЫТЬ ЕГО БЫЛО НЕЧЕМ: переключателя в разметке не существовало вовсе. То есть
// на узком окне оператор терял ВСЮ навигацию, а не только один пункт.
//
// Здесь только поведение: кнопка показывает сайдбар поверх страницы. Внешний вид
// и сама видимость кнопки — в responsive.css (она появляется лишь на узком окне и
// на широком не мешает раскладке).
(function () {
    'use strict';

    var BREAKPOINT = 768;

    function init() {
        var btn = document.getElementById('navBurger');
        if (!btn) return;

        btn.addEventListener('click', function () {
            document.body.classList.toggle('nav-open');
        });

        // Клик по пункту меню закрывает панель: на узком экране она перекрывает
        // содержимое, и оставлять её открытой после перехода незачем.
        var nav = document.querySelector('.sidebar .nav');
        if (nav && nav.addEventListener) {
            nav.addEventListener('click', function (e) {
                var t = e && e.target;
                if (t && t.closest && t.closest('.nav-item')) {
                    document.body.classList.remove('nav-open');
                }
            });
        }

        // Окно снова стало широким — состояние «открыт» больше не нужно (на
        // широком экране сайдбар и так виден).
        window.addEventListener('resize', function () {
            if (window.innerWidth > BREAKPOINT) {
                document.body.classList.remove('nav-open');
            }
        });
    }

    if (typeof document !== 'undefined') {
        if (document.readyState === 'loading') {
            document.addEventListener('DOMContentLoaded', init);
        } else {
            init();
        }
    }

    // Для теста: чистая функция без DOM.
    window.NavBurger = {
        BREAKPOINT: BREAKPOINT,
        shouldCloseOnResize: function (width) { return width > BREAKPOINT; }
    };
})();
