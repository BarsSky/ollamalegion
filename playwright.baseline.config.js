// playwright.baseline.config.js — отдельная конфигурация для baseline-теста.
// Не пересекается с tests/visual/density.spec.js (тот требует snapshot-режим).
//
// Запуск:
//   $env:PLAYWRIGHT_BASE_URL = "http://localhost:18083"
//   $env:CPPWORKER_API_TOKEN = "changeme-..."
//   npx playwright test --config=playwright.baseline.config.js

const { defineConfig } = require('@playwright/test');

const BASE_URL = process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:18083';

module.exports = defineConfig({
    testDir: './scripts',
    testMatch: /baseline\.spec\.js$/,
    timeout: 180 * 1000,
    fullyParallel: false,
    workers: 1,
    use: {
        baseURL: BASE_URL,
        headless: true,
        viewport: { width: 1440, height: 900 },
        launchOptions: {
            args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu'],
        },
    },
    reporter: [['list'], ['json', { outputFile: 'test-results/baseline-report.json' }]],
});
