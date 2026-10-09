// Живой замер контраста палитры канваса по всем 7 темам (CDP, wall-стенд).
// Запуск: node webui/tests/theme-live-probe.js <cdpUrl> <pageUrl> [screenshotPath]
// Требует уже запущенный Chrome с --remote-debugging-port.
'use strict';
var path = require('path');
var chromium = require(path.join(__dirname, '..', '..', 'node_modules', 'playwright')).chromium;

var CDP = process.argv[2] || 'http://127.0.0.1:9334';
var URL = process.argv[3] || 'http://127.0.0.1:18083/monitor.html';
var SHOT = process.argv[4] || path.join(process.env.TEMP || '.', 'monitor-mint.png');
var THEMES = ['dark', 'light', 'linear', 'nvidia', 'vercel', 'sentry', 'mint'];

function probe() {
  var pal = window.MonitorApp.themePalette();
  var cs = getComputedStyle(document.documentElement);
  var bodyBg = getComputedStyle(document.body).backgroundColor;
  var v = function(n) { return (cs.getPropertyValue(n) || '').trim(); };
  var card = v('--bg-card') || v('--bg-secondary') || bodyBg;
  var M = window.MonitorApp;
  var blend = function(fg, bg) {
    var f = M.parseCssColor(fg), b = M.parseCssColor(bg);
    if (!f || !b) return null;
    var a = f.a == null ? 1 : f.a;
    return { r: f.r * a + b.r * (1 - a), g: f.g * a + b.g * (1 - a), b: f.b * a + b.b * (1 - a), a: 1 };
  };
  var c = function(fg, bg) {
    var f = blend(fg, bg), b = M.parseCssColor(bg);
    if (!f || !b) return null;
    return Math.round(M.contrastRatio(f, b) * 100) / 100;
  };
  return {
    theme: document.documentElement.getAttribute('data-theme'),
    isLight: pal.isLight,
    bodyBg: bodyBg,
    card: card,
    grid: c(pal.grid, bodyBg),
    gridStrong: c(pal.gridStrong, bodyBg),
    text: c(pal.text, bodyBg),
    textMuted: c(pal.textMuted, card),
    tileText: c(pal.tileText, card),
    lane: c(pal.lane, bodyBg),
    laneLabel: c(pal.laneLabel, bodyBg)
  };
}

(async function() {
  var browser = await chromium.connectOverCDP(CDP);
  var ctx = browser.contexts()[0];
  var page = ctx.pages()[0] || await ctx.newPage();
  await page.goto(URL, { waitUntil: 'load', timeout: 60000 });
  await page.waitForFunction(function() { return window.MonitorApp && MonitorApp.themePalette; }, null, { timeout: 30000 });
  await page.waitForTimeout(2500);

  var rows = [];
  for (var i = 0; i < THEMES.length; i++) {
    var t = THEMES[i];
    await page.evaluate(function(th) { document.documentElement.setAttribute('data-theme', th); }, t);
    await page.waitForTimeout(800);
    rows.push(await page.evaluate(probe));
  }

  var bad = [];
  rows.forEach(function(r) {
    if (r.grid < 1.15) bad.push(r.theme + ' grid=' + r.grid);
    if (r.text < 4.5) bad.push(r.theme + ' text=' + r.text);
    if (r.textMuted < 4.5) bad.push(r.theme + ' textMuted=' + r.textMuted);
    if (r.tileText < 4.5) bad.push(r.theme + ' tileText=' + r.tileText);
  });

  console.log(JSON.stringify({ url: URL, rows: rows, violations: bad }, null, 1));

  await page.evaluate(function() { document.documentElement.setAttribute('data-theme', 'mint'); });
  await page.waitForTimeout(1000);
  await page.screenshot({ path: SHOT, fullPage: false });
  console.log('screenshot: ' + SHOT);
  await browser.close();
  process.exit(bad.length === 0 ? 0 : 2);
})().catch(function(e) {
  console.error('FAIL: ' + e.message);
  process.exit(1);
});
