#!/usr/bin/env node
/*
 * theme-contrast.test.js — R95 (2026-10-09).
 *
 * ЗАПУСК: node webui/tests/theme-contrast.test.js
 *
 * ЖАЛОБА: «в мониторе при теме mint не видно сетки и подписей в блоках».
 *
 * ПРИЧИНА. Канвас (сетка, подписи внутри плиток клиентов/бэкендов, плашки «+N»)
 * выбирал цвета по ИМЕНИ темы — `data-theme === 'light'` — то есть знал ровно две
 * палитры. Все остальные темы получали тёмную: на светлом фоне mint (#f5f7f6)
 * сетка rgba(255,255,255,0.03) давала контраст 1.00, текст #e2e8f0 — 1.15,
 * подписи в блоках #fff — 1.15, то есть сливались с фоном.
 *
 * ЧТО ПРОВЕРЯЕТ ЭТОТ ТЕСТ (по всем темам из themes.css):
 *   1. тема задаёт все токены, от которых зависит читаемость (фон/текст/границы);
 *   2. контраст основного, вторичного и приглушённого текста к обеим поверхностям
 *      ≥ 4.5 (WCAG AA для обычного текста);
 *   3. палитра канваса выбирается по ФАКТИЧЕСКОМУ фону темы (чистая функция
 *      MonitorApp.paletteFor), а не по имени: для светлых тем — светлая палитра;
 *   4. в выбранной палитре текст и приглушённый текст контрастны к фону (≥ 4.5),
 *      а линии сетки заметны (контраст ≥ 1.15, иначе «сетки не видно»).
 */

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const THEMES_CSS = path.join(REPO_ROOT, 'webui', 'css', 'themes.css');
const THEMES = ['dark', 'light', 'linear', 'nvidia', 'vercel', 'sentry', 'mint'];

// --- разбор themes.css -------------------------------------------------------
const src = fs.readFileSync(THEMES_CSS, 'utf8');
const rules = [];
{
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = re.exec(src)) !== null) {
    const sel = m[1].replace(/\/\*[\s\S]*?\*\//g, '').trim();
    const vars = {};
    const vre = /(--[a-z0-9-]+)\s*:\s*([^;]+);/gi;
    let v;
    while ((v = vre.exec(m[2])) !== null) vars[v[1]] = v[2].trim();
    rules.push({ sel: sel, vars: vars });
  }
}
function themeTokens(theme) {
  const acc = {};
  for (const r of rules) {
    const applies = r.sel.indexOf(':root') !== -1 ||
      new RegExp('\\[data-theme="' + theme + '"\\]').test(r.sel);
    if (applies) Object.assign(acc, r.vars);
  }
  return acc;
}

// --- цветовая математика (независимая от проверяемого JS) --------------------
function hexToRgb(c) {
  const s = String(c || '').trim();
  const m = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(s);
  if (!m) return null;
  let h = m[1];
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)];
}
function lum(rgb) {
  const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
  return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2]);
}
function ratio(a, b) {
  const l1 = lum(a), l2 = lum(b);
  const hi = Math.max(l1, l2), lo = Math.min(l1, l2);
  return (hi + 0.05) / (lo + 0.05);
}

// --- MonitorApp.paletteFor: загружаем state.js в минимальном DOM --------------
global.window = global;
global.window.WEBUI_CONFIG = {};
global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.document = {
  getElementById() { return null; },
  addEventListener() {},
  removeEventListener() {},
  documentElement: { getAttribute() { return null; }, setAttribute() {} },
  querySelectorAll() { return []; },
  querySelector() { return null; },
  createElement() { return { style: {}, classList: { add() {}, remove() {} }, appendChild() {} }; },
};
global.window.location = { search: '', origin: 'http://127.0.0.1:18083', hash: '' };
global.location = global.window.location;
global.window.addEventListener = function () {};
global.window.removeEventListener = function () {};
global.addEventListener = global.window.addEventListener;
global.removeEventListener = global.window.removeEventListener;
vm.runInThisContext(fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'monitor', 'state.js'), 'utf8'),
  { filename: 'state.js' });
const MA = global.window.MonitorApp;
assert.ok(MA && typeof MA.paletteFor === 'function', 'MonitorApp.paletteFor должен существовать');

let checks = 0;
function check(name, fn) {
  fn();
  checks++;
  console.log('  \u2713 ' + name);
}

console.log('theme-contrast (все темы themes.css)');

// 1. Токены, от которых зависит читаемость.
const REQUIRED = ['--bg-primary', '--bg-secondary', '--bg-card', '--text-primary',
  '--text-secondary', '--text-muted', '--accent', '--success', '--warning', '--danger',
  '--border'];
check('каждая тема задаёт фон/текст/акценты/границы', function () {
  for (const t of THEMES) {
    const tk = themeTokens(t);
    const miss = REQUIRED.filter((k) => !tk[k]);
    assert.strictEqual(miss.length, 0, t + ': нет токенов ' + miss.join(' ') +
      ' — они наследуются от другой темы и дают смешанные цвета');
  }
});

// 2. Контраст текста к поверхностям.
check('текст контрастен к фону во всех темах (WCAG AA ≥ 4.5)', function () {
  const fails = [];
  for (const t of THEMES) {
    const tk = themeTokens(t);
    const surfaces = ['--bg-primary', '--bg-secondary', '--bg-card'];
    for (const fgKey of ['--text-primary', '--text-secondary', '--text-muted']) {
      const fg = hexToRgb(tk[fgKey]);
      if (!fg) { fails.push(t + ':' + fgKey + ' не hex'); continue; }
      for (const bgKey of surfaces) {
        const bg = hexToRgb(tk[bgKey]);
        if (!bg) continue;
        const r = ratio(fg, bg);
        if (r < 4.5) fails.push(t + ' ' + fgKey + '/' + bgKey + '=' + r.toFixed(2));
      }
    }
  }
  assert.strictEqual(fails.length, 0, 'низкий контраст: ' + fails.join(', '));
});

// 3. Палитра канваса — по фактическому фону темы.
check('палитра канваса выбирается по фону темы, а не по её имени', function () {
  const bad = [];
  for (const t of THEMES) {
    const tk = themeTokens(t);
    const bg = hexToRgb(tk['--bg-primary']);
    const isLightTheme = lum(bg) > 0.5;
    const pal = MA.paletteFor(tk['--bg-primary']);
    if (pal.isLight !== isLightTheme) {
      bad.push(t + ': фон ' + tk['--bg-primary'] + ' → палитра ' +
        (pal.isLight ? 'светлая' : 'тёмная') + ', ожидалась ' + (isLightTheme ? 'светлая' : 'тёмная'));
    }
  }
  assert.strictEqual(bad.length, 0, bad.join('; '));
});

// 4. В выбранной палитре текст и сетка видны на фоне темы.
check('в палитре каждой темы текст ≥ 4.5 и сетка ≥ 1.15 к фону', function () {
  const bad = [];
  for (const t of THEMES) {
    const tk = themeTokens(t);
    const bgHex = tk['--bg-primary'];
    const bg = hexToRgb(bgHex);
    const pal = MA.paletteFor(bgHex);
    const text = hexToRgb(pal.text) || hexToRgb(pal.tileText);
    const muted = hexToRgb(pal.textMuted);
    const gridLight = hexToRgb('#161a22'); // условный «цвет» сетки: берём как серый
    if (text && ratio(text, bg) < 4.5) bad.push(t + ' pal.text=' + ratio(text, bg).toFixed(2));
    if (muted && ratio(muted, bg) < 4.5) bad.push(t + ' pal.textMuted=' + ratio(muted, bg).toFixed(2));
    if (gridLight) {
      // Сетка задаётся rgba поверх фона — считаем композит и требуем заметность.
      const gm = /^rgba?\(([^)]+)\)$/.exec(pal.grid);
      let gridRgb = null;
      if (gm) {
        const p = gm[1].split(',').map(Number);
        const a = p.length > 3 ? p[3] : 1;
        gridRgb = [p[0] * a + bg[0] * (1 - a), p[1] * a + bg[1] * (1 - a), p[2] * a + bg[2] * (1 - a)];
      }
      if (gridRgb && ratio(gridRgb, bg) < 1.15) {
        bad.push(t + ' pal.grid контраст=' + ratio(gridRgb, bg).toFixed(3) + ' (сетка сливается)');
      }
    }
  }
  assert.strictEqual(bad.length, 0, bad.join(', '));
});

// 5. Регресс на конкретную жалобу: mint — светлая тема.
check('mint получает светлую палитру (регресс на «не видно сетки и подписей»)', function () {
  const tk = themeTokens('mint');
  const pal = MA.paletteFor(tk['--bg-primary']);
  assert.strictEqual(pal.isLight, true, 'mint обязан получать светлую палитру (фон ' + tk['--bg-primary'] + ')');
  const bg = hexToRgb(tk['--bg-primary']);
  assert.ok(ratio(hexToRgb(pal.tileText), bg) >= 4.5,
    'подписи внутри блоков в mint не контрастны: ' + pal.tileText);
});

console.log('\nOK: ' + checks + ' checks passed по ' + THEMES.length + ' темам');
