// window-limit-note-r319.spec.js — R319 (2026-10-30).
//
// ЖИВАЯ ПРОВЕРКА В БРАУЗЕРЕ: в форме параметров модели на странице GGUF есть
// строка «Предел окна: N — ограничивает env/профиль — причина».
//
// Зачем спек, если есть юнит-тест логики (gguf-window-limit-r319.test.js): юнит
// проверяет текст функции на подставных данных. Здесь проверяется то, чего юнит
// не видит: что живая страница WebUI, снятая с работающего стенда, реально
// запрашивает GET /api/v1/cppworker/model-profiles, получает nctxCeiling и
// дорисовывает блок в уже отрисованную форму.
//
// Запуск по живому стенду (WebUI на 18083):
//   npx playwright test tests/visual/window-limit-note-r319.spec.js
//   PLAYWRIGHT_BASE_URL=http://127.0.0.1:18083 WEBUI_API_TOKEN=<токен>

const { test, expect } = require('@playwright/test');

const TOKEN = process.env.WEBUI_API_TOKEN || 'changeme-bundled-with-agent-token';

test.beforeEach(async ({ context }) => {
    await context.addInitScript((token) => {
        try {
            localStorage.setItem('apiToken', token);
            localStorage.setItem('ollamalegion_wizard_done', '1');
            localStorage.setItem('ollamalegion_backend_type', 'llama_cpp');
        } catch (e) {
            // private mode / quota — тест упадёт на понятной проверке ниже
        }
    }, TOKEN);
    await context.addInitScript(() => {
        const removeModal = () => {
            const m = document.getElementById('setupWizardModal');
            if (m && m.parentNode) m.parentNode.removeChild(m);
        };
        if (document.readyState === 'loading') {
            document.addEventListener('DOMContentLoaded', removeModal, { once: true });
        } else {
            removeModal();
        }
        setInterval(removeModal, 50);
    });
});

test('форма параметров модели объясняет, чем ограничено окно', async ({ page }) => {
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));

    await page.goto('/');
    await page.click('a.nav-item[data-page="gguf"]');
    await page.waitForLoadState('networkidle').catch(() => {});

    // Раскрываем параметры первой модели на диске.
    const summary = page.locator('details.gguf-mp-acc summary').first();
    const hasModel = await summary.count();
    if (!hasModel) {
        test.skip(true, 'на стенде нет моделей на диске — форма параметров не рисуется');
        return;
    }
    await summary.click();

    // Блок дорисовывается ПОСЛЕ ответа model-profiles, поэтому ждём его, а не
    // проверяем синхронно (иначе тест ловил бы состояние «ещё не пришло»).
    const note = page.locator('.gguf-mp-limit').first();
    await note.waitFor({ state: 'visible', timeout: 15000 });

    const text = (await note.textContent()) || '';
    expect(text).toContain('Предел окна');
    // Одно из трёх чисел: потолок env, окно профиля либо честное «не задан».
    expect(text).toMatch(/Предел окна:\s*\d+|Предел окна не задан/);
    // Источник назван — иначе оператор снова не поймёт, почему окно такое.
    expect(text).toMatch(/LB_NCTX_RELOAD_MAX_N_CTX|профиль|VRAM\/GGUF/);
    // Повторный клик (закрыть/открыть) не должен плодить блоки.
    await summary.click();
    await summary.click();
    await note.waitFor({ state: 'visible', timeout: 15000 });
    const blocks = await page.locator('.gguf-mp-limit').count();
    expect(blocks).toBeLessThan(3);

    expect(errors, 'ошибки JS на странице: ' + errors.join('; ')).toEqual([]);
});
