const assert = require('node:assert/strict');
const { test } = require('node:test');
const { chromium } = require('playwright');

const baseURL = process.env.NOTINV_BROWSER_URL || 'http://127.0.0.1:18080';
const scriptName = 'checkout & inventory';

test('script pages and slim cards with the real Go API and SQLite', async t => {
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(baseURL);
  await page.waitForFunction(() => document.querySelector('.card h2 a'));
  const scripts = await (await page.request.get(`${baseURL}/api/scripts`)).json();

  await t.test('titles have distinct encoded destinations and cards contain only summary fields', async () => {
    const destinations = [];
    for (const script of scripts) {
      const card = page.locator('.card').filter({ has: page.getByRole('heading', { name: script.name, exact: true }) });
      const title = card.locator('h2 a');
      const href = await title.getAttribute('href');
      assert.equal(href, '/script.html?name=' + encodeURIComponent(script.name));
      destinations.push(href);
      assert.deepEqual(await card.locator('[data-field]').evaluateAll(fields => fields.map(field => field.dataset.field)), ['counts', 'uptime']);
      assert.equal(await card.locator('[data-field=counts]').textContent(), `${script.okRuns} OK / ${script.failedRuns} failing`);
      assert.equal(await card.locator('[data-field=uptime]').textContent(), `${(script.uptime * 100).toFixed(1)}%`);
      assert.equal(await card.locator('.badge').textContent(), script.latest ? (script.latest.ok ? 'OK' : 'Failing') : 'No runs yet');
      assert.equal(await card.locator('.schedule, .run-error, [data-relative], canvas, select, .legend').count(), 0);
    }
    assert.equal(new Set(destinations).size, scripts.length);
    await page.getByRole('link', { name: scriptName, exact: true }).click();
    assert.equal(new URL(page.url()).pathname, '/script.html');
    assert.equal(new URL(page.url()).searchParams.get('name'), scriptName);
  });

  await t.test('detail fields and interactive chart retain the original data', async () => {
    await page.waitForFunction(() => document.querySelector('.chart-status')?.textContent === '7 requests in this window');
    const script = scripts.find(script => script.name === scriptName);
    assert.equal(await page.locator('.card h2').textContent(), script.name);
    assert.equal(await page.locator('[data-field=schedule]').textContent(), '*/5 * * * *');
    assert.equal(await page.locator('.badge').textContent(), 'OK');
    assert.equal(await page.locator('[data-field=counts]').textContent(), '1 OK / 1 failing');
    assert.equal(await page.locator('[data-field=uptime]').textContent(), '50.0%');
    assert.equal(await page.locator('.meter span').evaluate(meter => meter.style.width), '50%');
    assert.equal(await page.locator('[data-field=average]').textContent(), '45.0 ms');
    assert.equal(await page.locator('[data-field=p95]').textContent(), '80.0 ms');
    assert.equal(await page.locator('[data-field=lastRun]').textContent(), await page.evaluate(time => new Date(time).toLocaleString(), script.latest.time));
    assert.ok(await page.locator('[data-relative]').textContent());
    assert.deepEqual(await page.locator('.legend span').allTextContents(), ['2xx', '3xx', '4xx failure', '5xx failure', 'Request error']);
    for (const [range, count] of [['3600000', 6], ['86400000', 7], ['604800000', 8]]) {
      await page.locator('select').selectOption(range);
      await page.waitForFunction(count => document.querySelector('.chart-status').textContent === `${count} requests in this window`, count);
      assert.equal(await page.locator('canvas').evaluate(canvas => Chart.getChart(canvas).data.datasets.reduce((sum, dataset) => sum + dataset.data.length, 0)), count);
    }
  });

  await t.test('direct navigation decodes untrusted names and renders no-runs state as text', async () => {
    const name = '<img src=x onerror=alert(1)>';
    await page.goto(`${baseURL}/script.html?name=${encodeURIComponent(name)}`);
    await page.waitForFunction(() => document.querySelector('.badge')?.textContent === 'No runs yet');
    assert.equal(await page.locator('.card h2').textContent(), name);
    assert.equal(await page.locator('.card img').count(), 0);
    assert.equal(await page.locator('[data-field=lastRun]').textContent(), 'Never run');
  });

  await t.test('missing or absent names show a message, no details, and a working back link', async () => {
    for (const query of ['', '?name=not-a-known-script']) {
      await page.goto(`${baseURL}/script.html${query}`);
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Script not found'));
      assert.equal(await page.locator('.card').count(), 0);
      await page.getByRole('link', { name: 'Back to dashboard' }).click();
      assert.equal(new URL(page.url()).pathname, '/');
    }
  });

  await t.test('cross-page enable and disable updates state across landing cards and script details', async () => {
    // Navigate to landing page /
    await page.goto(baseURL);
    const card = page.locator('.card').filter({ has: page.getByRole('heading', { name: scriptName, exact: true }) });
    const cardToggle = card.locator('.toggle-btn');
    await cardToggle.waitFor({ state: 'visible' });
    assert.equal(await cardToggle.textContent(), 'Disable');

    // Click Disable on card, wait for label to become Enable
    await cardToggle.click();
    await page.waitForFunction(name => {
      const el = [...document.querySelectorAll('.card')].find(c => c.querySelector('h2 a')?.textContent === name);
      return el?.querySelector('.toggle-btn')?.textContent === 'Enable';
    }, scriptName);
    assert.equal(await cardToggle.textContent(), 'Enable');

    // Verify /api/scripts reports enabled:false
    let res = await (await page.request.get(`${baseURL}/api/scripts`)).json();
    let scriptState = res.find(s => s.name === scriptName);
    assert.equal(scriptState?.enabled, false);

    // Navigate to script page /script.html?name=<encoded name>
    await page.goto(`${baseURL}/script.html?name=${encodeURIComponent(scriptName)}`);
    const scriptToggle = page.locator('#toggle');
    await scriptToggle.waitFor({ state: 'visible' });
    assert.equal(await scriptToggle.textContent(), 'Enable');

    // Click Enable on script page, wait for label to become Disable
    await scriptToggle.click();
    await page.waitForFunction(() => document.querySelector('#toggle')?.textContent === 'Disable');
    assert.equal(await scriptToggle.textContent(), 'Disable');

    // Verify /api/scripts reports enabled:true
    res = await (await page.request.get(`${baseURL}/api/scripts`)).json();
    scriptState = res.find(s => s.name === scriptName);
    assert.equal(scriptState?.enabled, true);

    // Navigate back to / and assert card button reads Disable
    await page.goto(baseURL);
    const restoredCard = page.locator('.card').filter({ has: page.getByRole('heading', { name: scriptName, exact: true }) });
    const restoredToggle = restoredCard.locator('.toggle-btn');
    await restoredToggle.waitFor({ state: 'visible' });
    assert.equal(await restoredToggle.textContent(), 'Disable');
  });
  assert.deepEqual(errors, []);
});

test('failing summary displays the API error on the script page', async t => {
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const page = await browser.newPage();
  // Only the summary API is substituted; the embedded page and Chart.js are real.
  await page.route('**/api/scripts', route => route.fulfill({ json: [{
    name: scriptName, schedule: '@hourly', uptime: 0, avgLatency: 12, p95Latency: 12,
    okRuns: 0, failedRuns: 1,
    latest: { time: new Date().toISOString(), ok: false, error: 'upstream timeout' }
  }] }));
  await page.goto(`${baseURL}/script.html?name=${encodeURIComponent(scriptName)}`);
  await page.waitForFunction(() => document.querySelector('.badge')?.textContent === 'Failing');
  assert.equal(await page.locator('.run-error').textContent(), 'upstream timeout');
  assert.equal(await page.locator('.run-error').isVisible(), true);
});
