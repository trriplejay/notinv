import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { chromium } from 'playwright';

const base = process.env.DASHBOARD_URL || 'http://127.0.0.1:4173';
const now = new Date('2026-10-06T06:00:00Z');
let browser;
before(async () => { browser = await chromium.launch({ headless: true }); });
after(async () => { await browser?.close(); });

const script = {
  name: 'checkout <img src=x onerror=alert(1)>',
  schedule: '@every 5m',
  latest: { time: '2026-10-06T05:59:00Z', ok: false, error: 'connection <b>refused</b>' },
  uptime: 0.9876,
  avgLatency: 42.25,
  p95Latency: 90,
};
const points = [
  { url: 'https://target.test/a', time: '2026-10-06T05:55:00Z', latency: 12, status: 200, error: null },
  { url: 'https://target.test/b', time: '2026-10-06T05:56:00Z', latency: 24, status: 404, error: null },
  { url: 'https://target.test/a', time: '2026-10-06T05:57:00Z', latency: 48, status: 302, error: null },
  { url: 'https://target.test/b', time: '2026-10-06T05:58:00Z', latency: 96, status: 503, error: null },
  { url: 'https://target.test/a', time: '2026-10-06T05:59:00Z', latency: 0, status: 0, error: 'timeout' },
];

async function loaded(page) {
  await page.waitForFunction(() => document.getElementById('status').textContent.startsWith('Updated'));
}

async function component(t, scripts = [script]) {
  const page = await browser.newPage();
  t.after(() => page.close());
  const requests = [];
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => requests.push(request.url()));
  await page.clock.install({ time: now });
  await page.route('**/api/scripts', route => route.fulfill({ json: scripts }));
  await page.route('**/requests/series?*', route => route.fulfill({ json: route.request().url().includes('/second/') ? [] : points }));
  await page.goto(base);
  await loaded(page);
  return { page, requests, errors };
}

// Component tests execute the whole page with the real vendored chart/adapter;
// only the API boundary is substituted. No copied dashboard implementation.
test('cards consume DTOs safely and each chart groups and colors real time-axis points', async t => {
  const { page, requests, errors } = await component(t, [script, {
    ...script, name: 'second', latest: null, uptime: 0, avgLatency: 0, p95Latency: 0,
  }]);
  const first = page.locator('article').first();
  assert.equal(await page.locator('article').count(), 2);
  for (const expected of [script.name, script.schedule, 'Failing', 'connection <b>refused</b>', '2 ok / 3 failing', '98.8%', '42.3 ms', '90.0 ms']) {
    assert.ok((await first.textContent()).includes(expected), expected);
  }
  assert.equal(await first.locator('time').getAttribute('datetime'), script.latest.time);
  assert.equal(await first.locator('img, b').count(), 0, 'API text must not become markup');
  assert.ok((await page.locator('article').nth(1).textContent()).includes('Not run yet'));
  assert.ok((await page.locator('article').nth(1).textContent()).includes('0.0%'));
  assert.ok((await page.locator('article').nth(1).textContent()).includes('0 ok / 0 failing'), 'counts belong to each script’s own series');
  const chart = await first.locator('canvas').evaluate(canvas => {
    const chart = Chart.getChart(canvas);
    return {
      type: chart.options.scales.x.type,
      datasets: chart.data.datasets.map(d => ({
        label: d.label, colors: d.pointBackgroundColor,
        data: d.data.map(p => ({ x: p.x.toISOString(), isDate: p.x instanceof Date, y: p.y })),
      })),
      ticks: chart.scales.x.ticks.length,
    };
  });
  assert.equal(chart.type, 'time');
  assert.ok(chart.ticks > 0, 'date adapter actually rendered the axis');
  assert.deepEqual(chart.datasets.map(d => d.label), ['https://target.test/a', 'https://target.test/b']);
  assert.deepEqual(chart.datasets[0].data, [
    { x: points[0].time.replace('Z', '.000Z'), isDate: true, y: 12 },
    { x: points[2].time.replace('Z', '.000Z'), isDate: true, y: 48 },
    { x: points[4].time.replace('Z', '.000Z'), isDate: true, y: 0 },
  ]);
  assert.equal(chart.datasets[0].colors[0], chart.datasets[0].colors[1]);
  const failureColors = [...chart.datasets[1].colors, chart.datasets[0].colors[2]];
  assert.equal(new Set(failureColors).size, 3);
  assert.ok(failureColors.every(color => color !== chart.datasets[0].colors[0]));
  assert.deepEqual(errors, []);
  assert.ok(requests.every(url => new URL(url).origin === new URL(base).origin));
  assert.ok(requests.some(url => url.endsWith('/vendor/chart.umd.min.js')));
  assert.ok(requests.some(url => url.endsWith('/vendor/chartjs-adapter-date-fns.bundle.min.js')));
  assert.ok(requests.some(url => url.includes(encodeURIComponent(script.name))));
});

test('pointColor is total and non-success classes never use success colors', async t => {
  const { page } = await component(t);
  const result = await page.evaluate(() => {
    const success = pointColor({ status: 200, error: null });
    const statuses = [-1, 0, 100, 199, 200, 299, 300, 399, 400, 499, 500, 599, 600, 999, null];
    return { success, values: statuses.map(status => ({
      status, normal: pointColor({ status, error: null }), empty: pointColor({ status, error: '' }), error: pointColor({ status, error: 'network failed' }),
    })) };
  });
  for (const value of result.values) {
    assert.match(value.normal, /^#[0-9a-f]{6}$/i);
    assert.equal(value.empty, value.normal);
    assert.notEqual(value.error, result.success);
    if (value.status >= 400) assert.notEqual(value.normal, result.success);
    if (value.status >= 200 && value.status < 400) assert.equal(value.normal, result.success);
  }
});

test('all three ranges send RFC3339 lower bounds and replace chart data', async t => {
  const { page, requests } = await component(t);
  assert.deepEqual(await page.locator('#range option').allTextContents(), ['1h', '24h', '7d']);
  assert.equal(await page.locator('#range').inputValue(), '24h');
  assert.equal(new URL(requests.find(url => url.includes('/series?'))).searchParams.get('since'), '2026-10-05T06:00:00.000Z');
  const expected = { '1h': '2026-10-06T05:00:00.000Z', '7d': '2026-09-29T06:00:00.000Z', '24h': '2026-10-05T06:00:00.000Z' };
  for (const [range, since] of Object.entries(expected)) {
    await page.route('**/requests/series?*', route => route.fulfill({ json: [points[0]] }));
    const response = page.waitForResponse(r => r.url().includes('/series?'));
    await page.selectOption('#range', range);
    const url = new URL((await response).url());
    assert.equal(url.searchParams.get('since'), since);
    await loaded(page);
    assert.ok((await page.locator('article').textContent()).includes('1 ok / 0 failing'));
    assert.equal(await page.locator('canvas').evaluate(c => Chart.getChart(c).options.scales.x.min), Date.parse(since));
    assert.equal(await page.locator('canvas').evaluate(c => Chart.getChart(c).data.datasets[0].data.length), 1);
    assert.equal(await page.evaluate(() => Object.keys(Chart.instances).length), 1, 'old charts must be destroyed');
  }
});

test('30-second interval refetches cards and series without navigation', async t => {
  const { page, requests } = await component(t);
  let navigations = 0;
  page.on('framenavigated', () => navigations++);
  await page.route('**/api/scripts', route => route.fulfill({ json: [{ ...script, schedule: '@hourly', latest: { ...script.latest, ok: true, error: null } }] }));
  await page.route('**/requests/series?*', route => route.fulfill({ json: [] }));
  const before = requests.filter(url => url.includes('/series?')).length;
  await page.clock.fastForward(29999);
  assert.equal(requests.filter(url => url.includes('/series?')).length, before);
  const response = page.waitForResponse(r => r.url().includes('/series?'));
  await page.clock.fastForward(1);
  await response;
  await loaded(page);
  const text = await page.locator('article').textContent();
  for (const expected of ['@hourly', 'Passing', 'OK', '0 ok / 0 failing', 'No requests in this window.']) assert.ok(text.includes(expected));
  assert.equal(navigations, 0);
});

test('dark preference changes page, card, chart and text palette', async t => {
  const { page } = await component(t);
  const palette = () => page.evaluate(() => ['body', 'article', '.chart'].map(selector => {
    const style = getComputedStyle(document.querySelector(selector));
    return [style.backgroundColor, style.color];
  }));
  await page.emulateMedia({ colorScheme: 'light' });
  const light = await palette();
  await page.emulateMedia({ colorScheme: 'dark' });
  const dark = await palette();
  for (let i = 0; i < light.length; i++) {
    assert.notEqual(light[i][0], dark[i][0]);
    assert.notEqual(light[i][1], dark[i][1]);
  }
});

test('API failures are visible and never represented as successful zero counts', async t => {
  const { page } = await component(t);
  await page.route('**/requests/series?*', route => route.fulfill({ status: 503, body: 'unavailable' }));
  await page.selectOption('#range', '1h');
  await loaded(page);
  assert.ok((await page.locator('article').textContent()).includes('Request history unavailable: HTTP 503'));
  assert.ok((await page.locator('article').textContent()).includes('Unavailable'));
  assert.equal(await page.evaluate(() => Object.keys(Chart.instances).length), 0);
  await page.route('**/api/scripts', route => route.fulfill({ status: 500, body: 'unavailable' }));
  await page.selectOption('#range', '7d');
  await page.waitForFunction(() => document.getElementById('status').textContent.includes('Could not refresh: HTTP 500'));
  assert.equal(await page.locator('article').count(), 1, 'retain prior data with stale warning');
});

test('empty API shows an honest no-scripts state', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  await page.route('**/api/scripts', route => route.fulfill({ json: [] }));
  await page.goto(base);
  await page.waitForFunction(() => document.getElementById('status').textContent.includes('No scripts have reported yet'));
  assert.equal(await page.locator('article').count(), 0);
});

// Assembly test: real browser -> embedded HTTP page -> real API handlers -> SQLite.
// The Go test server seeds the database; there is no request interception here.
test('dashboard renders data from the real API and SQLite store', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const external = [];
  const errors = [];
  page.on('request', r => { if (new URL(r.url()).origin !== new URL(base).origin) external.push(r.url()); });
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(base);
  await loaded(page);
  const text = await page.locator('article').textContent();
  for (const expected of ['browser-check', '@every 1m', 'Failing', 'upstream unavailable', '2 ok / 1 failing', '75.0%', '50.0 ms', '80.0 ms']) assert.ok(text.includes(expected), expected);
  assert.equal(await page.locator('canvas').evaluate(c => Chart.getChart(c).data.datasets.length), 2);
  await page.selectOption('#range', '1h');
  await loaded(page);
  assert.ok((await page.locator('article').textContent()).includes('2 ok / 1 failing'));
  assert.deepEqual(external, []);
  assert.deepEqual(errors, []);
});
