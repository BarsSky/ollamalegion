// scripts/webui-audit-layout.js - R239 (2026-10-26).
//
// ЖИВАЯ ПРОВЕРКА ВЁРСТКИ WebUI (не скриншот "на глаз", а измерения).
//
// Зачем: жалобы «элементы выглядят разрозненно», «шрифт угловат», «не везде
// отступы» невозможно принимать глазами - они проявляются в цифрах:
//   * горизонтальный оверфлоу (документ шире окна);
//   * контролы (кнопки/селекты/поля) с разной высотой в одной строке;
//   * текст, отрисованный НЕ системным sans (UA-стили форм дают Arial);
//   * размеры шрифта вне типографической шкалы тем;
//   * карточки с разными радиусами/паддингами;
//   * заголовок страницы, который переносится на вторую строку.
//
// Скрипт проходит по всем страницам, собирает замеры, печатает отчёт и
// складывает JSON + опционально скриншоты. Код возврата 1, если есть жёсткие
// нарушения (оверфлоу, чужой шрифт, перенос заголовка).
//
// Запуск:
//   node scripts/webui-audit-layout.js
//   WEBUI=http://127.0.0.1:18083 node scripts/webui-audit-layout.js
//   WEBUI=https://demo-host/ node scripts/webui-audit-layout.js          (демо, без входа)
//   DENSITY=dense node scripts/webui-audit-layout.js                     (компактный режим)
//   SHOTS=1 node scripts/webui-audit-layout.js                           (плюс скриншоты)
//   WIDTH=390 HEIGHT=844 node scripts/webui-audit-layout.js              (мобильная вёрстка)
//
// Логин: WEBUI_USER / WEBUI_PASSWORD. Если не заданы и рядом есть
// deployments/.env, берутся LB_ADMIN_USER / LB_ADMIN_PASSWORD.
'use strict';

const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright');

const WEBUI = (process.env.WEBUI || 'http://127.0.0.1:18083').replace(/\/+$/, '');
const OUT = process.env.OUT_DIR || path.join('dist', 'audit-layout');
const WIDTH = Number(process.env.WIDTH || 1440);
const HEIGHT = Number(process.env.HEIGHT || 900);
const DENSITY = process.env.DENSITY === 'dense' ? 'dense' : 'normal';
const SHOTS = process.env.SHOTS === '1' || process.env.SHOTS === 'true';
const PW_CHANNEL = process.env.PW_CHANNEL || 'msedge';
const LANG = process.env.AUDIT_LANG || 'ru';
const THEME = process.env.AUDIT_THEME === 'light' ? 'light' : 'dark';

// Страницы WebUI: [подпись, значение data-page у пункта навигации].
const PAGES = [
  ['dashboard', 'dashboard'],
  ['backends', 'backends'],
  ['models', 'models'],
  ['sessions', 'sessions'],
  ['queue', 'queue'],
  ['gguf', 'gguf'],
  ['image', 'image'],
  ['agents', 'agents'],
  ['logs', 'logs'],
  ['settings', 'settings']
];

function envFileValue(file, key) {
  try {
    const txt = fs.readFileSync(file, 'utf8');
    const m = txt.match(new RegExp('^' + key + '=(.*)$', 'm'));
    return m ? m[1].trim() : '';
  } catch (e) {
    return '';
  }
}

const USER = process.env.WEBUI_USER || envFileValue(path.join('deployments', '.env'), 'LB_ADMIN_USER');
const PASSWORD = process.env.WEBUI_PASSWORD || envFileValue(path.join('deployments', '.env'), 'LB_ADMIN_PASSWORD');

let hardFails = 0;
let warns = 0;

function pass(what, detail) {
  console.log('  PASS  ' + what + (detail ? ' :: ' + detail : ''));
}
function warn(what, detail) {
  warns++;
  console.log('  WARN  ' + what + (detail ? ' :: ' + detail : ''));
}
function fail(what, detail) {
  hardFails++;
  console.log('  FAIL  ' + what + (detail ? ' :: ' + detail : ''));
}

async function launchBrowser() {
  try {
    return await chromium.launch({ channel: PW_CHANNEL, headless: true });
  } catch (e) {
    console.log('  канал ' + PW_CHANNEL + ' недоступен, беру встроенный chromium');
    return await chromium.launch({ headless: true });
  }
}

// Замер выполняется внутри страницы: только то, что реально нарисовано.
function measurePage(tokens) {
  const san = (s) => String(s || '').toLowerCase().replace(/["']/g, '').trim();
  const firstFam = (s) => san(String(s || '').split(',')[0]);

  const root = getComputedStyle(document.documentElement);
  const bodyStyle = getComputedStyle(document.body);
  const stackTokens = (s) => String(s || '').split(',').map(t => san(t)).filter(Boolean);
  // Шрифт считается своим, если он из любого токен-стека (монитор объявляет
  // свой body-стек, поэтому сравнение только по первому токену давало ложняк).
  const allowedFams = new Set(
    stackTokens(bodyStyle.fontFamily).concat(stackTokens(root.getPropertyValue('--font-family-sans')))
  );
  const sansFirst = firstFam(bodyStyle.fontFamily);
  const monoStack = root.getPropertyValue('--font-family-mono') || '';
  const monoFirst = firstFam(monoStack);

  const visible = (el) => {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    if (r.width < 2 || r.height < 2) return false;
    if (cs.visibility === 'hidden' || cs.display === 'none' || Number(cs.opacity) < 0.05) return false;
    if (el.closest('[hidden], .hidden, canvas, script, style')) return false;
    return true;
  };

  const hasOwnText = (el) => Array.from(el.childNodes).some(
    (n) => n.nodeType === 3 && n.textContent.replace(/[\s\-–\u2014·|/]/g, '').length > 1
  );

  const describe = (el) => {
    const cls = String(el.className || '').trim().split(/\s+/).filter(Boolean).slice(0, 2).join('.');
    return el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (cls ? '.' + cls : '');
  };

  const isMono = (el, fam) => {
    if (fam === monoFirst) return true;
    if (/mono|courier|consolas|menlo/.test(fam)) return true;
    if (el.closest('pre, code, kbd, samp, .mono, [class*="mono"], .log-line, .logs-table')) return true;
    return false;
  };

  // --- горизонтальный оверфлоу ---
  const vw = window.innerWidth;
  const docW = Math.max(
    document.documentElement.scrollWidth,
    document.body ? document.body.scrollWidth : 0
  );
  const overflowOffenders = [];
  if (docW > vw + 1) {
    document.querySelectorAll('body *').forEach((el) => {
      if (overflowOffenders.length >= 6) return;
      if (!visible(el)) return;
      const r = el.getBoundingClientRect();
      if (r.right > vw + 2 && r.width > 20) {
        overflowOffenders.push({ sel: describe(el), right: Math.round(r.right) });
      }
    });
  }

  // --- типографика ---
  const fams = {};
  const sizes = {};
  const weights = {};
  const foreignFonts = [];
  const oddSizes = [];
  const allowedSizes = tokens.sizes;
  document.querySelectorAll('body *').forEach((el) => {
    if (!visible(el) || !hasOwnText(el)) return;
    const cs = getComputedStyle(el);
    const fam = firstFam(cs.fontFamily);
    fams[fam] = (fams[fam] || 0) + 1;
    sizes[cs.fontSize] = (sizes[cs.fontSize] || 0) + 1;
    weights[cs.fontWeight] = (weights[cs.fontWeight] || 0) + 1;
    if (!isMono(el, fam) && !allowedFams.has(fam)) {
      if (foreignFonts.length < 8) {
        foreignFonts.push({ sel: describe(el), fam, text: (el.textContent || '').trim().slice(0, 32) });
      }
    }
    const px = Math.round(parseFloat(cs.fontSize));
    if (allowedSizes.indexOf(px) < 0 && oddSizes.length < 10) {
      oddSizes.push({ sel: describe(el), size: cs.fontSize, text: (el.textContent || '').trim().slice(0, 24) });
    }
  });

  // --- геометрия контролов ---
  const CONTROL_SEL = [
    '.btn', '.btn-sm', '.btn-lg', '.form-control', '.lang-select', '.search-input',
    '.btn-theme-toggle', '.data-freshness-toggle', '.data-freshness', '.density-toggle',
    'select', 'input:not([type="checkbox"]):not([type="radio"]):not([type="hidden"]):not([type="file"])'
  ].join(',');
  const heights = [];
  const heightOutliers = [];
  document.querySelectorAll(CONTROL_SEL).forEach((el) => {
    if (!visible(el)) return;
    if (el.closest('.notifications-dropdown, .modal:not(.active), .toast')) return;
    if (el.classList.contains('btn-link')) return;
    const r = el.getBoundingClientRect();
    const h = Math.round(r.height);
    heights.push(h);
    const ok = tokens.controlHeights.some((allow) => Math.abs(h - allow) <= 1);
    if (!ok && heightOutliers.length < 10) {
      heightOutliers.push({ sel: describe(el), h, text: (el.textContent || '').trim().slice(0, 24) });
    }
  });

  // --- карточки ---
  const radii = {};
  const pads = {};
  document.querySelectorAll('.card, .metric-card, .settings-card, .capacity-card, .panel').forEach((el) => {
    if (!visible(el)) return;
    const cs = getComputedStyle(el);
    radii[cs.borderRadius] = (radii[cs.borderRadius] || 0) + 1;
    pads[cs.padding] = (pads[cs.padding] || 0) + 1;
  });
  const cardOutliers = [];
  document.querySelectorAll('.card, .metric-card, .settings-card, .capacity-card').forEach((el) => {
    if (!visible(el)) return;
    const cs = getComputedStyle(el);
    if (tokens.cardRadii.indexOf(cs.borderRadius) < 0 && cardOutliers.length < 8) {
      const q = el.getBoundingClientRect();
      cardOutliers.push({ sel: describe(el), radius: cs.borderRadius, padding: cs.padding,
        rect: [Math.round(q.width), Math.round(q.height)],
        page: el.closest('.page') ? el.closest('.page').id : null,
        detached: !document.contains(el), display: cs.display });
    }
  });

  // --- «белые» контролы в тёмной теме (UA-фон вместо токена) ---
  const whiteControls = [];
  if (tokens.theme === 'dark') {
    document.querySelectorAll('input, select, textarea').forEach((el) => {
      if (!visible(el) || whiteControls.length >= 6) return;
      if (getComputedStyle(el).backgroundColor === 'rgb(255, 255, 255)') {
        whiteControls.push({ sel: describe(el), tag: el.tagName });
      }
    });
  }

  // --- подпись и поле в одной строке (текст «впритык» к контролу) ---
  // Классический дефект вёрстки: label обнимает input, input inline-block, и
  // текст подписи оказывается слева от поля без зазора. Поле должно быть
  // блочным, чтобы подпись стояла над ним.
  const inlineLabels = [];
  document.querySelectorAll('.form-control, select.form-control, input.form-control').forEach((el) => {
    if (!visible(el) || inlineLabels.length >= 6) return;
    const cs = getComputedStyle(el);
    if (cs.display.indexOf('inline') !== 0) return;
    let prev = el.previousSibling;
    let hasLabelText = false;
    while (prev) {
      if (prev.nodeType === 3 && /[A-Za-zА-Яа-яЁё]/.test(prev.textContent)) { hasLabelText = true; break; }
      if (prev.nodeType === 1) { break; }
      prev = prev.previousSibling;
    }
    if (hasLabelText) { inlineLabels.push({ sel: describe(el) }); }
  });

  // --- заголовок страницы ---
  const title = document.querySelector('.header h1');
  let headerTitle = null;
  if (title && visible(title)) {
    const cs = getComputedStyle(title);
    const lh = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.2;
    headerTitle = {
      text: (title.textContent || '').trim(),
      height: Math.round(title.getBoundingClientRect().height),
      lineHeight: Math.round(lh),
      wrapped: title.getBoundingClientRect().height > lh * 1.6,
      truncated: title.scrollWidth > title.clientWidth + 1
    };
  }

  return {
    vw, docW, overflowOffenders,
    sansFirst, monoFirst, fams, sizes, weights, foreignFonts, oddSizes,
    controlHeights: heights, heightOutliers,
    radii, pads, cardOutliers, whiteControls, inlineLabels,
    headerTitle
  };
}

async function readTokens(page) {
  return page.evaluate(() => {
    const root = getComputedStyle(document.documentElement);
    const num = (name, fallback) => {
      const v = parseFloat(root.getPropertyValue(name));
      return Number.isFinite(v) ? v : fallback;
    };
    const sizes = ['--font-size-xs', '--font-size-sm', '--font-size-base', '--font-size-md',
      '--font-size-lg', '--font-size-xl', '--font-size-2xl', '--font-size-3xl', '--font-size-4xl']
      .map((n) => Math.round(num(n, 0)));
    const controlHeights = ['--control-height-sm', '--control-height', '--control-height-lg']
      .map((n) => Math.round(num(n, 0)));
    const radiusOf = (name) => {
      const probe = document.createElement('div');
      probe.style.borderRadius = 'var(' + name + ')';
      document.body.appendChild(probe);
      const v = getComputedStyle(probe).borderRadius;
      probe.remove();
      return v;
    };
    const cardRadii = [radiusOf('--card-radius'), radiusOf('--radius-md'), radiusOf('--radius-lg')];
    return { sizes, controlHeights, cardRadii };
  });
}

// Общие правила для любой страницы (index-страницы и monitor.html).
function checkPage(report, label, m, tokens) {
  report.pages.push({ page: label, ...m });

  if (m.docW > m.vw + 1) {
    fail('нет горизонтального оверфлоу', 'ширина документа ' + m.docW + ' > окна ' + m.vw +
      (m.overflowOffenders.length ? '; ' + m.overflowOffenders.map((o) => o.sel + ' right=' + o.right).join(', ') : ''));
  } else {
    pass('нет горизонтального оверфлоу', m.vw + 'px');
  }

  if (m.foreignFonts.length) {
    fail('весь текст на системном sans', m.foreignFonts.map((f) => f.sel + ' -> ' + f.fam).join(', '));
  } else {
    pass('весь текст на системном sans', Object.keys(m.fams).join(', '));
  }

  if (m.whiteControls && m.whiteControls.length) {
    warn('контролы не белые в тёмной теме',
      m.whiteControls.map((o) => o.sel).join(', '));
  } else if (tokens.theme === 'dark') {
    pass('контролы не белые в тёмной теме', 'ok');
  }

  if (m.inlineLabels && m.inlineLabels.length) {
    warn('подпись стоит над полем, а не в одной строке',
      m.inlineLabels.map((o) => o.sel).join(', '));
  } else {
    pass('подпись стоит над полем', 'ok');
  }

  if (m.headerTitle && m.headerTitle.wrapped) {
    fail('заголовок страницы в одну строку', '"' + m.headerTitle.text + '" height=' +
      m.headerTitle.height + ' line-height=' + m.headerTitle.lineHeight);
  } else if (m.headerTitle && m.headerTitle.truncated) {
    fail('заголовок страницы не обрезан', '"' + m.headerTitle.text + '"');
  } else if (m.headerTitle) {
    pass('заголовок страницы целиком', '"' + m.headerTitle.text + '"');
  }

  if (m.heightOutliers.length) {
    warn('высоты контролов в шкале ' + tokens.controlHeights.join('/'),
      m.heightOutliers.map((o) => o.sel + '=' + o.h).join(', '));
  } else {
    const uniq = Array.from(new Set(m.controlHeights)).sort((a, b) => a - b);
    pass('высоты контролов в шкале', uniq.join('/') + 'px');
  }
  if (m.oddSizes.length) {
    warn('кегли в типографической шкале',
      m.oddSizes.map((o) => o.sel + '=' + o.size).join(', '));
  } else {
    pass('кегли в типографической шкале', Object.keys(m.sizes).join('/'));
  }

  if (m.cardOutliers.length) {
    warn('радиусы карточек из токенов',
      m.cardOutliers.map((o) => o.sel + '=' + o.radius).join(', '));
  } else {
    pass('радиусы карточек из токенов', Object.keys(m.radii).join(' | ') || 'нет карточек');
  }

  console.log('  INFO  паддинги карточек: ' +
    Object.entries(m.pads).sort((a, b) => b[1] - a[1]).slice(0, 5)
      .map(([k, v]) => (k || '0') + ' x' + v).join(', '));
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const report = {
    when: new Date().toISOString(),
    webui: WEBUI, viewport: WIDTH + 'x' + HEIGHT, density: DENSITY, lang: LANG, theme: THEME,
    pages: [], hardFails: 0, warns: 0
  };

  console.log('R239: аудит вёрстки WebUI :: ' + WEBUI + ' :: ' + WIDTH + 'x' + HEIGHT + ' / ' + DENSITY + ' / ' + THEME);

  const browser = await launchBrowser();
  const context = await browser.newContext({ viewport: { width: WIDTH, height: HEIGHT } });
  await context.addInitScript((cfg) => {
    try {
      localStorage.setItem('ollamalegion_lang', cfg.lang);
      localStorage.setItem('ollamalegion_theme', cfg.theme);
      localStorage.setItem('ollamalegion_density', cfg.density);
    } catch (e) { /* ignore */ }
  }, { lang: LANG, density: DENSITY, theme: THEME });

  const page = await context.newPage();
  try {
    await page.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded', timeout: 45000 });
    await page.waitForTimeout(1200);

    let authed = false;
    if (USER && PASSWORD && (await page.$('#authPassword'))) {
      await page.fill('#authName', USER);
      await page.fill('#authPassword', PASSWORD);
      await page.click('#authSubmit');
      try {
        await page.waitForFunction(
          () => window.Auth && window.Auth.state && window.Auth.state().authenticated,
          { timeout: 25000 }
        );
        authed = true;
      } catch (e) {
        console.log('  вход не подтвердился, продолжаю как гость (часть страниц будет скрыта)');
      }
    } else {
      console.log('  логин/пароль не заданы, режим гостя (демо-стенд)');
    }
    report.authed = authed;
    await page.waitForTimeout(1500);

    const tokens = await readTokens(page);
    tokens.theme = THEME;
    console.log('  токены: шрифты ' + tokens.sizes.join('/') +
      ', контролы ' + tokens.controlHeights.join('/') +
      ', радиусы ' + tokens.cardRadii.join(' | '));

    for (const [label, dataPage] of PAGES) {
      console.log('');
      console.log('=== ' + label + ' ===');
      const navSel = '.nav-item[data-page="' + dataPage + '"]';
      let nav = await page.$(navSel);
      // На узком окне сайдбар уезжает за край: открываем бургер.
      if (nav && !(await nav.isVisible())) {
        const burger = await page.$('#navBurger');
        if (burger && (await burger.isVisible())) {
          await burger.click();
          await page.waitForTimeout(500);
        }
      }
      if (!nav || !(await nav.isVisible())) {
        console.log('  SKIP  страница недоступна в этой роли');
        report.pages.push({ page: label, skipped: true });
        continue;
      }
      try {
        await page.click(navSel, { timeout: 5000 });
      } catch (e) {
        // На узком окне сайдбар перекрыт оверлеем: переключаем страницу тем же
        // методом, который вызывает обработчик клика.
        const switched = await page.evaluate((p) => {
          const fn = window.App && window.App.context && window.App.context.switchPage;
          if (!fn) return false;
          fn(p);
          return true;
        }, dataPage);
        if (!switched) {
          console.log('  SKIP  страница не открывается');
          report.pages.push({ page: label, skipped: true });
          continue;
        }
      }
      await page.waitForTimeout(1500);

      const m = await page.evaluate(measurePage, tokens);
      checkPage(report, label, m, tokens);

      if (SHOTS) {
        await page.screenshot({ path: path.join(OUT, label + '.png'), fullPage: false });
      }
    }

    // Отдельное приложение-монитор (iframe, но живёт своей вёрсткой).
    if (process.env.AUDIT_MONITOR === '1') {
      console.log('');
      console.log('=== monitor ===');
      await page.goto(WEBUI + '/monitor.html', { waitUntil: 'domcontentloaded', timeout: 45000 });
      await page.waitForTimeout(3000);
      // Монитор - плотное приложение с собственным компактным рядом высот
      // (кнопки шапки 24-28px). Токены sm/base/lg к нему не применяются, но
      // разнобой внутри этого ряда всё равно виден в отчёте.
      const monTokens = Object.assign({}, tokens, {
        controlHeights: tokens.controlHeights.concat([24, 26, 27, 28])
      });
      const m = await page.evaluate(measurePage, monTokens);
      checkPage(report, 'monitor', m, monTokens);
      if (SHOTS) {
        await page.screenshot({ path: path.join(OUT, 'monitor.png'), fullPage: false });
      }
    }

    report.hardFails = hardFails;
    report.warns = warns;
    fs.writeFileSync(path.join(OUT, 'report.json'), JSON.stringify(report, null, 2), 'utf8');
  } finally {
    await page.close();
    await browser.close();
  }

  console.log('');
  console.log('ИТОГ: жёстких нарушений ' + hardFails + ', замечаний ' + warns);
  console.log('отчёт: ' + path.join(OUT, 'report.json') + (SHOTS ? ' + скриншоты' : ''));
  if (hardFails) process.exit(1);
})().catch((e) => {
  console.error('аудит упал: ' + (e && e.stack || e));
  process.exit(2);
});
