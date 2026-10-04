// Verifies the credit-consistency warning renders, and stays quiet on good data.
//
// The panel builds its cards from /accounts, whose entries already embed a
// `credits` object, so that is the response to poison. The numbers below are the
// exact ones the workbuddy TotalDosage bug produced, so this exercises the real
// failure shape without touching any live account.
//
// It reads the management key from /opt/cpa/.credentials and needs the live CPA
// at $CPA_URL (default 127.0.0.1:8317), so it is a deployment check rather than a
// unit test. Nothing it does writes to the accounts it reads: the poisoned half
// only rewrites responses inside the browser.
const { execFileSync } = require('child_process');

const path = require('path');

function loadPlaywright() {
  const roots = [
    process.env.PW_DIR,
    process.env.NODE_PATH,
    path.join(__dirname, '..', 'node_modules'),
    process.cwd(),
  ].filter(Boolean);
  for (const root of roots) {
    try { return require(require.resolve('playwright', { paths: [root] })); } catch (e) { /* next */ }
  }
  try { return require('playwright'); } catch (e) {
    console.error('SKIP: playwright not found (set PW_DIR).');
    process.exit(77);
  }
}
const pw = loadPlaywright();

const BASE = process.env.CPA_URL || 'http://127.0.0.1:8317';
// Each panel keeps its management key under its own sessionStorage key.
const SS_KEYS = { workbuddy: 'workbuddy-mgmt-key', qoder: 'qoderwork-mgmt-key' };
const mgmtKey = execFileSync('sudo', ['-n', 'grep', '-oP', '(?<=^CPA_MANAGEMENT_KEY=).*', '/opt/cpa/.credentials'])
  .toString().trim();

const fails = [];
const check = (name, ok, extra) => {
  console.log((ok ? '  ok   ' : '  FAIL ') + name + (extra ? '  ' + extra : ''));
  if (!ok) fails.push(name);
};

async function poison(page) {
  await page.route('**/v0/management/plugins/workbuddy/credits*', async route => {
    const res = await route.fetch();
    const body = await res.text();
    let out = body;
    try {
      const j = JSON.parse(body);
      let hit = false;
      const p = o => { if (o && o.credits) { o.credits.total_size = 109699; o.credits.total_used = 99500; hit = true; } };
      (j.accounts || []).forEach(p);
      if (j.credits) p(j);
      if (j.summary) { j.summary.total_size = 109699; hit = true; }
      if (hit) out = JSON.stringify(j);
    } catch (e) { /* not json */ }
    await route.fulfill({ response: res, body: out });
  });
}

// The panel reads its management key from sessionStorage, which is per-origin, so
// seed it on any same-origin page and then navigate to the panel. workbuddy also
// accepts ?key=, but qoder has no such capture path, so this route works for both
// and avoids the load-then-reload race.
async function openPanel(page, plugin) {
  await page.goto(`${BASE}/v0/management/plugins/${plugin}/status`);
  await page.evaluate(([k, key]) => sessionStorage.setItem(k, key), [SS_KEYS[plugin], mgmtKey]);
  await page.goto(`${BASE}/v0/resource/plugins/${plugin}/panel`);
}

(async () => {
  const browser = await pw.chromium.launch();

  for (const id of ['workbuddy', 'qoder']) {
    const page = await browser.newPage();
    const errs = [];
    page.on('pageerror', e => errs.push(String(e)));
    await openPanel(page, id);
    await page.waitForSelector('.pb', { timeout: 30000 });
    await page.waitForTimeout(3000);
    const warns = await page.$$eval('.pb-warn', els => els.map(e => e.textContent.trim()));
    check(`${id}: no false warning on live data`, warns.length === 0, JSON.stringify(warns));
    check(`${id}: no page errors`, errs.length === 0, errs.join(' | '));
    await page.close();
  }

  const page = await browser.newPage();
  await poison(page);
  await openPanel(page, 'workbuddy');
  await page.waitForSelector('.pb', { timeout: 30000 });
  await page.waitForTimeout(3000);
  const warns = await page.$$eval('.pb-warn', els => els.map(e => e.textContent.trim()));
  check('workbuddy: warning appears on inconsistent figures', warns.length > 0,
    warns.length ? warns[0].slice(0, 70) : 'no warning rendered');
  check('workbuddy: warning names the pool figure', warns.some(t => t.includes('109699')),
    JSON.stringify(warns.map(t => t.slice(0, 50))));
  await page.close();

  await browser.close();
  console.log(fails.length ? `\n${fails.length} FAILED: ${fails.join(', ')}` : '\ncredit warning check passed');
  process.exit(fails.length ? 1 : 0);
})().catch(e => { console.error('harness error:', e); process.exit(2); });
