const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement() {
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    files: [],
    style: {},
    dataset: {},
    children: [],
    parentNode: null,
    classList: {
      values: new Set(),
      add(value) { this.values.add(value); },
      remove(value) { this.values.delete(value); },
      contains(value) { return this.values.has(value); },
    },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    querySelector(selector) {
      if (!queries.has(selector)) queries.set(selector, fakeElement());
      return queries.get(selector);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    focus() {},
    remove() {
      if (!this.parentNode) return;
      const index = this.parentNode.children.indexOf(this);
      if (index >= 0) this.parentNode.children.splice(index, 1);
      this.parentNode = null;
    },
  };
  Object.defineProperty(element, "firstChild", {
    get() { return element.children[0] || null; },
  });
  return element;
}

function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  const source = scripts.at(-1)[1]
    .replace("__WB_MANAGEMENT_BASE_PATH_JSON__", JSON.stringify("/v0/management"))
    .replace(/\nloadInitial\(\);\s*$/, "");
  const elements = new Map();
  const storage = new Map(overrides.sessionEntries || []);
  const document = {
    documentElement: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const sessionStorage = overrides.sessionStorage || {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = {
    console,
    document,
    sessionStorage,
    localStorage: overrides.localStorage || { getItem() { return null; }, setItem() { throw new Error("localStorage write"); } },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "", host: "localhost" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(value) { return Buffer.from(value, "binary").toString("base64"); },
    atob(value) { return Buffer.from(value, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout(fn) { fn(); return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage };
}

function fakeResponse(status, contentType, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get(name) { return name.toLowerCase() === "content-type" ? contentType : null; } },
    async text() { return body; },
  };
}

test("query key replaces session key once and is removed from URL", () => {
  let replaced = "";
  let localWrites = 0;
  const location = {
    href: "http://localhost/panel?key=secret-value&view=all#accounts",
    search: "?key=secret-value&view=all",
    pathname: "/panel",
    hash: "#accounts",
    host: "localhost",
  };
  const { storage } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "old-value"]],
    location,
    history: { replaceState(state, title, url) { replaced = url; } },
    localStorage: {
      getItem() { return null; },
      setItem() { localWrites += 1; },
    },
  });
  assert.equal(storage.get("workbuddy-mgmt-key"), "secret-value");
  assert.equal(replaced, "/panel?view=all#accounts");
  assert.equal(localWrites, 0);
});

test("Web Storage exceptions clean the query key and use the fixed prompt fallback", async t => {
  for (const errorName of ["SecurityError", "QuotaExceededError"]) {
    await t.test(errorName, async () => {
      const nativeError = new Error(errorName + " native storage detail");
      nativeError.name = errorName;
      const sessionStorage = {
        getItem() { throw nativeError; },
        setItem() { throw nativeError; },
        removeItem() { throw nativeError; },
      };
      let replaced = "";
      let fetchCalls = 0;
      const { context, elements } = loadPanel({
        sessionStorage,
        location: {
          href: "http://localhost/custom/panel?view=all&key=secret-value#accounts",
          search: "?view=all&key=secret-value",
          pathname: "/custom/panel",
          hash: "#accounts",
          host: "localhost",
        },
        history: { replaceState(state, title, url) { replaced = url; } },
        fetch: async () => { fetchCalls += 1; throw new Error("unexpected fetch"); },
      });

      assert.equal(replaced, "/custom/panel?view=all#accounts");
      assert.equal(context.getKey(), null);
      assert.equal(await context.load(false), false);
      assert.equal(elements.get("authBox").style.display, "block");
      assert.match(elements.get("grid").innerHTML, /请先填写 management key/);
      assert.doesNotMatch(elements.get("grid").innerHTML, /native storage detail|SecurityError|QuotaExceededError/);
      await assert.rejects(
        context.api("/accounts"),
        error => error.message === "需要管理密钥" && !error.message.includes("native storage detail"),
      );

      context.document.getElementById("keyInput").value = "manual-key";
      await context.saveKey();
      assert.equal(elements.get("authBox").style.display, "block");
      assert.throws(
        () => context.rejectPanelAuth(fakeResponse(401, "application/json", "")),
        error => error.message === "management key 无效或缺失" && !error.message.includes("native storage detail"),
      );
      assert.equal(fetchCalls, 0);
    });
  }
});

test("query key storage write failure never reuses an older session key", async () => {
  const nativeError = new Error("QuotaExceededError native storage detail");
  nativeError.name = "QuotaExceededError";
  let replaced = "";
  let fetchCalls = 0;
  const sessionStorage = {
    getItem() { return "old-value"; },
    setItem() { throw nativeError; },
    removeItem() { throw nativeError; },
  };
  const { context, elements } = loadPanel({
    sessionStorage,
    location: {
      href: "http://localhost/custom/panel?key=new-value&view=all#accounts",
      search: "?key=new-value&view=all",
      pathname: "/custom/panel",
      hash: "#accounts",
      host: "localhost",
    },
    history: { replaceState(state, title, url) { replaced = url; } },
    fetch: async () => { fetchCalls += 1; throw new Error("unexpected fetch"); },
  });

  assert.equal(replaced, "/custom/panel?view=all#accounts");
  assert.equal(context.getKey(), null);
  context.readPanelKey = () => "old-panel-value";
  assert.equal(context.getKey(), null);
  assert.equal(await context.load(false), false);
  assert.equal(elements.get("authBox").style.display, "block");
  assert.match(elements.get("grid").innerHTML, /请先填写 management key/);
  assert.doesNotMatch(elements.get("grid").innerHTML, /old-value|new-value|native storage detail/);
  assert.equal(fetchCalls, 0);
});

test("model status banner hides ready and persists non-ready states", () => {
  const { context, elements } = loadPanel();
  const statuses = [
    { state: "stale", message: "模型目录刷新失败，正在使用上次有效缓存" },
    { state: "failed", message: "模型目录不可用" },
    { state: "loading", message: "模型目录正在初始化" },
    { state: "not_started", message: "模型目录尚未初始化" },
  ];
  for (const status of statuses) {
    context.updateModelStatus(status);
    const banner = elements.get("modelStatus");
    assert.equal(banner.hidden, false);
    assert.equal(banner.className, "model-status " + status.state);
    assert.equal(banner.textContent, status.message);
  }
  context.updateModelStatus({ state: "ready", message: "ignored" });
  const banner = elements.get("modelStatus");
  assert.equal(banner.hidden, true);
  assert.equal(banner.className, "model-status");
  assert.equal(banner.textContent, "");
});

test("load updates model status before dashboard error", async () => {
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  let toastCalls = 0;
  context.toast = () => { toastCalls += 1; };
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: "dashboard unavailable",
  });
  await context.load(false);
  assert.equal(elements.get("modelStatus").textContent, "模型目录不可用");
  assert.equal(toastCalls, 0);
});

test("model status message is rendered as text", () => {
  const { context, elements } = loadPanel();
  const message = "<img src=x onerror=1>";
  context.updateModelStatus({ state: "failed", message });
  const banner = elements.get("modelStatus");
  assert.equal(banner.textContent, message);
  assert.equal(banner.innerHTML, "");
});

test("panel response parser never exposes response bodies", async () => {
  const { context } = loadPanel();
  const cases = [
    [fakeResponse(200, "application/json", ""), "响应为空"],
    [fakeResponse(200, "text/html; charset=utf-8", "<html>secret-token</html>"), "响应格式无效"],
    [fakeResponse(200, "application/json", "{secret-token"), "响应 JSON 无效"],
    [fakeResponse(503, "application/json", `{"error":"secret-token"}`), "请求失败"],
  ];
  for (const [response, category] of cases) {
    await assert.rejects(
      context.readPanelResponse(response),
      error => error.message.includes(category) &&
        error.message.includes("HTTP ") &&
        !error.message.includes("secret-token"),
    );
  }
  const sanitized = await context.readPanelResponse(
    fakeResponse(200, "application/json", `{"error":"secret-token","nested":{"error":"credential-value"}}`),
  );
  assert.equal(sanitized.error, "请求失败");
  assert.equal(sanitized.nested.error, "请求失败");
  assert.doesNotMatch(JSON.stringify(sanitized), /secret-token|credential-value/);
});

test("panel transport errors use a fixed message", async () => {
  const nativeMessage = "https://host.invalid/?key=secret-token";
  const { context, elements, storage } = loadPanel({
    fetch: async () => { throw new Error(nativeMessage); },
  });
  storage.set("workbuddy-mgmt-key", "test-key");
  await assert.rejects(context.api("/accounts"), error => error.message === "网络请求失败");
  await assert.rejects(context.managementAPI("/plugins/workbuddy/config"), error => error.message === "网络请求失败");

  const toasts = [];
  context.toast = (...args) => { toasts.push(args.join(" ")); };
  await context.load(false);
  await context.load(true, fakeElement());
  const visible = elements.get("grid").innerHTML + toasts.join(" ");
  assert.match(visible, /网络请求失败/);
  assert.doesNotMatch(visible, /secret-token|host\.invalid/);
});

test("panel response auth failures clear the key without reading bodies", async () => {
  for (const [status, message] of [[401, "management key 无效或缺失"], [403, "禁止访问 (403)"]]) {
    let reads = 0;
    const response = fakeResponse(status, "application/json", `{"error":"secret-token"}`);
    response.text = async () => { reads += 1; return `{"error":"secret-token"}`; };
    const { context, storage } = loadPanel({ fetch: async () => response });

    storage.set("workbuddy-mgmt-key", "test-key");
    await assert.rejects(context.api("/accounts"), error => error.message === message);
    assert.equal(storage.has("workbuddy-mgmt-key"), false);

    assert.equal(context.storeSessionKey("test-key"), true);
    await assert.rejects(context.managementAPI("/plugins/workbuddy/config"), error => error.message === message);
    assert.equal(storage.has("workbuddy-mgmt-key"), false);
    assert.equal(reads, 0);
  }
});

test("panel response auth failures use a fixed local cooldown", async () => {
  let calls = 0;
  const { context, storage } = loadPanel({
    fetch: async () => {
      calls += 1;
      return fakeResponse(403, "application/json", `{"error":"secret-token"}`);
    },
  });
  for (let attempt = 0; attempt < 2; attempt += 1) {
    assert.equal(context.storeSessionKey("test-key"), true);
    await assert.rejects(context.api("/accounts"), error => error.message === "禁止访问 (403)");
  }
  assert.equal(context.storeSessionKey("test-key"), true);
  await assert.rejects(
    context.api("/accounts"),
    error => error.message === "认证多次失败，请检查管理密钥后 60s 再试",
  );
  assert.equal(context.storeSessionKey("test-key"), true);
  await assert.rejects(
    context.api("/accounts"),
    error => error.message === "认证多次失败，请稍后重试（防 IP 封禁）",
  );
  assert.equal(calls, 3);
});

test("panel response raw dashboard errors never reach the grid or toast", async () => {
  const originalError = "credential-value secret-token";
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: originalError,
  });
  const toasts = [];
  context.toast = (...args) => { toasts.push(args.join(" ")); };

  await context.load(false);
  await context.load(true, fakeElement());
  const visible = elements.get("grid").innerHTML + toasts.join(" ");
  assert.match(visible, /请求失败/);
  assert.doesNotMatch(visible, /credential-value|secret-token/);
});

test("credits sort keeps real zero above unknown without mutation", () => {
  const { context } = loadPanel();
  const accounts = [
    { auth_index: "unknown" },
    { auth_index: "zero", credits: { total_remain: 0, total_used: 0, total_size: 0 } },
    { auth_index: "positive", credits: { total_remain: 10, total_used: 0, total_size: 10 } },
  ];
  const original = structuredClone(accounts);
  const ids = list => Array.from(list, account => account.auth_index);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  const button = { textContent: "" };
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["positive", "zero", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["zero", "positive", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  assert.deepEqual(accounts, original);
});

test("available balance excludes disabled and exhausted positive credits", () => {
  const { context, elements } = loadPanel();
  context.renderSummary([
    { region: "cn", credits: { total_remain: 100, total_used: 1, total_size: 101 } },
    { region: "cn", disabled: true, credits: { total_remain: 50, total_used: 2, total_size: 52 } },
    { region: "cn", exhausted: true, credits: { total_remain: 25, total_used: 3, total_size: 28 } },
  ]);
  const html = elements.get("summaryBox").innerHTML;
  assert.match(html, /剩余\(可用\)<\/div><div class="v ok">100<\/div>/);
  assert.match(html, /已用\(消耗\)<\/div><div class="v [^"]*">6<\/div>/);
  assert.match(html, /CN 可用 100 \/ 已用 6/);
  assert.doesNotMatch(html, /剩余\(可用\)<\/div><div class="v ok">175<\/div>/);
});

test("partial import clears credentials and keeps modal open", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const raw = context.document.getElementById("importRaw");
  raw.value = "raw-secret-credential";
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "ok.json", size: 10, async text() { return "credential-one"; } },
    { name: "bad.json", size: 10, async text() { return "credential-two"; } },
  ];
  context.importOneCredential = async value => ({ success: value !== "credential-two" });
  let toastDetail = "";
  context.toast = (title, kind, detail) => { toastDetail = title + " " + kind + " " + detail; };
  let reloads = 0;
  context.load = async () => { reloads += 1; return true; };
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), true);
  assert.equal(raw.value, "");
  assert.equal(files.value, "");
  assert.equal(reloads, 1);
  assert.match(toastDetail, /ok\.json：成功/);
  assert.match(toastDetail, /bad\.json：导入失败/);
  assert.doesNotMatch(toastDetail, /raw-secret|credential-one|credential-two/);
});

test("partial import all success closes modal", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "ok.json", size: 10, async text() { return "credential-one"; } },
  ];
  context.importOneCredential = async () => ({ success: true });
  context.toast = () => {};
  context.load = async () => true;
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), false);
});

test("partial import all failure keeps modal open and sanitizes long names", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "\u0000\u001f\u007f" + "x".repeat(125) + ".json", size: 10, async text() { return "credential-one"; } },
  ];
  context.importOneCredential = async () => ({ success: false });
  let toastDetail = "";
  context.toast = (_title, _kind, detail) => { toastDetail = detail; };
  context.load = async () => { throw new Error("unexpected reload"); };
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), true);
  assert.equal(toastDetail, "0 成功 / 1 失败 · " + "x".repeat(120) + "：导入失败");
});

test("fmtLeft renders cooldown remainders without leaking raw seconds", () => {
  const { context } = loadPanel();
  assert.equal(context.fmtLeft(0), "0s");
  assert.equal(context.fmtLeft(45), "45s");
  assert.equal(context.fmtLeft(60), "1m00s");
  assert.equal(context.fmtLeft(125), "2m05s");
  assert.equal(context.fmtLeft(3725), "1h02m");
  assert.equal(context.fmtLeft(undefined), "0s");
  assert.equal(context.fmtLeft("900"), "15m00s");
});

test("card renders one cooldown row per throttled model with a clear action", () => {
  const { context } = loadPanel();
  const html = context.card({
    auth_index: "3",
    auth_id: "wb-a",
    name: "acct",
    nickname: "账号 A",
    region: "cn",
    plan: "free",
    cooling: [
      { model: "model-1", reason: "rate_limit", seconds: 300 },
      { model: "model-2", reason: "rate_limit", seconds: 0 },
    ],
  });
  assert.match(html, /冷却 1/);
  assert.match(html, /data-action="cooldown-clear"/);
  assert.match(html, /data-model="model-1"/);
  // An already-expired entry is surfaced as 0 and must not get a badge slot.
  assert.doesNotMatch(html, /data-model="model-2"/);
  const without = context.card({ auth_index: "3", auth_id: "wb-a", name: "acct", region: "cn", plan: "free" });
  assert.doesNotMatch(without, /冷却/);
  assert.doesNotMatch(without, /cooldown-list/);
});

test("clearCooldown scopes the request to the host auth id", async () => {
  const { context } = loadPanel();
  context.lastAccounts = [{ auth_index: "3", auth_id: "wb-a" }];
  let path = "";
  let opts = {};
  context.api = async (p, o) => { path = p; opts = o || {}; return { removed: 1 }; };
  context.load = async () => true;
  // The card carries the host auth id; the handler uses it verbatim.
  await context.clearCooldown("wb-a", "model-1", { dataset: {}, innerHTML: "解除", disabled: false });
  assert.equal(path, "/cooldowns/clear?auth_id=wb-a&model=model-1");
  assert.equal(opts.method, "POST");

  await context.clearCooldown("wb-a", "", { dataset: {}, innerHTML: "解除", disabled: false });
  assert.equal(path, "/cooldowns/clear?auth_id=wb-a");
});

test("clearCooldown falls back to the index when no host id is known", async () => {
  const { context } = loadPanel();
  context.lastAccounts = [];
  let path = "";
  context.api = async p => { path = p; return {}; };
  context.load = async () => true;
  await context.clearCooldown("7", null, null);
  assert.equal(path, "/cooldowns/clear?auth_id=7");
});

test("card exposes the host auth id used by the clear action", () => {
  const { context } = loadPanel();
  const html = context.card({
    auth_index: "3",
    auth_id: "wb-a",
    name: "acct",
    region: "cn",
    plan: "free",
    cooling: [{ model: "model-1", seconds: 90 }],
  });
  assert.match(html, /data-auth-id="wb-a"/);
  const fallback = context.card({
    auth_index: "9",
    name: "acct",
    region: "cn",
    plan: "free",
    cooling: [{ model: "model-1", seconds: 90 }],
  });
  assert.match(fallback, /data-auth-id="9"/);
});
// The panel's model state is a module-local `let`, so tests drive the real
// loadModels() path with a stubbed api() rather than assigning it directly.
const CREDITS_MODELS = [
  { id: "glm-5.2", name: "GLM", credits: { value: "x0.79 credits", rate: 0.79, source: "upstream" } },
  { id: "hy3", name: "HY3", credits: { value: "x0.00", rate: 0, source: "upstream" } },
  { id: "deep-model", name: "Deep", credits: { value: "x1.20", rate: 1.2, source: "override" } },
  { id: "mystery", name: "?", credits: { value: "", rate: 0, source: "" } },
];

function loadPanelWithCredits() {
  const panel = loadPanel();
  panel.context.api = async () => ({ models: CREDITS_MODELS, source: "fresh" });
  return panel;
}

test("renderModels shows the credit multiplier with its source", async () => {
  const { context, elements } = loadPanelWithCredits();
  await context.loadModels(false);
  const html = elements.get("modelList").innerHTML;
  assert.match(html, /0\.79x/);
  assert.match(html, /1\.20x/);
  // x0.00 is a promotion, not an unknown rate: it must read as free.
  assert.match(html, /免费/);
  // A model with no multiplier gets no badge at all rather than a bare "0x".
  assert.doesNotMatch(html, /mystery[\s\S]{0,160}?badge/);
  // Every row gets a way to pin the multiplier.
  assert.equal((html.match(/openCreditsEditor\(/g) || []).length, 4);
});

test("openCreditsEditor seeds the input and saveModelCredits posts it", async () => {
  const { context, elements } = loadPanelWithCredits();
  await context.loadModels(false);
  context.openCreditsEditor("hy3");
  const row = elements.get("creditsEditRow");
  assert.equal(row.style.display, "flex");
  assert.equal(elements.get("creditsEditTitle").textContent, "积分倍率 · hy3");
  assert.match(elements.get("creditsEditHint").textContent, /上游同步/);

  context.openCreditsEditor("deep-model");
  // A pinned value seeds the input so editing starts from what is in effect.
  assert.equal(elements.get("creditsEditInput").value, "x1.20");
  assert.match(elements.get("creditsEditHint").textContent, /本地固定/);

  let path = "", body = "";
  const realApi = context.api;
  context.api = async (p, o) => {
    if (p === "/models/credits") { path = p; body = JSON.parse(o.body); return { success: true }; }
    return realApi(p, o);
  };
  let toastDetail = "";
  context.toast = (_t, _k, detail) => { toastDetail = detail; };
  elements.get("creditsEditInput").value = "x0.33";
  await context.saveModelCredits();
  assert.equal(path, "/models/credits");
  assert.deepEqual(body, { model: "deep-model", credits: "x0.33" });
  assert.equal(row.style.display, "none");
  assert.match(toastDetail, /x0\.33/);
});

test("saveModelCredits clears the pin when the input is empty", async () => {
  const { context, elements } = loadPanelWithCredits();
  await context.loadModels(false);
  context.openCreditsEditor("deep-model");
  let body = "";
  const realApi = context.api;
  context.api = async (p, o) => {
    if (p === "/models/credits") { body = JSON.parse(o.body); return { success: true }; }
    return realApi(p, o);
  };
  context.toast = () => {};
  elements.get("creditsEditInput").value = "   ";
  await context.saveModelCredits();
  assert.deepEqual(body, { model: "deep-model", credits: "" });
});

test("saveModelCredits with no model open is a no-op", async () => {
  const { context } = loadPanelWithCredits();
  let calls = 0;
  context.api = async () => { calls += 1; return { models: [], source: "none" }; };
  context.closeCreditsEditor();
  await context.saveModelCredits();
  assert.equal(calls, 0);
});
