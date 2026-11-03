const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const { chromium } = require('playwright');

const staticDir = path.resolve(__dirname, '../static');
const fakeOrigin = 'http://notinv.test';

function getContentType(filePath) {
  if (filePath.endsWith('.html')) return 'text/html; charset=utf-8';
  if (filePath.endsWith('.js')) return 'application/javascript; charset=utf-8';
  if (filePath.endsWith('.css')) return 'text/css; charset=utf-8';
  if (filePath.endsWith('.svg')) return 'image/svg+xml';
  if (filePath.endsWith('.png')) return 'image/png';
  if (filePath.endsWith('.txt')) return 'text/plain; charset=utf-8';
  return 'application/octet-stream';
}

function makeScript(name, enabled = true) {
  return {
    name,
    schedule: '*/5 * * * *',
    okRuns: 1,
    failedRuns: 0,
    uptime: 1.0,
    avgLatency: 45.0,
    p95Latency: 80.0,
    enabled,
    latest: {
      ok: true,
      time: new Date().toISOString()
    }
  };
}

async function routePage(page, {
  getScripts = () => [],
  onPostEnabled = null,
  getSeries = () => []
} = {}) {
  await page.route(`${fakeOrigin}/**`, async route => {
    const req = route.request();
    const url = new URL(req.url());
    const pathname = url.pathname;

    // Static pages and assets
    if (pathname === '/' || pathname === '/index.html') {
      const body = fs.readFileSync(path.join(staticDir, 'index.html'));
      return route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body });
    }
    if (pathname === '/script.html') {
      const body = fs.readFileSync(path.join(staticDir, 'script.html'));
      return route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body });
    }
    if (pathname.startsWith('/vendor/') || pathname.startsWith('/favicon') || pathname.startsWith('/apple-touch-icon')) {
      const relPath = pathname.replace(/^\//, '');
      const diskPath = path.join(staticDir, relPath);
      if (fs.existsSync(diskPath)) {
        const body = fs.readFileSync(diskPath);
        return route.fulfill({ status: 200, contentType: getContentType(diskPath), body });
      }
      return route.fulfill({ status: 404, body: 'Not found' });
    }

    // API routes
    if (pathname === '/api/scripts') {
      const scripts = typeof getScripts === 'function' ? getScripts() : getScripts;
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(scripts)
      });
    }

    const enabledMatch = pathname.match(/^\/api\/scripts\/([^/]+)\/enabled$/);
    if (enabledMatch && req.method() === 'POST') {
      const scriptName = decodeURIComponent(enabledMatch[1]);
      if (onPostEnabled) {
        return onPostEnabled(scriptName, req, route);
      }
      const data = req.postDataJSON();
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ name: scriptName, enabled: data.enabled })
      });
    }

    if (pathname.includes('/requests/series')) {
      const series = typeof getSeries === 'function' ? getSeries() : getSeries;
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(series)
      });
    }

    return route.fulfill({ status: 404, body: 'Not found' });
  });
}

test('enable/disable toggle component tests on landing and script pages', async t => {
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());

  await t.test('CLM-1 (landing): two scripts A(enabled:true), B(enabled:false); toggle A flips label, sends single POST, leaves B unchanged, and clicking again flips back', async () => {
    const page = await browser.newPage();
    const scripts = [makeScript('A', true), makeScript('B', false)];
    const posts = [];

    await routePage(page, {
      getScripts: () => scripts,
      onPostEnabled: async (scriptName, req, route) => {
        const body = req.postDataJSON();
        posts.push({
          name: scriptName,
          body,
          contentType: req.headers()['content-type']
        });
        const target = scripts.find(s => s.name === scriptName);
        if (target) target.enabled = body.enabled;
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ name: scriptName, enabled: body.enabled })
        });
      }
    });

    await page.goto(`${fakeOrigin}/`);
    await page.waitForFunction(() => document.querySelectorAll('.card').length === 2);

    const cardA = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'A', exact: true }) });
    const cardB = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'B', exact: true }) });

    assert.equal(await cardA.locator('.toggle-btn').textContent(), 'Disable');
    assert.equal(await cardB.locator('.toggle-btn').textContent(), 'Enable');

    // Click A: should send POST { enabled: false }
    await cardA.locator('.toggle-btn').click();
    await page.waitForFunction(() => {
      const cards = [...document.querySelectorAll('.card')];
      const elA = cards.find(c => c.querySelector('h2 a')?.textContent === 'A');
      return elA?.querySelector('.toggle-btn')?.textContent === 'Enable';
    });

    assert.equal(posts.length, 1);
    assert.equal(posts[0].name, 'A');
    assert.deepEqual(posts[0].body, { enabled: false });
    assert.ok(posts[0].contentType.includes('application/json'));
    assert.equal(await cardA.locator('.toggle-btn').textContent(), 'Enable');
    assert.equal(await cardB.locator('.toggle-btn').textContent(), 'Enable');

    // Click A again: should send POST { enabled: true }
    await cardA.locator('.toggle-btn').click();
    await page.waitForFunction(() => {
      const cards = [...document.querySelectorAll('.card')];
      const elA = cards.find(c => c.querySelector('h2 a')?.textContent === 'A');
      return elA?.querySelector('.toggle-btn')?.textContent === 'Disable';
    });

    assert.equal(posts.length, 2);
    assert.equal(posts[1].name, 'A');
    assert.deepEqual(posts[1].body, { enabled: true });
    assert.equal(await cardA.locator('.toggle-btn').textContent(), 'Disable');
    assert.equal(await cardB.locator('.toggle-btn').textContent(), 'Enable');

    await page.close();
  });

  await t.test('CLM-2 (script page): script.html?name=X has exactly one #toggle, label matches stub enabled, click POSTs inverse for X only, label flips on 200', async () => {
    const page = await browser.newPage();
    const scripts = [makeScript('X', true), makeScript('Y', false)];
    const posts = [];

    await routePage(page, {
      getScripts: () => scripts,
      onPostEnabled: async (scriptName, req, route) => {
        const body = req.postDataJSON();
        posts.push({
          name: scriptName,
          body,
          contentType: req.headers()['content-type']
        });
        const target = scripts.find(s => s.name === scriptName);
        if (target) target.enabled = body.enabled;
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ name: scriptName, enabled: body.enabled })
        });
      }
    });

    await page.goto(`${fakeOrigin}/script.html?name=X`);
    await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');

    assert.equal(await page.locator('#toggle').count(), 1);
    assert.equal(await page.locator('#toggle').textContent(), 'Disable');

    await page.locator('#toggle').click();
    await page.waitForFunction(() => document.querySelector('#toggle')?.textContent === 'Enable');

    assert.equal(posts.length, 1);
    assert.equal(posts[0].name, 'X');
    assert.deepEqual(posts[0].body, { enabled: false });
    assert.equal(await page.locator('#toggle').textContent(), 'Enable');

    await page.close();
  });

  await t.test('CLM-3 (both pages): save failure via 500 response or network abort preserves label, leaves button enabled, and shows error in #status', async () => {
    // 3a: Landing page with 500
    {
      const page = await browser.newPage();
      const scripts = [makeScript('A', true)];
      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (_name, _req, route) => {
          return route.fulfill({ status: 500, body: 'Server Error' });
        }
      });
      await page.goto(`${fakeOrigin}/`);
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn'));
      const btn = page.locator('.card .toggle-btn').first();
      assert.equal(await btn.textContent(), 'Disable');

      await btn.click();
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Failed to update script state'));

      assert.equal(await btn.textContent(), 'Disable');
      assert.equal(await btn.isDisabled(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Failed to update script state'));
      await page.close();
    }

    // 3b: Landing page with network abort
    {
      const page = await browser.newPage();
      const scripts = [makeScript('A', true)];
      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (_name, _req, route) => {
          return route.abort();
        }
      });
      await page.goto(`${fakeOrigin}/`);
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn'));
      const btn = page.locator('.card .toggle-btn').first();
      assert.equal(await btn.textContent(), 'Disable');

      await btn.click();
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Failed to update script state'));

      assert.equal(await btn.textContent(), 'Disable');
      assert.equal(await btn.isDisabled(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Failed to update script state'));
      await page.close();
    }

    // 3c: Script page with 500
    {
      const page = await browser.newPage();
      const scripts = [makeScript('X', true)];
      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (_name, _req, route) => {
          return route.fulfill({ status: 500, body: 'Server Error' });
        }
      });
      await page.goto(`${fakeOrigin}/script.html?name=X`);
      await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');
      const btn = page.locator('#toggle');
      assert.equal(await btn.textContent(), 'Disable');

      await btn.click();
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Failed to update script state'));

      assert.equal(await btn.textContent(), 'Disable');
      assert.equal(await btn.isDisabled(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Failed to update script state'));
      await page.close();
    }

    // 3d: Script page with network abort
    {
      const page = await browser.newPage();
      const scripts = [makeScript('X', true)];
      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (_name, _req, route) => {
          return route.abort();
        }
      });
      await page.goto(`${fakeOrigin}/script.html?name=X`);
      await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');
      const btn = page.locator('#toggle');
      assert.equal(await btn.textContent(), 'Disable');

      await btn.click();
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Failed to update script state'));

      assert.equal(await btn.textContent(), 'Disable');
      assert.equal(await btn.isDisabled(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Failed to update script state'));
      await page.close();
    }
  });

  await t.test('CLM-4 (both pages): hold POST pending, click twice; exactly one POST observed, button disabled while pending, enabled after release', async () => {
    // 4a: Landing page
    {
      const page = await browser.newPage();
      const scripts = [makeScript('A', true)];
      let postCount = 0;
      let releasePost;
      const releasePromise = new Promise(resolve => { releasePost = resolve; });

      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (scriptName, req, route) => {
          postCount++;
          await releasePromise;
          const target = scripts.find(s => s.name === scriptName);
          if (target) target.enabled = false;
          return route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ name: scriptName, enabled: false })
          });
        }
      });

      await page.goto(`${fakeOrigin}/`);
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn'));
      const btn = page.locator('.card .toggle-btn').first();

      // First click
      await btn.click();
      // Button must be disabled while pending
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn').disabled === true);
      assert.equal(await btn.isDisabled(), true);
      assert.equal(postCount, 1);

      // Attempt second click while pending
      await btn.dispatchEvent('click');
      await page.waitForTimeout(50);
      assert.equal(postCount, 1);

      // Release POST
      releasePost();
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn').disabled === false);
      assert.equal(await btn.isDisabled(), false);
      assert.equal(postCount, 1);
      assert.equal(await btn.textContent(), 'Enable');
      await page.close();
    }

    // 4b: Script page
    {
      const page = await browser.newPage();
      const scripts = [makeScript('X', true)];
      let postCount = 0;
      let releasePost;
      const releasePromise = new Promise(resolve => { releasePost = resolve; });

      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (scriptName, req, route) => {
          postCount++;
          await releasePromise;
          const target = scripts.find(s => s.name === scriptName);
          if (target) target.enabled = false;
          return route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ name: scriptName, enabled: false })
          });
        }
      });

      await page.goto(`${fakeOrigin}/script.html?name=X`);
      await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');
      const btn = page.locator('#toggle');

      // First click
      await btn.click();
      // Button must be disabled while pending
      await page.waitForFunction(() => document.querySelector('#toggle').disabled === true);
      assert.equal(await btn.isDisabled(), true);
      assert.equal(postCount, 1);

      // Attempt second click while pending
      await btn.dispatchEvent('click');
      await page.waitForTimeout(50);
      assert.equal(postCount, 1);

      // Release POST
      releasePost();
      await page.waitForFunction(() => document.querySelector('#toggle').disabled === false);
      assert.equal(await btn.isDisabled(), false);
      assert.equal(postCount, 1);
      assert.equal(await btn.textContent(), 'Enable');
      await page.close();
    }
  });

  await t.test('CLM-5: script.html with no ?name= and with ?name=missing hides #toggle and displays Script not found', async () => {
    // 5a: No name
    {
      const page = await browser.newPage();
      await routePage(page, {
        getScripts: () => [makeScript('A', true)]
      });
      await page.goto(`${fakeOrigin}/script.html`);
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Script not found'));

      assert.equal(await page.locator('#toggle').isVisible(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Script not found'));
      await page.close();
    }

    // 5b: ?name=missing (not in stub list)
    {
      const page = await browser.newPage();
      await routePage(page, {
        getScripts: () => [makeScript('A', true)]
      });
      await page.goto(`${fakeOrigin}/script.html?name=missing`);
      await page.waitForFunction(() => document.getElementById('status').textContent.includes('Script not found'));

      assert.equal(await page.locator('#toggle').isVisible(), false);
      const statusText = await page.locator('#status').textContent();
      assert.ok(statusText.includes('Script not found'));
      await page.close();
    }
  });

  await t.test('CLM-6 (both pages, with page.clock installed before goto): first GET enabled:true, later GET enabled:false; initial label Disable, after fastForward(30000) reads Enable', async () => {
    // 6a: Landing page
    {
      const page = await browser.newPage();
      let getCount = 0;
      await routePage(page, {
        getScripts: () => {
          getCount++;
          return [makeScript('A', getCount === 1)];
        }
      });

      await page.clock.install();
      await page.goto(`${fakeOrigin}/`);
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn'));
      const btn = page.locator('.card .toggle-btn').first();
      assert.equal(await btn.textContent(), 'Disable');

      await page.clock.fastForward(30000);
      await page.waitForFunction(() => document.querySelector('.card .toggle-btn')?.textContent === 'Enable');
      assert.equal(await btn.textContent(), 'Enable');
      await page.close();
    }

    // 6b: Script page
    {
      const page = await browser.newPage();
      let getCount = 0;
      await routePage(page, {
        getScripts: () => {
          getCount++;
          return [makeScript('X', getCount === 1)];
        }
      });

      await page.clock.install();
      await page.goto(`${fakeOrigin}/script.html?name=X`);
      await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');
      const btn = page.locator('#toggle');
      assert.equal(await btn.textContent(), 'Disable');

      await page.clock.fastForward(30000);
      await page.waitForFunction(() => document.querySelector('#toggle')?.textContent === 'Enable');
      assert.equal(await btn.textContent(), 'Enable');
      await page.close();
    }
  });

  await t.test('REQ-1 no credential (both pages): toggle POST carries no authorization or cookie header, targets only that script, once per click', async () => {
    for (const target of [
      { label: 'landing', other: 'B', url: `${fakeOrigin}/`, name: 'A', card: true, ready: () => document.querySelectorAll('.card').length === 2, btn: '.toggle-btn', flipped: () => [...document.querySelectorAll('.card')].find(c => c.querySelector('h2 a')?.textContent === 'A')?.querySelector('.toggle-btn')?.textContent === 'Enable' },
      { label: 'script page', other: 'Y', url: `${fakeOrigin}/script.html?name=X`, name: 'X', ready: () => document.querySelector('#toggle')?.style.display !== 'none', btn: '#toggle', flipped: () => document.querySelector('#toggle')?.textContent === 'Enable' }
    ]) {
      const page = await browser.newPage();
      const scripts = [makeScript(target.name, true), makeScript(target.other, true)];
      const posts = [];

      await routePage(page, {
        getScripts: () => scripts,
        onPostEnabled: async (scriptName, req, route) => {
          const headers = req.headers();
          posts.push({ name: scriptName, pathname: new URL(req.url()).pathname, headers });
          const body = req.postDataJSON();
          return route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ name: scriptName, enabled: body.enabled })
          });
        }
      });

      await page.goto(target.url);
      await page.waitForFunction(target.ready);
      const btn = target.card ? page.locator('.card').filter({ has: page.getByRole('heading', { name: target.name, exact: true }) }).locator('.toggle-btn') : page.locator(target.btn);
      assert.equal(await btn.textContent(), 'Disable', target.label);

      await btn.click();
      await page.waitForFunction(target.flipped);

      assert.equal(posts.length, 1, `${target.label}: exactly one POST per click`);
      assert.equal(posts[0].name, target.name, target.label);
      assert.equal(posts[0].pathname, `/api/scripts/${target.name}/enabled`, target.label);
      assert.equal('authorization' in posts[0].headers, false, `${target.label}: no authorization header`);
      assert.equal('cookie' in posts[0].headers, false, `${target.label}: no cookie header`);
      assert.equal(posts.some(p => p.name === target.other), false, `${target.label}: no POST for ${target.other}`);
      await page.close();
    }
  });

  await t.test('REQ-1 disabling changes only the button label (both pages)', async () => {
    // Landing page: card A (and B) compared before/after; relative times are not compared.
    {
      const page = await browser.newPage();
      const scripts = [makeScript('A', true), makeScript('B', true)];
      await routePage(page, { getScripts: () => scripts });
      await page.goto(`${fakeOrigin}/`);
      await page.waitForFunction(() => document.querySelectorAll('.card').length === 2);

      const cardA = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'A', exact: true }) });
      const cardB = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'B', exact: true }) });
      const snapshot = card => card.evaluate(el => {
        const clone = el.cloneNode(true);
        clone.querySelector('.toggle-btn').remove();
        return { className: el.className, text: clone.textContent.replace(/\s+/g, ' ').trim() };
      });
      const hasDisabledText = () => page.evaluate(() => /\bDisabled\b/.test(document.body.textContent));

      const beforeA = await snapshot(cardA);
      const beforeBHtml = await cardB.evaluate(el => el.outerHTML);
      assert.equal(await hasDisabledText(), false);

      await cardA.locator('.toggle-btn').click();
      await page.waitForFunction(() => {
        const elA = [...document.querySelectorAll('.card')].find(c => c.querySelector('h2 a')?.textContent === 'A');
        return elA?.querySelector('.toggle-btn')?.textContent === 'Enable';
      });

      assert.deepEqual(await snapshot(cardA), beforeA);
      assert.equal(await hasDisabledText(), false);
      assert.equal(await cardB.evaluate(el => el.outerHTML), beforeBHtml);
      await page.close();
    }

    // Script page: everything except #toggle and #status is unchanged.
    {
      const page = await browser.newPage();
      const scripts = [makeScript('X', true)];
      await routePage(page, { getScripts: () => scripts });
      await page.goto(`${fakeOrigin}/script.html?name=X`);
      await page.waitForFunction(() => document.querySelector('#toggle')?.style.display !== 'none');

      const snapshot = () => page.evaluate(() => {
        const clone = document.body.cloneNode(true);
        clone.querySelectorAll('#toggle, #status, script').forEach(n => n.remove());
        return {
          bodyClass: document.body.className,
          text: clone.textContent.replace(/\s+/g, ' ').trim(),
          disabledBadge: /\bDisabled\b/.test(document.body.textContent)
        };
      });

      const before = await snapshot();
      assert.equal(before.disabledBadge, false);

      await page.locator('#toggle').click();
      await page.waitForFunction(() => document.querySelector('#toggle')?.textContent === 'Enable');

      assert.deepEqual(await snapshot(), before);
      await page.close();
    }
  });
});
