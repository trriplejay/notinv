const assert = require('node:assert/strict');
const { test } = require('node:test');
const { chromium } = require('playwright');

const baseURL = process.env.NOTINV_BROWSER_URL || 'http://127.0.0.1:18080';
const scriptName = 'checkout & inventory';
const cardSelector = '.card';

async function chartData(page) {
  return page.locator(cardSelector).filter({ has: page.getByRole('heading', { name: scriptName, exact: true }) })
    .locator('canvas').evaluate(canvas => {
      const chart = Chart.getChart(canvas);
      return {
        type: chart.options.scales.x.type,
        datasets: chart.data.datasets.map(d => ({ label: d.label, data: d.data, colors: d.pointBackgroundColor })),
        text: chart.options.scales.x.ticks.color,
        grid: chart.options.scales.x.grid.color
      };
    });
}

async function waitForSeries(page, count) {
  await page.waitForFunction(({ name, count }) => {
    const card = [...document.querySelectorAll('.card')].find(c => c.querySelector('h2').textContent === name);
    return card?.querySelector('.chart-status').textContent === `${count} requests in this window`;
  }, { name: scriptName, count });
}

test('dashboard with embedded assets, real Go API and SQLite', async t => {
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const page = await browser.newPage({ colorScheme: 'light' });
  const external = [];
  const pageErrors = [];
  page.on('request', request => {
    if (new URL(request.url()).origin !== new URL(baseURL).origin) external.push(request.url());
  });
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.clock.install();
  const response = await page.goto(baseURL);
  assert.equal(response.status(), 200);
  await waitForSeries(page, 7);
  const card = page.locator(cardSelector).filter({ has: page.getByRole('heading', { name: scriptName, exact: true }) });

  await t.test('live summary DTOs and safe text rendering', async () => {
    const scripts = await (await page.request.get(`${baseURL}/api/scripts`)).json();
    assert.equal(await page.locator(cardSelector).count(), scripts.length);
    assert.equal(await card.locator('[data-field=schedule]').textContent(), '*/5 * * * *');
    assert.equal(await card.locator('[data-field=counts]').textContent(), '1 OK / 0 failing');
    assert.equal(await card.locator('[data-field=uptime]').textContent(), '50.0%');
    assert.equal(await card.locator('[data-field=average]').textContent(), '45.0 ms');
    assert.equal(await card.locator('[data-field=p95]').textContent(), '80.0 ms');
    const latest = scripts.find(s => s.name === scriptName).latest.time;
    assert.equal(await card.locator('[data-field=lastRun]').textContent(), await page.evaluate(time => new Date(time).toLocaleString(), latest));
    const noRuns = page.locator(cardSelector).filter({ hasText: '<img src=x onerror=alert(1)>' });
    assert.equal(await noRuns.locator('[data-field=lastRun]').textContent(), 'Never run');
    assert.equal(await noRuns.locator('[data-field=counts]').textContent(), '0 OK / 0 failing');
    assert.equal(await page.locator('.card img').count(), 0);
  });

  await t.test('real Chart.js time scale, URL groups, and per-point status colors', async () => {
    const chart = await chartData(page);
    assert.equal(await page.evaluate(() => Chart.version), '4.5.1');
    assert.equal(chart.type, 'time');
    assert.deepEqual(chart.datasets.map(d => d.label), ['https://shop.example/stock', 'https://shop.example/cart']);
    const cart = chart.datasets.find(d => d.label.endsWith('/cart'));
    assert.deepEqual(cart.data.map(p => p.y), [10, 20, 30, 40]);
    assert.deepEqual(cart.colors, ['#22a579', '#6099f8', '#ef795f', '#e34c75']);
    const stock = chart.datasets.find(d => d.label.endsWith('/stock'));
    assert.deepEqual(stock.colors, ['#22a579', '#c26bf3', '#c26bf3']);
    assert.ok(cart.data.every(p => Number.isFinite(Date.parse(p.x))));
    // Includes an empty but non-null error and less common status classes.
    assert.deepEqual(await page.evaluate(() => [
      pointColor({ status: 200, error: '' }),
      pointColor({ status: 102, error: null }),
      pointColor({ status: 700, error: null })
    ]), ['#c26bf3', '#94a3b8', '#c26bf3']);
  });

  await t.test('all ranges compute RFC3339 since and redraw from the real windowed API', async () => {
    for (const [span, count] of [[3600000, 6], [604800000, 8], [86400000, 7]]) {
      const now = await page.evaluate(() => Date.now());
      const requestPromise = page.waitForRequest(r => r.url().includes('/requests/series?') && decodeURIComponent(r.url()).includes(scriptName));
      await card.getByRole('combobox').selectOption(String(span));
      const request = await requestPromise;
      const since = new URL(request.url()).searchParams.get('since');
      assert.match(since, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
      assert.ok(Math.abs(Date.parse(since) - (now - span)) < 1000);
      await waitForSeries(page, count);
      const chart = await chartData(page);
      assert.equal(chart.datasets.reduce((sum, d) => sum + d.data.length, 0), count);
    }
  });

  await t.test('30-second timer refetches summaries and every chart without losing range', async () => {
    await card.getByRole('combobox').selectOption('3600000');
    await waitForSeries(page, 6);
    const summary = page.waitForResponse(r => new URL(r.url()).pathname === '/api/scripts');
    const series = page.waitForResponse(r => r.url().includes('/requests/series?') && decodeURIComponent(r.url()).includes(scriptName));
    const otherSeries = page.waitForResponse(r => r.url().includes('/requests/series?') && !decodeURIComponent(r.url()).includes(scriptName));
    await page.clock.fastForward(30000);
    assert.equal((await summary).status(), 200);
    assert.equal((await series).status(), 200);
    assert.equal((await otherSeries).status(), 200);
    await waitForSeries(page, 6);
    assert.equal(await card.getByRole('combobox').inputValue(), '3600000');
  });

  await t.test('system dark mode changes page, card, chart surface and chart axes', async () => {
    const colors = () => page.evaluate(() => ['body', '.card', '.chart-surface'].map(selector => {
      const style = getComputedStyle(document.querySelector(selector));
      return [style.backgroundColor, style.color];
    }));
    const light = await colors();
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.waitForFunction(() => Chart.getChart(document.querySelector('canvas')).options.scales.x.ticks.color === '#a5b5ce');
    const dark = await colors();
    assert.equal(dark[0][0], 'rgb(12, 19, 32)');
    assert.equal(dark[1][0], 'rgb(22, 33, 53)');
    assert.equal(dark[2][0], 'rgb(16, 26, 43)');
    for (let i = 0; i < 3; i++) assert.notDeepEqual(dark[i], light[i]);
    assert.equal((await chartData(page)).grid, '#34445e');
    await page.emulateMedia({ colorScheme: 'light' });
    await page.waitForFunction(() => Chart.getChart(document.querySelector('canvas')).options.scales.x.ticks.color === '#53627a');
  });

  await t.test('no external requests or browser runtime errors', () => {
    assert.deepEqual(external, []);
    assert.deepEqual(pageErrors, []);
  });

  // Component boundary scenarios: substitute only the API, keeping the actual
  // page, DOM, Chart.js, adapter and browser intact.
  await t.test('empty, failed, and recovered summary states', async () => {
    await page.route('**/api/scripts', route => route.fulfill({ json: [] }));
    await page.getByRole('button', { name: 'Refresh now' }).click();
    await page.waitForFunction(() => document.querySelectorAll('.card').length === 0);
    assert.match(await page.locator('#status').textContent(), /No scripts recorded/);
    await page.unroute('**/api/scripts');
    await page.getByRole('button', { name: 'Refresh now' }).click();
    await waitForSeries(page, 7);
    await page.route('**/api/scripts', route => route.fulfill({ status: 503, body: 'unavailable' }));
    const series = page.waitForResponse(r => r.url().includes('/requests/series?') && decodeURIComponent(r.url()).includes(scriptName));
    await page.getByRole('button', { name: 'Refresh now' }).click();
    await series;
    assert.match(await page.locator('#status').textContent(), /503.*stale/);
    assert.equal(await page.locator(cardSelector).count(), 2);
    await page.unroute('**/api/scripts');
  });

  await t.test('failing latest state, fractional uptime, empty series and recovery', async () => {
    const scripts = await (await page.request.get(`${baseURL}/api/scripts`)).json();
    const changed = scripts.find(s => s.name === scriptName);
    changed.latest.ok = false;
    changed.latest.error = '<b>failure</b>';
    changed.uptime = 0.995;
    await page.route('**/api/scripts', route => route.fulfill({ json: scripts }));
    await page.route('**/requests/series?*', route => route.fulfill({ json: [] }));
    await page.getByRole('button', { name: 'Refresh now' }).click();
    await page.waitForFunction(() => [...document.querySelectorAll('.chart-status')].every(e => e.textContent === 'No requests in this window.'));
    assert.equal(await card.locator('[data-field=counts]').textContent(), '0 OK / 1 failing');
    assert.equal(await card.locator('[data-field=uptime]').textContent(), '99.5%');
    assert.equal(await card.locator('.run-error').textContent(), '<b>failure</b>');
    assert.equal(await card.locator('.run-error b').count(), 0);
    assert.deepEqual((await chartData(page)).datasets, []);
    await page.unroute('**/requests/series?*');
    await page.route('**/requests/series?*', route => route.fulfill({ status: 500, body: 'unavailable' }));
    await card.getByRole('combobox').selectOption('3600000');
    await page.waitForFunction(() => [...document.querySelectorAll('.chart-status')].some(e => e.textContent.includes('500')));
    assert.match(await card.locator('.chart-status').textContent(), /500.*stale/);
    await page.unroute('**/requests/series?*');
    await page.unroute('**/api/scripts');
    await page.getByRole('button', { name: 'Refresh now' }).click();
    await waitForSeries(page, 6);
    assert.equal(await card.locator('[data-field=uptime]').textContent(), '50.0%');
  });
});
