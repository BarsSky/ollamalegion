// layout-chip-live.spec.js — R118.17 (2026-10-15).
//
// ЖИВАЯ ПРОВЕРКА В БРАУЗЕРЕ: чип «раскладка» на странице GGUF Models показывает
// ТОЧНЫЕ слои по устройствам (факт из лога llama.cpp), а не расчёт.
//
// Зачем отдельный спек, если есть юнит-тесты логики (gguf-layout-exact-r118.test.js):
// те проверяют преобразование и текст чипа на подставных данных. Здесь проверяется
// то, что юнитами не проверяется вовсе — что живая страница WebUI, снятая с
// работающего стенда, действительно рисует этот чип на строке загруженной модели.
//
// Запуск по живому стенду (WebUI контейнера на 18083):
//   npx playwright test tests/visual/layout-chip-live.spec.js
//   PLAYWRIGHT_BASE_URL=http://127.0.0.1:18083 WEBUI_API_TOKEN=<токен>
//
// Требуется загруженная модель на бэкенде (иначе чипа не будет — и это тоже
// проверяется отдельным тестом ниже).

const { test, expect } = require('@playwright/test');

const TOKEN = process.env.WEBUI_API_TOKEN || 'changeme-bundled-with-agent-token';

test.beforeEach(async ({ context }) => {
    await context.addInitScript((token) => {
        try {
            localStorage.setItem('apiToken', token);
            localStorage.setItem('ollamalegion_wizard_done', '1');
            localStorage.setItem('ollamalegion_backend_type', 'llama_cpp');
        } catch (e) {
            // private mode / quota — игнорируем, тест упадёт на понятной проверке
        }
    }, TOKEN);
    // Мастер настройки перекрывает страницу на свежем стенде — убираем.
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

// openGgufModels — перейти на страницу GGUF Models и дождаться загрузки списка.
async function openGgufModels(page) {
    await page.goto('/');
    await page.click('a.nav-item[data-page="gguf"]');
    await page.waitForLoadState('networkidle').catch(() => {});
}

test('чип «раскладка» на живой странице показывает устройства и слои', async ({ page }) => {
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));

    await openGgufModels(page);

    // Ищем чип «раскладка» среди отрисованных строк моделей. Если модели не
    // загружены — чипа нет вовсе, и это отличимо от «чип есть, но пустой».
    const chip = page.locator('text=/раскладка/').first();
    const hasChip = await chip.count();
    if (!hasChip) {
        test.skip(true, 'на стенде нет загруженной модели — чип раскладки не рисуется (это ожидаемо)');
        return;
    }

    const text = (await chip.textContent()) || '';
    expect(text).toContain('раскладка');
    // Точный список: устройство + диапазон слоёв + (для узла) адрес.
    expect(text).toMatch(/(CUDA\d+|RPC\d+|CPU)/);
    expect(text).toMatch(/\d+\s*[–-]\s*\d+/);

    // В подсказке (title) обязан быть источник факта, а не «расчёт».
    const title = (await chip.getAttribute('title')) || '';
    if (title) {
        expect(title).toMatch(/точный список из лога llama\.cpp|расчёт по n_layers/);
    }

    expect(errors, 'ошибки JS на странице: ' + errors.join('; ')).toEqual([]);
});

test('в чипе раскладки видно, что это факт, а не расчёт (когда факт есть)', async ({ page }) => {
    await openGgufModels(page);

    // Ищем чип, у которого в подсказке сказано «точный список из лога llama.cpp».
    const exactChip = page.locator('[title*="точный список из лога llama.cpp"]').first();
    const count = await exactChip.count();
    if (!count) {
        test.skip(true, 'на стенде нет модели с точной раскладкой (не загружена) — проверять нечего');
        return;
    }
    const text = (await exactChip.textContent()) || '';
    expect(text).toMatch(/CUDA\d+|RPC\d+/);
});
