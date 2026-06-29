// OllamaLegion — Playwright visual-regression config
// Run:    npm run test:visual        (compare against baseline)
// Update: npm run test:visual:update (regenerate baseline)
//
// WebUI served by `cmd/balancer` (admin port 18081 by default) or by any
// static server pointed at the `webui/` directory. The URL is taken from
// PLAYWRIGHT_BASE_URL, falling back to the bundled-stub stack (port 18083).
//
// To run against a stub stack:
//   cd deployments && docker compose -f docker-compose.test-stub.yml up -d
//   npm run test:visual
//
// To run against the local webui served by any HTTP server:
//   npx http-server webui -p 18083 &
//   npm run test:visual

const { defineConfig, devices } = require('@playwright/test');

const BASE_URL = process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:18083';

module.exports = defineConfig({
    testDir: './tests/visual',
    // Allow time for the in-process WebUI to settle theme/density/i18n.
    timeout: 30 * 1000,
    expect: {
        // Pixel-level tolerance — see Sprint 1 Day 3 baseline screenshots.
        // 0.2% (≈ 200 px of a 100 000-px screenshot) accommodates anti-aliasing
        // and font-rendering drift between Windows hosts.
        toHaveScreenshot: {
            maxDiffPixelRatio: 0.002,
            animations: 'disabled',
            caret: 'hide',
        },
    },
    fullyParallel: true,
    forbidOnly: !!process.env.CI,
    retries: process.env.CI ? 2 : 0,
    // Serial workers — visual-regression runs are I/O bound and we don't want
    // many concurrent Chromium instances fighting for CPU during baseline
    // generation.
    workers: 1,
    reporter: process.env.CI ? 'github' : [['list'], ['html', { open: 'never' }]],
    use: {
        baseURL: BASE_URL,
        trace: 'retain-on-failure',
        viewport: { width: 1440, height: 900 },
        // The data-density variant is toggled by the user via a button, so
        // tests need to drive the toggle and wait for the new `data-density`
        // attribute before screenshotting.
        actionTimeout: 5 * 1000,
    },
    projects: [
        {
            name: 'light',
            use: {
                ...devices['Desktop Chrome'],
                colorScheme: 'light',
            },
        },
        {
            name: 'dark',
            use: {
                ...devices['Desktop Chrome'],
                colorScheme: 'dark',
            },
        },
    ],
    // We do NOT auto-start a webServer here — the WebUI is normally served
    // by the bundled stub stack (`deployments/docker-compose.test-stub.yml`).
    // Setting `webServer` is intentionally left commented out so that the
    // developer controls *when* the stack is up (faster inner loop, no port
    // conflicts when iterating on CSS).
    //
    // webServer: {
    //     command: 'go run -tags llama_stub ./cmd/balancer -port 18081',
    //     url: 'http://localhost:18081/api/v1/ping',
    //     timeout: 30_000,
    //     reuseExistingServer: !process.env.CI,
    // },
});