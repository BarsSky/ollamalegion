/* scheme-canvas.js — R402 (2026-11-05).
 *
 * Интерактивная схема жизненного цикла запроса.
 *
 * ЗАЧЕМ. Серия правок «по месту» (R394–R401) лечила симптомы на одном маршруте,
 * а тот же класс дефекта всплывал на другом (native /api/chat починен, OpenAI-
 * стрим продолжал врать про finish_reason). Нужна одна схема всех стадий и
 * ответов, по которой можно пройти цепочку и увидеть, ГДЕ именно сломалась
 * логика, не перечитывая весь код.
 *
 * КАК РАБОТАЕТ. Читает docs/scheme.json (единственный источник структуры),
 * раскладывает узлы по стадиям, рисует связи, даёт панораму/масштаб/поиск и
 * инспектор узла: что происходит, что уходит клиенту, коды ошибок, файлы и
 * функции, ссылка на раздел техдокумента.
 *
 * БЕЗ ВНЕШНИХ БИБЛИОТЕК: WebUI собирается офлайн, CDN запрещён.
 */
(function () {
    'use strict';

    var SVG_NS = 'http://www.w3.org/2000/svg';
    var COL_W = 320;
    var NODE_W = 264;
    var NODE_H = 118;
    var ROW_H = 148;
    var PAD_X = 30;
    var PAD_TOP = 110;

    var state = {
        data: null,
        nodeEls: {},
        edgeEls: [],
        view: { x: 0, y: 0, scale: 1 },
        selected: null,
        query: '',
        dragging: false,
        dragStart: null
    };

    function el(tag, cls, text) {
        var e = document.createElement(tag);
        if (cls) e.className = cls;
        if (text !== undefined && text !== null) e.textContent = text;
        return e;
    }

    function stageIndex(id) {
        for (var i = 0; i < state.data.stages.length; i++) {
            if (state.data.stages[i].id === id) return i;
        }
        return 0;
    }

    function layout() {
        var rows = {};
        var positions = {};
        state.data.nodes.forEach(function (n) {
            var si = stageIndex(n.stage);
            rows[si] = (rows[si] || 0) + 1;
            var row = rows[si] - 1;
            positions[n.id] = {
                x: PAD_X + si * COL_W,
                y: PAD_TOP + row * ROW_H,
                col: si,
                row: row
            };
        });
        var maxRows = 1;
        Object.keys(rows).forEach(function (k) { if (rows[k] > maxRows) maxRows = rows[k]; });
        return {
            positions: positions,
            width: PAD_X * 2 + state.data.stages.length * COL_W,
            height: PAD_TOP + maxRows * ROW_H + 60
        };
    }

    function applyView() {
        var v = state.view;
        var board = document.getElementById('schemeBoard');
        var svg = document.getElementById('schemeEdges');
        var stages = document.getElementById('schemeStages');
        var t = 'translate(' + v.x + 'px,' + v.y + 'px) scale(' + v.scale + ')';
        if (board) board.style.transform = t;
        if (svg) svg.style.transform = t;
        if (stages) stages.style.transform = t;
        var z = document.getElementById('schemeZoomLabel');
        if (z) z.textContent = Math.round(v.scale * 100) + '%';
    }

    function fitView() {
        var stage = document.getElementById('schemeStage');
        var L = state._layout;
        if (!stage || !L) return;
        var w = stage.clientWidth || 1000;
        var h = stage.clientHeight || 600;
        var s = Math.min(w / L.width, h / L.height, 1);
        if (s < 0.35) s = 0.35;
        state.view.scale = s;
        state.view.x = Math.max(0, (w - L.width * s) / 2);
        state.view.y = Math.max(0, (h - L.height * s) / 2);
        applyView();
    }

    function edgePath(a, b) {
        var ax = a.x + NODE_W;
        var ay = a.y + NODE_H / 2;
        var bx = b.x;
        var by = b.y + NODE_H / 2;
        if (b.col === a.col) {
            ax = a.x + NODE_W / 2;
            ay = a.y + NODE_H;
            bx = b.x + NODE_W / 2;
            by = b.y;
        } else if (bx < ax) {
            ax = a.x;
            bx = b.x + NODE_W;
        }
        var dx = Math.max(40, Math.abs(bx - ax) * 0.45);
        return 'M ' + ax + ' ' + ay + ' C ' + (ax + dx) + ' ' + ay + ', ' + (bx - dx) + ' ' + by + ', ' + bx + ' ' + by;
    }

    function drawEdges(L) {
        var svg = document.getElementById('schemeEdges');
        if (!svg) return;
        svg.setAttribute('width', L.width);
        svg.setAttribute('height', L.height);
        svg.setAttribute('viewBox', '0 0 ' + L.width + ' ' + L.height);
        while (svg.firstChild) svg.removeChild(svg.firstChild);

        var defs = document.createElementNS(SVG_NS, 'defs');
        var marker = document.createElementNS(SVG_NS, 'marker');
        marker.setAttribute('id', 'schemeArrow');
        marker.setAttribute('viewBox', '0 0 10 10');
        marker.setAttribute('refX', '9');
        marker.setAttribute('refY', '5');
        marker.setAttribute('markerWidth', '7');
        marker.setAttribute('markerHeight', '7');
        marker.setAttribute('orient', 'auto-start-reverse');
        var mp = document.createElementNS(SVG_NS, 'path');
        mp.setAttribute('d', 'M 0 0 L 10 5 L 0 10 z');
        mp.setAttribute('class', 'scheme-arrow');
        marker.appendChild(mp);
        defs.appendChild(marker);
        svg.appendChild(defs);

        state.edgeEls = [];
        (state.data.edges || []).forEach(function (e) {
            var a = L.positions[e.from];
            var b = L.positions[e.to];
            if (!a || !b) return;
            var p = document.createElementNS(SVG_NS, 'path');
            p.setAttribute('d', edgePath(a, b));
            p.setAttribute('class', 'scheme-edge');
            p.setAttribute('marker-end', 'url(#schemeArrow)');
            p.setAttribute('data-from', e.from);
            p.setAttribute('data-to', e.to);
            svg.appendChild(p);
            if (e.label) {
                var lx = (a.x + b.x) / 2 + NODE_W / 2;
                var ly = (a.y + b.y) / 2 + NODE_H / 2 - 6;
                var t = document.createElementNS(SVG_NS, 'text');
                t.setAttribute('x', lx);
                t.setAttribute('y', ly);
                t.setAttribute('class', 'scheme-edge-label');
                t.textContent = e.label;
                svg.appendChild(t);
            }
            state.edgeEls.push(p);
        });
    }

    function renderStages(L) {
        var head = document.getElementById('schemeStages');
        if (!head) return;
        head.innerHTML = '';
        head.style.width = L.width + 'px';
        state.data.stages.forEach(function (s, i) {
            var d = el('div', 'scheme-stage-head');
            d.style.left = (PAD_X + i * COL_W) + 'px';
            d.style.width = NODE_W + 'px';
            d.appendChild(el('div', 'scheme-stage-num', String(i + 1)));
            var txt = el('div', 'scheme-stage-txt');
            txt.appendChild(el('div', 'scheme-stage-title', s.title.replace(/^\d+\.\s*/, '')));
            txt.appendChild(el('div', 'scheme-stage-hint', s.hint || ''));
            d.appendChild(txt);
            head.appendChild(d);
        });
    }

    function nodeMatches(n) {
        if (!state.query) return false;
        var q = state.query.toLowerCase();
        var hay = [n.title, n.stage, (n.detail || []).join(' '), (n.outputs || []).join(' '),
            (n.code || []).join(' '), (n.errors || []).join(' '), n.doc || ''].join(' ').toLowerCase();
        return hay.indexOf(q) >= 0;
    }

    function renderNodes(L) {
        var board = document.getElementById('schemeBoard');
        if (!board) return;
        board.style.width = L.width + 'px';
        board.style.height = L.height + 'px';
        board.innerHTML = '';
        state.nodeEls = {};

        state.data.nodes.forEach(function (n) {
            var p = L.positions[n.id];
            var card = el('button', 'scheme-node');
            card.type = 'button';
            card.style.left = p.x + 'px';
            card.style.top = p.y + 'px';
            card.style.width = NODE_W + 'px';
            card.style.minHeight = NODE_H + 'px';
            card.setAttribute('data-node', n.id);
            if ((n.errors || []).length) card.classList.add('has-errors');

            var stage = state.data.stages[stageIndex(n.stage)];
            card.appendChild(el('span', 'scheme-node-stage', stage ? stage.title.replace(/^\d+\.\s*/, '') : n.stage));
            card.appendChild(el('span', 'scheme-node-title', n.title));
            if ((n.errors || []).length) {
                card.appendChild(el('span', 'scheme-node-badge', n.errors.length + ' отказ(а)'));
            }
            card.addEventListener('click', function (ev) {
                ev.stopPropagation();
                select(n.id);
            });
            board.appendChild(card);
            state.nodeEls[n.id] = card;
        });
    }

    function select(id) {
        state.selected = id;
        Object.keys(state.nodeEls).forEach(function (k) {
            state.nodeEls[k].classList.toggle('is-selected', k === id);
            state.nodeEls[k].classList.toggle('is-dimmed', !!state.query && !nodeMatches(nodeById(k)));
        });
        state.edgeEls.forEach(function (p) {
            var on = p.getAttribute('data-from') === id || p.getAttribute('data-to') === id;
            p.classList.toggle('is-active', on);
        });
        var n = nodeById(id);
        var box = document.getElementById('schemeInspector');
        if (!box || !n) return;
        box.innerHTML = '';
        box.classList.add('is-open');

        var head = el('div', 'scheme-insp-head');
        head.appendChild(el('div', 'scheme-insp-stage', (state.data.stages[stageIndex(n.stage)] || {}).title || n.stage));
        var close = el('button', 'scheme-insp-close', '×');
        close.type = 'button';
        close.title = 'Закрыть';
        close.addEventListener('click', function () {
            box.classList.remove('is-open');
            state.selected = null;
            Object.keys(state.nodeEls).forEach(function (k) {
                state.nodeEls[k].classList.remove('is-selected');
            });
            state.edgeEls.forEach(function (p) { p.classList.remove('is-active'); });
        });
        head.appendChild(close);
        box.appendChild(head);
        box.appendChild(el('h3', 'scheme-insp-title', n.title));

        (n.detail || []).forEach(function (d) {
            box.appendChild(el('p', 'scheme-insp-text', d));
        });

        function section(title, items, cls) {
            if (!items || !items.length) return;
            box.appendChild(el('div', 'scheme-insp-section', title));
            var ul = el('ul', 'scheme-insp-list ' + (cls || ''));
            items.forEach(function (it) { ul.appendChild(el('li', '', it)); });
            box.appendChild(ul);
        }

        section('Что видит клиент', n.outputs);
        section('Отказы', n.errors, 'is-errors');
        if (n.code && n.code.length) {
            box.appendChild(el('div', 'scheme-insp-section', 'Где в коде'));
            var code = el('ul', 'scheme-insp-code');
            n.code.forEach(function (c) { code.appendChild(el('li', '', c)); });
            box.appendChild(code);
        }
        if (n.doc) {
            var docLink = el('a', 'scheme-insp-doc', 'Техдокумент: ' + n.doc);
            docLink.href = (state.data.docs && state.data.docs.tech) || '#';
            docLink.target = '_blank';
            docLink.rel = 'noopener';
            box.appendChild(docLink);
        }
        var related = relatedNodes(n.id);
        if (related.length) {
            box.appendChild(el('div', 'scheme-insp-section', 'Связанные шаги'));
            var wrap = el('div', 'scheme-insp-related');
            related.forEach(function (r) {
                var b = el('button', 'scheme-chip', r.title);
                b.type = 'button';
                b.addEventListener('click', function () { select(r.id); });
                wrap.appendChild(b);
            });
            box.appendChild(wrap);
        }

        var copy = el('button', 'scheme-copy', 'Скопировать для агента');
        copy.type = 'button';
        copy.addEventListener('click', function () {
            var text = 'Шаг схемы «' + n.title + '» (стадия ' + n.stage + ')\n' +
                (n.detail || []).join('\n') + '\nКлиент видит: ' + (n.outputs || []).join('; ') +
                '\nОтказы: ' + (n.errors || []).join('; ') +
                '\nКод: ' + (n.code || []).join('; ') +
                '\nТехдокумент: ' + (state.data.docs && state.data.docs.tech || '');
            if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(text).then(function () {
                    copy.textContent = 'Скопировано';
                    setTimeout(function () { copy.textContent = 'Скопировать для агента'; }, 1500);
                });
            }
        });
        box.appendChild(copy);
    }

    function nodeById(id) {
        for (var i = 0; i < state.data.nodes.length; i++) {
            if (state.data.nodes[i].id === id) return state.data.nodes[i];
        }
        return null;
    }

    function relatedNodes(id) {
        var out = [];
        (state.data.edges || []).forEach(function (e) {
            if (e.from === id && nodeById(e.to)) out.push(nodeById(e.to));
            if (e.to === id && nodeById(e.from)) out.push(nodeById(e.from));
        });
        return out;
    }

    function applySearch(q) {
        state.query = (q || '').trim();
        Object.keys(state.nodeEls).forEach(function (k) {
            var n = nodeById(k);
            var hit = !state.query || nodeMatches(n);
            state.nodeEls[k].classList.toggle('is-match', !!state.query && hit);
            state.nodeEls[k].classList.toggle('is-dimmed', !!state.query && !hit);
        });
    }

    function bindPanZoom() {
        var stage = document.getElementById('schemeStage');
        if (!stage) return;
        stage.addEventListener('mousedown', function (e) {
            if (e.target.closest && e.target.closest('.scheme-node')) return;
            state.dragging = true;
            state.dragStart = { x: e.clientX - state.view.x, y: e.clientY - state.view.y };
            stage.classList.add('is-dragging');
        });
        window.addEventListener('mousemove', function (e) {
            if (!state.dragging) return;
            state.view.x = e.clientX - state.dragStart.x;
            state.view.y = e.clientY - state.dragStart.y;
            applyView();
        });
        window.addEventListener('mouseup', function () {
            state.dragging = false;
            if (stage) stage.classList.remove('is-dragging');
        });
        stage.addEventListener('wheel', function (e) {
            if (!e.ctrlKey && !e.metaKey) return;
            e.preventDefault();
            var k = e.deltaY > 0 ? 0.9 : 1.1;
            zoomAt(e.clientX, e.clientY, k);
        }, { passive: false });
        var zin = document.getElementById('schemeZoomIn');
        var zout = document.getElementById('schemeZoomOut');
        var zfit = document.getElementById('schemeZoomFit');
        if (zin) zin.addEventListener('click', function () { zoomAt(stage.clientWidth / 2, stage.clientHeight / 2, 1.15); });
        if (zout) zout.addEventListener('click', function () { zoomAt(stage.clientWidth / 2, stage.clientHeight / 2, 0.87); });
        if (zfit) zfit.addEventListener('click', fitView);
        var search = document.getElementById('schemeSearch');
        if (search) search.addEventListener('input', function () { applySearch(search.value); });
        document.addEventListener('keydown', function (e) {
            if (e.key === 'Escape') {
                var box = document.getElementById('schemeInspector');
                if (box) box.classList.remove('is-open');
            }
        });
    }

    function zoomAt(cx, cy, k) {
        var v = state.view;
        var rect = document.getElementById('schemeStage').getBoundingClientRect();
        var px = cx - rect.left;
        var py = cy - rect.top;
        var ns = Math.min(2.5, Math.max(0.3, v.scale * k));
        var r = ns / v.scale;
        v.x = px - (px - v.x) * r;
        v.y = py - (py - v.y) * r;
        v.scale = ns;
        applyView();
    }

    function render(data) {
        state.data = data;
        var title = document.getElementById('schemeTitle');
        if (title) title.textContent = data.title || 'Схема работы';
        var sub = document.getElementById('schemeSubtitle');
        if (sub) sub.textContent = data.subtitle || '';
        var ver = document.getElementById('schemeVersion');
        if (ver) ver.textContent = data.version || '';
        var tech = document.getElementById('schemeDocTech');
        if (tech && data.docs) tech.href = data.docs.tech;
        var human = document.getElementById('schemeDocHuman');
        if (human && data.docs) human.href = data.docs.human;

        var L = layout();
        state._layout = L;
        renderStages(L);
        renderNodes(L);
        drawEdges(L);
        fitView();
    }

    function fail(msg) {
        var stage = document.getElementById('schemeStage');
        if (!stage) return;
        stage.innerHTML = '<div class="scheme-error">' + msg + '</div>';
    }

    function boot() {
        bindPanZoom();
        fetch('docs/scheme.json', { cache: 'no-store' })
            .then(function (r) {
                if (!r.ok) throw new Error('HTTP ' + r.status);
                return r.json();
            })
            .then(render)
            .catch(function (e) {
                fail('Не удалось загрузить схему (docs/scheme.json): ' + (e && e.message ? e.message : e) +
                    '. В образе нет файла схемы — обновите WebUI.');
            });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', boot);
    } else {
        boot();
    }
})();
