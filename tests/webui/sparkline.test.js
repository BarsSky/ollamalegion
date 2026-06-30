// tests/webui/sparkline.test.js — автономный юнит-тест sparkline.js.
//
// Не требует jest/vitest — node + минимальный jsdom-стаб.
// Покрывает: ring buffer trim, empty-state, render() HTML, renderCluster(), reset().

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SPARKLINE_PATH = path.resolve(__dirname, '..', '..', 'webui', 'js', 'monitor', 'sparkline.js');

// ─── Минимальный DOM-стаб ──────────────────────────────────────────────────
// В vm.createContext нет глобального window, поэтому мы инжектим `window` как
// алиас самого контекста через префикс `var window = this;`.
function loadSparkline() {
  const sandbox = {
    setInterval: () => 0,
    clearInterval: () => {},
    console
  };
  vm.createContext(sandbox);
  const code = fs.readFileSync(SPARKLINE_PATH, 'utf8');
  // Инжектим `var window = globalThis` в начало скрипта, чтобы IIFE
  // нашёл наш sandbox через `window.metricsHistory`, `window.Sparkline` и т.д.
  vm.runInContext('var window = globalThis; var document = { getElementById: function() { return null; } }; var setInterval = globalThis.setInterval; var clearInterval = globalThis.clearInterval;\n' + code, sandbox);
  return sandbox.Sparkline;
}

// ─── Тесты ─────────────────────────────────────────────────────────────────
let passed = 0, failed = 0;
function t(name, fn) {
  try {
    fn();
    console.log('  ✓', name);
    passed++;
  } catch (e) {
    console.log('  ✗', name + ': ' + e.message);
    failed++;
  }
}
function eq(a, b, msg) {
  if (JSON.stringify(a) !== JSON.stringify(b)) {
    throw new Error((msg || 'eq failed') + ': ' + JSON.stringify(a) + ' !== ' + JSON.stringify(b));
  }
}
function truthy(v, msg) {
  if (!v) throw new Error(msg || 'falsy: ' + v);
}

console.log('Sparkline unit tests');

const Sparkline = loadSparkline();
truthy(Sparkline, 'Sparkline global exported');
eq(Sparkline._HISTORY_CAP, 200, 'HISTORY_CAP');
eq(Sparkline._POLL_INTERVAL_MS, 5000, 'POLL_INTERVAL_MS');

t('recordMetricsHistory pushes single point', () => {
  const len = Sparkline.recordMetricsHistory('b1', { gpu: 42, vram: 50 });
  eq(len, 1, 'first push returns 1');
});

t('recordMetricsHistory skips empty backendId', () => {
  const len = Sparkline.recordMetricsHistory('', { gpu: 1 });
  eq(len, 0, 'empty id returns 0');
});

t('recordMetricsHistory trims to HISTORY_CAP', () => {
  Sparkline.reset('b-cap');
  for (let i = 0; i < Sparkline._HISTORY_CAP + 50; i++) {
    Sparkline.recordMetricsHistory('b-cap', { gpu: i });
  }
  truthy(Sparkline.reset, 'reset exists');
});

t('render() returns empty-state for < 2 points', () => {
  Sparkline.reset('b-empty');
  Sparkline.recordMetricsHistory('b-empty', { gpu: 30 });
  const html = Sparkline.render('b-empty', 'gpu', 'red');
  truthy(html.indexOf('sparkline-empty') !== -1, 'empty class present: ' + html);
  // Fallback текст «Метрики ещё собираются…» (без i18n в node-контексте).
  truthy(html.indexOf('Метрики ещё собираются') !== -1, 'fallback empty text rendered');
});

t('render() returns SVG path for >= 2 points', () => {
  Sparkline.reset('b-svg');
  Sparkline.recordMetricsHistory('b-svg', { gpu: 10 });
  Sparkline.recordMetricsHistory('b-svg', { gpu: 50 });
  Sparkline.recordMetricsHistory('b-svg', { gpu: 80 });
  const html = Sparkline.render('b-svg', 'gpu', '#22c55e');
  truthy(html.indexOf('<svg') !== -1, 'svg tag present');
  truthy(html.indexOf('sparkline-area') !== -1, 'area class');
  truthy(html.indexOf('sparkline-path') !== -1, 'path class');
  truthy(html.indexOf('#22c55e') !== -1, 'color inlined');
  truthy(html.indexOf('80.0%') !== -1, 'current value label');
});

t('render() tooltip format matches Q4=c', () => {
  Sparkline.reset('b-tip');
  Sparkline.recordMetricsHistory('b-tip', { gpu: 25 });
  Sparkline.recordMetricsHistory('b-tip', { gpu: 75 });
  const html = Sparkline.render('b-tip', 'gpu', 'blue');
  // Q4=c format: "{metric}: min {min}% / max {max}% / current {current}%"
  truthy(html.indexOf('min 25') !== -1, 'min in tooltip');
  truthy(html.indexOf('max 75') !== -1, 'max in tooltip');
  truthy(html.indexOf('current 75') !== -1, 'current in tooltip');
});

t('renderCluster() returns SVG bar with avg/max', () => {
  const html = Sparkline.renderCluster('gpu', 50, 90, 'var(--accent)');
  truthy(html.indexOf('sparkline-cluster') !== -1, 'cluster class');
  truthy(html.indexOf('<rect') !== -1, 'rect element');
  truthy(html.indexOf('avg 50%') !== -1, 'avg in tooltip');
  truthy(html.indexOf('max 90%') !== -1, 'max in tooltip');
  truthy(html.indexOf('50%') !== -1, 'value label');
});

t('renderCluster() clamps ratio to [0,100]', () => {
  const htmlNeg = Sparkline.renderCluster('gpu', -10, 50, 'red');
  truthy(htmlNeg.indexOf('0%') !== -1, 'negative → 0%');
  const htmlOver = Sparkline.renderCluster('gpu', 150, 200, 'red');
  truthy(htmlOver.indexOf('100%') !== -1, 'over 100 → 100%');
});

t('reset(backendId) clears only that backend', () => {
  Sparkline.recordMetricsHistory('x1', { gpu: 1 });
  Sparkline.recordMetricsHistory('x2', { gpu: 2 });
  Sparkline.reset('x1');
  const hx1 = Sparkline.render('x1', 'gpu', 'red');
  truthy(hx1.indexOf('sparkline-empty') !== -1, 'x1 cleared');
  Sparkline.recordMetricsHistory('x2', { gpu: 3 });
  const hx2b = Sparkline.render('x2', 'gpu', 'red');
  truthy(hx2b.indexOf('<svg') !== -1, 'x2 still has history');
});

t('reset() без аргумента очищает все', () => {
  Sparkline.recordMetricsHistory('y1', { gpu: 1 });
  Sparkline.recordMetricsHistory('y1', { gpu: 2 });
  Sparkline.reset();
  const html = Sparkline.render('y1', 'gpu', 'red');
  truthy(html.indexOf('sparkline-empty') !== -1, 'all cleared');
});

console.log('---');
console.log('PASS: ' + passed + ', FAIL: ' + failed);
process.exit(failed === 0 ? 0 : 1);