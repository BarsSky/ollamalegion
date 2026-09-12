// scripts/diag_openwebui_playwright.js — R60.52
// Reusable Playwright script for testing OpenWebUI-style chat UIs (ai.skynas.ru, etc.)
//
// Why Playwright and not in-app Browser:
//   OpenWebUI uses TipTap (ProseMirror) contenteditable div which the
//   in-app Browser cannot fill (validation rejects programmatic input).
//   Playwright's locator.fill() handles contenteditable correctly.
//
// Usage:
//   1. npm install playwright
//   2. npx playwright install chromium
//   3. node scripts/diag_openwebui_playwright.js
//
// The script:
//   - Opens Chromium with stored OpenWebUI cookies (or fresh login)
//   - Locates the TipTap contenteditable input
//   - Fills the prompt (preserves Cyrillic, emoji, newlines)
//   - Submits via Enter or send button
//   - Waits for response completion
//   - Intercepts /api/chat and /v1/chat/completions network traffic
//   - Saves request body (what OpenWebUI sends) and response preview
//   - Captures screenshot of rendered response

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const OPENWEBUI_URL = process.env.OPENWEBUI_URL || 'https://ai.skynas.ru';
const USERNAME = process.env.OPENWEBUI_USER;  // optional, for fresh login
const PASSWORD = process.env.OPENWEBUI_PASS;
const PROMPT = process.env.PROMPT || 'Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета';
const OUT_DIR = process.env.OUT_DIR || 'C:/tmp';
const WAIT_TIMEOUT_MS = 300_000;  // 5 min max

(async () => {
    console.log(`[R60.52 OpenWebUI Diagnostic]`);
    console.log(`URL: ${OPENWEBUI_URL}`);
    console.log(`Prompt length: ${PROMPT.length}`);
    console.log(`Out dir: ${OUT_DIR}`);

    fs.mkdirSync(OUT_DIR, { recursive: true });

    const browser = await chromium.launch({ headless: true });
    const context = await browser.newContext({
        ignoreHTTPSErrors: true,
        // If you have stored cookies, load them here:
        // storageState: 'C:/path/to/openwebui-auth.json',
    });

    // Storage for intercepted traffic
    const capturedRequests = [];
    const capturedResponses = [];

    context.on('request', req => {
        const url = req.url();
        if (url.includes('/api/chat') || url.includes('/v1/chat/completions') ||
            url.includes('openwebui') || url.includes('skynas')) {
            capturedRequests.push({
                ts: Date.now(),
                method: req.method(),
                url: url.replace(OPENWEBUI_URL, '[BALANCER]'),
                headers: req.headers(),
                body: req.postData() || null,
            });
        }
    });

    context.on('response', async resp => {
        const url = resp.url();
        if (url.includes('/api/chat') || url.includes('/v1/chat/completions') ||
            url.includes('openwebui') || url.includes('skynas')) {
            let body = null;
            try { body = await resp.text(); } catch (e) { body = '[stream/binary]'; }
            capturedResponses.push({
                ts: Date.now(),
                status: resp.status(),
                url: url.replace(OPENWEBUI_URL, '[BALANCER]'),
                bodyPreview: body ? body.substring(0, 1000) : null,
                bodyLength: body ? body.length : 0,
            });
        }
    });

    const page = await context.newPage();

    // 1. Navigate to OpenWebUI
    console.log(`Navigating to ${OPENWEBUI_URL}...`);
    await page.goto(OPENWEBUI_URL, { waitUntil: 'domcontentloaded', timeout: 30000 });

    // 2. Login if needed (interactive — won't auto-fill credentials)
    const isLoggedIn = await page.locator('text=/Qwen3-Instruct/i').first()
        .isVisible({ timeout: 5000 }).catch(() => false);
    if (!isLoggedIn && USERNAME && PASSWORD) {
        console.log('Attempting login...');
        await page.fill('input[type="email"]', USERNAME);
        await page.fill('input[type="password"], input[type="text"][name="password"]', PASSWORD);
        await Promise.all([
            page.waitForNavigation({ timeout: 30000 }),
            page.click('button[type="submit"]'),
        ]);
    } else if (!isLoggedIn) {
        console.log('Not logged in. Provide OPENWEBUI_USER and OPENWEBUI_PASS env vars,');
        console.log('or load cookies via storageState. Waiting up to 60s for manual login...');
        await page.waitForSelector('text=/Qwen3-Instruct/i', { timeout: 60000 });
    }

    // 3. Find the input — OpenWebUI uses TipTap contenteditable
    console.log('Locating input field (contenteditable)...');
    const inputField = page.locator('[contenteditable="true"]').first();
    await inputField.waitFor({ state: 'visible', timeout: 10000 });

    // 4. Fill the prompt — Playwright handles contenteditable correctly
    console.log(`Filling prompt (${PROMPT.length} chars)...`);
    await inputField.click();
    await inputField.fill(PROMPT);

    // Verify
    const textValue = await inputField.textContent();
    if (textValue !== PROMPT) {
        console.warn(`WARN: input value differs (lengths: ${textValue?.length} vs ${PROMPT.length})`);
    }

    // 5. Submit — try Enter key first (more reliable for OpenWebUI)
    console.log('Submitting...');
    await page.keyboard.press('Enter');
    // Fallback: click submit button
    const submitBtn = page.locator('button[type="submit"], button[aria-label*="Send"]').first();
    if (await submitBtn.isVisible({ timeout: 1000 }).catch(() => false)) {
        // Already submitted via Enter? Check if input is empty
        const stillFilled = await inputField.textContent();
        if (stillFilled === PROMPT) {
            console.log('Enter did not submit, clicking button...');
            await submitBtn.click();
        }
    }

    // 6. Wait for response — look for response completion
    console.log(`Waiting for response (max ${WAIT_TIMEOUT_MS/1000}s)...`);
    try {
        await page.waitForFunction(() => {
            // OpenWebUI shows eval_count in stats panel when done
            return document.body.innerText.includes('eval_count') ||
                   document.body.innerText.includes('копировать') ||  // Russian "copy"
                   document.body.innerText.includes('Copy');
        }, { timeout: WAIT_TIMEOUT_MS });
        console.log('Response received!');
    } catch (e) {
        console.warn('Response wait timeout — capturing current state anyway');
    }

    // 7. Capture screenshot of response
    await page.waitForTimeout(3000);  // let stream finish
    await page.screenshot({ path: path.join(OUT_DIR, 'openwebui_response.png'), fullPage: false });
    console.log('Screenshot saved');

    // 8. Save captured network traffic
    fs.writeFileSync(
        path.join(OUT_DIR, 'openwebui_captured_requests.json'),
        JSON.stringify(capturedRequests, null, 2)
    );
    fs.writeFileSync(
        path.join(OUT_DIR, 'openwebui_captured_responses.json'),
        JSON.stringify(capturedResponses.slice(0, 5), null, 2)  // first 5 only
    );

    console.log(`\n=== CAPTURED ${capturedRequests.length} REQUESTS ===`);
    capturedRequests.forEach((r, i) => {
        console.log(`\n[REQ ${i}] ${r.method} ${r.url}`);
        if (r.body) {
            console.log(`  Body (${r.body.length} bytes):`);
            try {
                const parsed = JSON.parse(r.body);
                console.log(`    ${JSON.stringify(parsed, null, 2).substring(0, 500)}`);
            } catch (e) {
                console.log(`    ${r.body.substring(0, 200)}`);
            }
        }
    });

    console.log(`\n=== RESPONSE PREVIEWS (first 5) ===`);
    capturedResponses.slice(0, 5).forEach((r, i) => {
        console.log(`\n[RESP ${i}] status=${r.status} length=${r.bodyLength}`);
        if (r.bodyPreview) {
            try {
                const parsed = JSON.parse(r.bodyPreview);
                console.log(`    ${JSON.stringify(parsed, null, 2).substring(0, 500)}`);
            } catch (e) {
                console.log(`    ${r.bodyPreview.substring(0, 300)}`);
            }
        }
    });

    await browser.close();
    console.log('\nDone. Captured files:');
    console.log(`  ${OUT_DIR}/openwebui_captured_requests.json`);
    console.log(`  ${OUT_DIR}/openwebui_captured_responses.json`);
    console.log(`  ${OUT_DIR}/openwebui_response.png`);
})().catch(e => {
    console.error('FATAL:', e.message);
    console.error(e.stack);
    process.exit(1);
});
