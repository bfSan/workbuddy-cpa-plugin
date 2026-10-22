const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const STORE = 'workbuddy-oauth-pending-v1';
const html = fs.readFileSync(path.join(__dirname, 'panel.html'), 'utf8');
const source = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1]
  .replace('__WB_MANAGEMENT_BASE_PATH_JSON__', JSON.stringify('/v0/management'))
  .replace(/\nloadInitial\(\);\s*$/, '');

function element() {
  const classes = new Set(), listeners = new Map();
  let markup = '';
  const e = { hidden: false, value: '', textContent: '', disabled: false, className: '', style: {}, dataset: {}, children: [], files: [],
    classList: { add: x => classes.add(x), remove: x => classes.delete(x), contains: x => classes.has(x) },
    addEventListener(name, fn) { listeners.set(name, fn); },
    click() { return listeners.get('click')?.({ currentTarget: e }); },
    appendChild(child) { e.children.push(child); },
    querySelectorAll() { return []; }, querySelector() { return null; }, focus() {}, select() {}, scrollIntoView() {},
  };
  Object.defineProperty(e, 'innerHTML', { get() { return markup; }, set(value) { markup = value; e.children = []; } });
  return e;
}
function harness({ storage = new Map(), sessionStorage, now = 1900000000000 } = {}) {
  let clock = now, timerID = 0;
  const elements = new Map(), timers = new Map(), toasts = [], opens = [], logs = [];
  const ss = sessionStorage || { getItem: k => storage.get(k) || null, setItem: (k, v) => storage.set(k, String(v)), removeItem: k => storage.delete(k) };
  const document = {
    getElementById(id) { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); },
    createElement: element, querySelectorAll() { return []; }, addEventListener() {},
  };
  class Clock extends Date { static now() { return clock; } }
  const c = { document, sessionStorage: ss, localStorage: { getItem() { return null; } },
    location: { href: 'http://localhost/panel', host: 'localhost' }, history: { replaceState() {} }, navigator: {},
    URL, URLSearchParams, TextEncoder, TextDecoder, Uint8Array, Date: Clock,
    console: { log(...args) { logs.push(args); }, error(...args) { logs.push(args); }, warn(...args) { logs.push(args); } },
    setTimeout(fn, ms) { const id = ++timerID; timers.set(id, { fn, ms }); return id; }, clearTimeout(id) { timers.delete(id); },
    setInterval() { return 1; }, clearInterval() {}, open(...args) { opens.push(args); },
    fetch() { throw new Error('No real upstream calls allowed'); },
  };
  c.window = c; c.self = c; c.top = c;
  vm.createContext(c); vm.runInContext(source, c);
  c.toast = (...args) => toasts.push(args);
  c.load = async () => true;
  const run = code => vm.runInContext(code, c);
  return { c, document, elements, storage, timers, toasts, opens, logs, run,
    now: () => clock, advance(ms) { clock += ms; },
    pending() { return JSON.parse(storage.get(STORE) || '[]'); },
    async tick() { const next = timers.entries().next().value; assert.ok(next, 'poll scheduled'); timers.delete(next[0]); await next[1].fn(); },
  };
}
function startResponse(h, state = 'private-state', extra = {}) {
  return { success: true, state, url_base: 'https://login.example.test/oauth', query: { state, redirect: 'https://client.test/a?a=1&amp;b=2' },
    expiresAt: new Date(h.now() + 300000).toISOString(), expiresIn: 300, persistent: true, background_polling: false, ...extra };
}
async function start(h, state, region = 'cn', extra = {}) {
  h.c.api = async () => startResponse(h, state, extra);
  await h.c.startOAuthLogin(element(), region);
}

test('restored panel bootstrap and every declarative action exist', async () => {
  const h = harness();
  for (const name of ['openOAuthLogin', 'startOAuthLogin', 'pollOAuthLogin', 'copyOAuthURL', 'importAuth', 'importOneCredential', 'openDesensitizeModal', 'waitForDesensitizeSettings', 'saveDesensitize', 'restoreDesensitizeTerms', 'restoreAllDesensitizeDefaults', 'toggleAuto', 'loadInitial']) {
    assert.equal(typeof h.c[name], 'function', name);
  }
  for (const [, name] of html.matchAll(/(?:onclick|onchange|oninput)="([A-Za-z]\w*)\(/g)) assert.equal(typeof h.c[name], 'function', name);
  let loads = 0, egress = 0;
  h.c.getKey = () => 'key'; h.c.load = async () => { loads++; return true; }; h.c.loadEgressIP = () => { egress++; };
  await h.c.loadInitial();
  assert.equal(loads, 1); assert.equal(egress, 1);
  assert.match(html, /if\(window\.__wbThemeSync\)window\.__wbThemeSync\(\)/);
});

test('OAuth start split URL supports host escaping and stores only approved fields', async () => {
  const h = harness();
  await start(h, 'private-state', 'global');
  assert.equal(h.pending().length, 1);
  assert.deepEqual(Object.keys(h.pending()[0]).sort(), ['deadline', 'region', 'state']);
  assert.equal(h.pending()[0].region, 'global');
  assert.equal(h.pending()[0].deadline, h.now() + 300000);
  const url = new URL(h.document.getElementById('oauthUrl').value);
  assert.equal(url.searchParams.get('redirect'), 'https://client.test/a?a=1&b=2');
  assert.equal(h.opens[0][2], 'noopener,noreferrer');
  assert.doesNotMatch(h.storage.get(STORE), /https|redirect|token|url/);
  assert.doesNotMatch(JSON.stringify(h.logs), /private-state/);
  assert.match(h.document.getElementById('oauthStatus').textContent, /后台不会自主轮询/);
});

test('OAuth legacy escaped URL and expiry fallback remain compatible', async () => {
  const h = harness();
  h.c.api = async () => ({ success: true, state: 's', url: 'https://login.test/?a=1&amp;state=s', expiresIn: 75 });
  await h.c.startOAuthLogin(null, 'cn');
  assert.equal(h.document.getElementById('oauthUrl').value, 'https://login.test/?a=1&state=s');
  assert.equal(h.pending()[0].deadline, h.now() + 75000);
  assert.equal(h.c.buildLoginURL({ url: 'javascript:alert(1)' }), '');
  assert.equal(h.c.buildLoginURL({ url: 'https://user:password@login.test/' }), '');
  assert.equal(h.c.oauthResponseDeadline({ expiresAt: (h.now() + 60000) / 1000 }, 1), h.now() + 60000);
});

test('closing pauses polling without deleting pending; same-tab refresh offers recovery', async () => {
  const h = harness(); await start(h, 'saved-session');
  h.c.closeOAuthModal();
  assert.equal(h.timers.size, 0); assert.equal(h.pending().length, 1);
  assert.equal(h.document.getElementById('oauthModal').classList.contains('show'), false);
  const refreshed = harness({ storage: h.storage, now: h.now() });
  refreshed.c.restoreOAuthPending();
  assert.equal(refreshed.document.getElementById('oauthResumeBtn').hidden, false);
  assert.equal(refreshed.run('oauthState'), 'saved-session');
  assert.equal(refreshed.timers.size, 0);
  refreshed.c.resumeOAuthLogin();
  assert.equal(refreshed.document.getElementById('oauthModal').classList.contains('show'), true);
  assert.equal(refreshed.document.getElementById('oauthOpenBtn').disabled, true);
  assert.equal(refreshed.document.getElementById('oauthUrl').value, '');
  let request;
  refreshed.c.api = async (path, opts) => { request = [path, JSON.parse(opts.body)]; return { success: false, status: 'pending', done: false }; };
  await refreshed.tick();
  assert.deepEqual(request, ['/oauth/poll', { state: 'saved-session' }]);
});

test('multiple pending logins survive switching, closing and refreshing with selected restored', async () => {
  const h = harness(); await start(h, 'cn-state'); await start(h, 'global-state', 'global');
  assert.deepEqual(h.pending().map(p => p.state), ['global-state', 'cn-state']);
  h.c.resumeOAuthLogin('cn-state'); h.c.closeOAuthModal();
  assert.deepEqual(h.pending().map(p => p.state), ['cn-state', 'global-state']);
  const refreshed = harness({ storage: h.storage, now: h.now() }); refreshed.c.restoreOAuthPending();
  assert.equal(refreshed.run('oauthState'), 'cn-state');
  assert.equal(refreshed.document.getElementById('oauthPendingList').children.length, 2);
  refreshed.document.getElementById('oauthPendingList').children[1].click();
  assert.equal(refreshed.run('oauthState'), 'global-state');
  assert.equal(refreshed.run('oauthRegion'), 'global');
  assert.equal(refreshed.timers.size, 1);
});

test('late concurrent OAuth start responses never overwrite the selected session', async () => {
  const h = harness(); let first, second;
  h.c.api = (_p, opts) => new Promise(resolve => { if (JSON.parse(opts.body).region === 'cn') first = resolve; else second = resolve; });
  const a = h.c.startOAuthLogin(null, 'cn'), b = h.c.startOAuthLogin(null, 'global');
  second(startResponse(h, 'new-selected')); await b;
  first(startResponse(h, 'late-other')); await a;
  assert.deepEqual(h.pending().map(p => p.state), ['new-selected', 'late-other']);
  assert.equal(h.run('oauthState'), 'new-selected'); assert.equal(h.timers.size, 1);
});

test('closing during start retains late pending without reopening or polling', async () => {
  const h = harness(); let resolve;
  h.c.api = () => new Promise(r => { resolve = r; });
  const pending = h.c.startOAuthLogin(); h.c.closeOAuthModal();
  resolve(startResponse(h, 'late')); await pending;
  assert.equal(h.pending()[0].state, 'late'); assert.equal(h.timers.size, 0);
  assert.equal(h.document.getElementById('oauthModal').classList.contains('show'), false);
});

test('expiry cleans storage on refresh and before polling without API calls', async () => {
  const h = harness(); await start(h, 'expired'); h.c.closeOAuthModal(); h.advance(300001);
  const refreshed = harness({ storage: h.storage, now: h.now() }); refreshed.c.restoreOAuthPending();
  assert.equal(refreshed.storage.has(STORE), false); assert.equal(refreshed.document.getElementById('oauthResumeBtn').hidden, true);
  await start(refreshed, 'will-expire'); refreshed.advance(300001);
  refreshed.c.api = async () => { assert.fail('expired state must not poll'); };
  await refreshed.tick(); assert.equal(refreshed.storage.has(STORE), false); assert.equal(refreshed.timers.size, 0);
  assert.match(refreshed.document.getElementById('oauthStatus').textContent, /已过期/);
});

test('invalid stored payloads and duplicate states are cleaned and extra fields discarded', () => {
  const h = harness();
  h.storage.set(STORE, JSON.stringify([{ state: 'ok', deadline: h.now() + 1000, region: 'cn', token: 'never-store', url: 'https://secret' },
    { state: 'ok', deadline: h.now() + 1000, region: 'cn' }, { state: 'expired', deadline: 1, region: 'cn' },
    { state: 'bad', deadline: 'forever', region: 'cn' }, { state: 'bad-region', deadline: h.now() + 1000, region: '<img>' }]));
  h.c.restoreOAuthPending();
  assert.deepEqual(h.pending(), [{ state: 'ok', deadline: h.now() + 1000, region: 'cn' }]);
  for (const malformed of ['broken-json', '{}', 'null']) {
    h.storage.set(STORE, malformed); assert.doesNotThrow(() => h.c.restoreOAuthPending()); assert.equal(h.storage.has(STORE), false);
  }
});

test('storage exceptions preserve in-memory flow and show fixed warning, never raw data', async () => {
  for (const name of ['SecurityError', 'QuotaExceededError']) {
    const fail = () => { throw new Error(name + ' private-state secret-token https://proxy'); };
    const h = harness({ sessionStorage: { getItem: fail, setItem: fail, removeItem: fail } });
    h.c.restoreOAuthPending(); await start(h, 'private-state');
    assert.equal(h.run('oauthPending.length'), 1);
    assert.match(h.document.getElementById('oauthStatus').textContent, /存储不可用/);
    assert.doesNotMatch(h.document.getElementById('oauthStatus').textContent, /SecurityError|QuotaExceededError|private-state|secret-token|https/);
    h.c.closeOAuthModal(); h.c.resumeOAuthLogin(); assert.equal(h.timers.size, 1);
    h.c.api = async () => ({ success: true, status: 'success', done: true }); await h.tick();
    assert.equal(h.run('oauthPending.length'), 0);
  }
});

test('OAuth expiry is bounded to loginTTL and honors shorter post-token expiry', async () => {
  const h = harness();
  h.storage.set(STORE, JSON.stringify([{ state: 'forged-long-window', deadline: h.now() + 365 * 86400000, region: 'cn' }]));
  h.c.restoreOAuthPending(); assert.equal(h.pending().length, 0);
  assert.equal(h.c.oauthResponseDeadline({ expiresIn: 9999999 }), h.now() + 1800000);
  assert.equal(h.c.oauthResponseDeadline({}), h.now() + 1800000);
  assert.equal(h.c.oauthResponseDeadline({ expiresAt: new Date(h.now() + 365 * 86400000).toISOString() }), h.now() + 1800000);
  await start(h, 'short-result');
  h.c.api = async () => ({ success: false, status: 'error', retryable: true, expiresAt: new Date(h.now() + 600000).toISOString() });
  await h.tick(); assert.equal(h.pending()[0].deadline, h.now() + 600000);
});

test('login entity decoding is exactly one layer, including split query values', () => {
  const h = harness();
  assert.equal(h.c.decodeLoginPart('&amp;amp;'), '&amp;');
  assert.equal(h.c.decodeLoginPart('&#38;amp;'), '&amp;');
  assert.equal(h.c.decodeLoginPart('&quot;a&#39;&lt;b&gt;'), '\"a\'<b>');
  const url = new URL(h.c.buildLoginURL({ url_base: 'https://login.test/', query: { value: '&amp;amp;', state: 's' } }));
  assert.equal(url.searchParams.get('value'), '&amp;');
  const legacy = new URL(h.c.buildLoginURL({ url: 'https://login.test/?value=&amp;amp;' }));
  assert.equal(legacy.search, '?value=&amp;');
});

test('OAuth unknown business status cannot produce success toast', async () => {
  const h = harness(); await start(h, 'unknown');
  h.c.api = async () => ({ success: true, status: 'unrecognized', done: false }); await h.tick();
  assert.equal(h.pending().length, 1); assert.equal(h.toasts.length, 0); assert.equal(h.timers.size, 1);
});

test('OAuth pending acknowledgement success is not login success', async () => {
  const h = harness(); await start(h, 'pending');
  h.c.api = async () => ({ success: true, status: 'pending', done: false, persistent: true }); await h.tick();
  assert.equal(h.pending().length, 1); assert.equal(h.toasts.length, 0); assert.equal(h.timers.size, 1);
  assert.equal(h.document.getElementById('oauthModal').classList.contains('show'), true);
});

test('retryable credential-save errors retain state even with done/success and renew deadline', async () => {
  const h = harness(); await start(h, 'retry-save');
  const deadline = h.now() + 500000;
  h.c.api = async () => ({ success: true, status: 'error', done: true, retryable: true, expiresAt: new Date(deadline).toISOString(), error: 'secret-token', message: '<proxy html>' });
  await h.tick();
  assert.equal(h.pending()[0].deadline, deadline); assert.equal(h.pending()[0].state, 'retry-save');
  assert.equal(h.toasts.length, 0); assert.equal(h.timers.size, 1);
  const status = h.document.getElementById('oauthStatus').textContent;
  assert.match(status, /尚未保存/); assert.doesNotMatch(status, /secret-token|proxy|成功/);
});

test('OAuth success removes only the completed session and closes modal', async () => {
  const h = harness(); await start(h, 'other'); await start(h, 'completed');
  let reloads = 0; h.c.load = async () => { reloads++; return true; };
  h.c.api = async () => ({ success: true, status: 'success', done: true, nickname: '<secret>' }); await h.tick();
  assert.deepEqual(h.pending().map(p => p.state), ['other']); assert.equal(reloads, 1);
  assert.equal(h.timers.size, 0); assert.equal(h.document.getElementById('oauthModal').classList.contains('show'), false);
  assert.equal(h.toasts[0][1], 'ok'); assert.doesNotMatch(JSON.stringify(h.toasts), /secret/);
});

test('terminal OAuth failures remove only failed state without exposing upstream messages', async () => {
  const h = harness(); await start(h, 'other'); await start(h, 'failed');
  h.c.api = async () => ({ success: false, status: 'error', done: true, retryable: false, error: '<upstream secret-token>' }); await h.tick();
  assert.deepEqual(h.pending().map(p => p.state), ['other']); assert.equal(h.timers.size, 0);
  assert.doesNotMatch(h.document.getElementById('oauthStatus').textContent, /secret-token|upstream/);
});

test('transport and permission errors preserve pending and pause when key is unavailable', async () => {
  const h = harness(); await start(h, 'recover');
  h.c.api = async () => { throw new Error('https://proxy/?token=secret-token'); }; await h.tick();
  assert.equal(h.pending().length, 1); assert.equal(h.timers.size, 0);
  assert.doesNotMatch(h.document.getElementById('oauthStatus').textContent, /https|secret-token/);
  h.c.resumeOAuthLogin(); h.c.getKey = () => 'key'; await h.tick(); assert.equal(h.timers.size, 1);
});

test('closed in-flight polls cannot restart the timer or lose pending', async () => {
  const h = harness(); await start(h, 'inflight'); let resolve;
  h.c.api = () => new Promise(r => { resolve = r; }); const tick = h.tick(); h.c.closeOAuthModal();
  resolve({ success: false, status: 'pending' }); await tick;
  assert.equal(h.pending().length, 1); assert.equal(h.timers.size, 0);
});

test('account freeze displays scope/reason/recovery/timing escaped and clears account scope', async () => {
  const h = harness();
  const rendered = h.c.card({ auth_index: '2', auth_id: 'id&"', name: 'a', disabled: true, cooling: [
    { scope: 'account', model: 'must-not-be-used', seconds: 90, reason: '<img src=x onerror=1>', message: 'm"<script>',
      recovery_condition: 'balance > 0 <ok>', occurred_at: '2026-10-22T00:00:00Z', next_probe_at: '2026-10-22T00:02:00Z', until: '2026-10-22T00:03:00Z', persistent: false },
    { scope: 'model', model: 'model<quoted>', reason: 'rate_limit', seconds: 10, recovery_condition: 'probe succeeds' },
  ] });
  assert.match(rendered, /账号共享额度（全部模型）/); assert.match(rendered, /恢复条件：balance &gt; 0 &lt;ok&gt;/);
  assert.match(rendered, /title="作用范围：账号共享额度/); assert.match(rendered, /下次探测/); assert.match(rendered, /冻结至/);
  assert.match(rendered, /重启清空/); assert.match(rendered, /解除冻结不解除手动禁用/); assert.match(rendered, /已禁用/);
  assert.match(rendered, /data-model=""/); assert.match(rendered, /data-model="model&lt;quoted&gt;"/);
  assert.doesNotMatch(rendered, /<img|<script>|must-not-be-used/);
  let request; h.c.api = async (p, o) => { request = [p, o.method]; return {}; };
  await h.c.clearCooldown('id&"', '', null);
  assert.deepEqual(request, ['/cooldowns/clear?auth_id=id%26%22', 'POST']);
});

test('quota machine codes are localized and account_model keeps model-scoped clearing', () => {
  const h = harness();
  const expected = { rate_limit: '请求频率受限', call_quota: '调用次数额度耗尽', credit_quota: '积分额度耗尽', shared_credits_exhausted: '账号共享积分已耗尽', upstream_error: '上游暂时异常' };
  for (const [reason, label] of Object.entries(expected)) {
    const markup = h.c.card({ auth_index: '1', name: 'a', cooling: [{ scope: 'account_model', model: 'm', seconds: 30, reason, recovery_condition: 'bounded_ttl_probe' }] });
    assert.ok(markup.includes(label)); assert.ok(!markup.includes(reason));
    assert.match(markup, /短暂冻结结束后有限探测/); assert.match(markup, /data-model="m"/);
  }
  assert.match(h.c.freezeRecoveryText('upstream_retry_after'), /上游指定重试时间/);
  assert.match(h.c.freezeRecoveryText('upstream_reset'), /上游额度重置/);
  assert.equal(h.c.displayStatusTime('0001-01-01T00:00:00Z'), '—');
  assert.equal(h.c.checkinNextRetry({ retry_allowed: false, next_retry_at: '2026-10-22T06:10:00Z' }), '');
  assert.equal(h.c.checkinNextRetry({ retry_scheduled: false, next_retry_at: '2026-10-22T06:10:00Z' }), '');
});

test('checkin confirmation is strictly authoritative, never inferred from success or words', () => {
  const h = harness();
  for (const value of [{ success: true }, { state: 'success' }, { state: 'already' }, { message: '今日已签到 already' },
    { skipped: true, message: '今日尚未签到' }, { success: true, reason: 'already', confirmed: false }, { confirmed: 'true' }]) {
    assert.equal(h.c.isCheckinDoneResult(value), false, JSON.stringify(value));
  }
  assert.equal(h.c.isCheckinDoneResult({ confirmed: true, state: 'success' }), true);
  assert.equal(h.c.isCheckinDoneResult({ success: true, reason: 'already' }), true);
  const card = h.c.card({ auth_index: '1', name: 'a', checkin: { today_checked_in: true, confirmed: false } });
  assert.doesNotMatch(card, />已签到</); assert.doesNotMatch(card, /data-action="checkin-history"/);
});

test('pending checkin produces warning with next retry, never success toast or done button', async () => {
  const h = harness(), button = element(); button.innerHTML = '签到';
  h.c.api = async () => ({ results: [{ auth_index: '1', success: true, confirmed: false, state: 'pending_confirmation', next_retry_at: '2026-10-22T06:10:00Z', message: '今日已签到' }] });
  h.run('lastAccounts=[{auth_index:"1"}]'); await h.c.checkin('1', button);
  assert.equal(button.disabled, false); assert.equal(button.innerHTML, '签到');
  assert.equal(h.toasts[0][1], 'warn'); assert.match(h.toasts[0][2], /下次重试/);
  assert.equal(h.run('lastAccounts[0].checkin.today_checked_in'), false);
});

test('batch checkin marks only matched confirmed rows, ignores misleading aggregate summary', async () => {
  const h = harness(); const buttons = ['1', '2', '3'].map(idx => { const b = element(); b.dataset.authIndex = idx; return b; });
  h.document.querySelectorAll = selector => { assert.equal(selector, '#grid [data-action="checkin"]'); return buttons; };
  h.run('lastAccounts=[{auth_index:"1"},{auth_index:"2"},{auth_index:"3"}]');
  h.c.api = async () => ({ results: [{ auth_index: '1', confirmed: true, state: 'success' }, { auth_index: '2', confirmed: false, success: true, state: 'pending_confirmation' }, { auth_index: '3', confirmed: false, state: 'failed' }], summary: { success: 3 } });
  await h.c.checkinAll(element());
  assert.equal(buttons[0].innerHTML, '已签到'); assert.notEqual(buttons[1].innerHTML, '已签到'); assert.notEqual(buttons[2].innerHTML, '已签到');
  assert.equal(h.run('lastAccounts.filter(a=>a.checkin.today_checked_in).length'), 1);
  assert.match(h.toasts[0][0], /1 已确认 \/ 1 待确认 \/ 1 失败/); assert.notEqual(h.toasts[0][1], 'ok');
});

test('unrelated single-account response cannot mark requested account done', async () => {
  const h = harness(), button = element();
  h.c.api = async () => ({ results: [{ auth_index: 'other', confirmed: true, state: 'success' }] });
  await h.c.checkin('requested', button); assert.notEqual(button.innerHTML, '已签到');
});

test('panel excludes checkin history but keeps normal checkin controls', () => {
  const h = harness();
  assert.doesNotMatch(html, /签到记录|签到运行记录|checkinHistory|CheckinHistory|checkin-history|checkin-record|checkinRecordHTML|\/checkin\/status/);
  for (const name of ['openCheckinHistory', 'refreshCheckinHistory', 'renderCheckinHistory', 'checkinRecordHTML']) assert.equal(typeof h.c[name], 'undefined');
  const card = h.c.card({ auth_index: 'a', name: 'CN', checkin: { active: true, today_checked_in: false } });
  assert.match(card, /data-action="checkin"/); assert.doesNotMatch(card, /签到记录|checkin-history/);
  assert.match(html, /onclick="checkinAll\(this\)"/); assert.match(html, /id="autoToggle"/);
});

test('single and batch checkin never query or advertise history', async () => {
  const h = harness(), requests = [];
  h.run('lastAccounts=[{auth_index:"1"}]');
  h.c.api = async p => { requests.push(p); assert.equal(p, '/checkin'); return { results: [{ auth_index: '1', confirmed: false, state: 'pending_confirmation' }] }; };
  await h.c.checkin('1', element()); await h.c.checkinAll(element());
  assert.deepEqual(requests, ['/checkin', '/checkin']);
  assert.doesNotMatch(JSON.stringify(h.toasts), /签到记录|签到运行记录|运行记录/);
  assert.equal(h.run('lastAccounts[0].checkin.today_checked_in'), false);
});

test('desensitize save waits for matching effective runtime settings', async () => {
  const h = harness(); const order = [];
  h.document.getElementById('desensitizeEnabled').checked = true; h.document.getElementById('desensitizeTerms').value = 'One\nOne\nTwo';
  h.c.managementAPI = async (p, o) => { order.push(['patch', p, JSON.parse(o.body)]); return {}; };
  h.c.api = async p => { order.push(['read', p]); return { enabled: true, source: 'custom', terms: ['One', 'Two'] }; };
  await h.c.saveDesensitize(element());
  assert.deepEqual(order, [['patch', '/plugins/workbuddy/config', { desensitize: true, desensitize_terms: ['One', 'Two'] }], ['read', '/desensitize']]);
  assert.equal(h.toasts.at(-1)[1], 'ok');
});

test('restored file import is bounded, sequential and never surfaces credential errors', async () => {
  const h = harness(); let inFlight = 0, maxInFlight = 0; const imported = [];
  h.document.getElementById('importFiles').files = [
    { name: 'huge.json', size: 2 * 1024 * 1024 + 1, text() { assert.fail('oversize file read'); } },
    { name: 'one.json', size: 3, async text() { return 'one-secret'; } },
    { name: 'two.json', size: 3, async text() { return 'two-secret'; } },
  ];
  h.c.importOneCredential = async raw => { inFlight++; maxInFlight = Math.max(maxInFlight, inFlight); imported.push(raw); await Promise.resolve(); inFlight--; throw new Error(raw); };
  h.c.openImportModal(); await h.c.importAuth(element());
  assert.equal(maxInFlight, 1); assert.deepEqual(imported, ['one-secret', 'two-secret']);
  assert.doesNotMatch(JSON.stringify(h.toasts), /one-secret|two-secret/); assert.match(JSON.stringify(h.toasts), /超过 2 MiB/);
  assert.equal(h.document.getElementById('importModal').classList.contains('show'), true);
});
