"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const appPath = process.argv[2];
const appSource = fs.readFileSync(appPath, "utf8");
const elementIDs = [
  "result-count", "auto-refresh-toggle", "refresh-button", "refresh-label", "refresh-countdown",
  "retitle-weak-button", "scan-button", "settings-button", "filters", "session-search", "agent-filter",
  "active-filter", "cmux-filter", "cwd-filter", "directory-list", "topic-filter", "active-after-filter",
  "active-before-filter", "started-after-filter", "started-before-filter", "status-filter", "sort-filter",
  "filter-chips", "results-heading", "results-status", "session-results", "load-more", "detail-pane",
  "detail-empty", "detail-content", "confirm-dialog", "confirm-delete", "command-dialog", "resume-command",
  "copy-status", "copy-command", "settings-dialog", "settings-form", "settings-provider", "settings-model",
  "settings-auto", "settings-cmux-sync", "settings-cmux-status", "settings-error", "settings-cancel", "toast"
];

class FakeNode {
  constructor(tagName = "div", id = "") {
    this.tagName = tagName.toUpperCase();
    this.id = id;
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.listeners = new Map();
    this.value = "";
    this.textContent = "";
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.selectedOptions = [];
  }

  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name, value) { this[name] = String(value); }
  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }
  focus() {}
  select() {}
  showModal() { this.open = true; }
  close() { this.open = false; }
}

function fakeResponse(body, status = 200) {
  return {
    status,
    ok: status >= 200 && status < 300,
    json: async () => body
  };
}

function deferredResponse() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = (body, status = 200) => resolvePromise(fakeResponse(body, status));
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function createEnvironment(storedPreference) {
  const nodes = Object.fromEntries(elementIDs.map((id) => [id, new FakeNode("div", id)]));
  for (const id of ["agent-filter", "active-filter", "cmux-filter", "topic-filter", "status-filter", "sort-filter"]) {
    nodes[id].tagName = "SELECT";
  }
  const storage = new Map();
  if (storedPreference !== undefined) storage.set("agent-history.auto-refresh", storedPreference);
  const timeouts = new Map();
  const intervals = new Map();
  const requests = [];
  let nextTimerID = 1;

  const environment = {
    nodes,
    storage,
    timeouts,
    intervals,
    requests,
    fetchHandler(url) {
      const path = String(url).replace("/test-token/api", "");
      if (path === "/sessions/facets") return Promise.resolve(fakeResponse({ directories: [] }));
      if (path === "/settings") {
        return Promise.resolve(fakeResponse({
          analysis_provider: "codex-cli", analysis_model: "test", analysis_auto: false,
          cmux_title_sync: false, cmux_available: true, cmux_access_mode: "allowAll",
          cmux_error: "", cmux_observed_at: ""
        }));
      }
      if (path.startsWith("/sessions?")) return Promise.resolve(fakeResponse({ sessions: [], next_cursor: "" }));
      if (path === "/cmux/refresh") return Promise.resolve(fakeResponse({ status: "refreshed" }));
      throw new Error(`unexpected fetch ${path}`);
    }
  };

  const document = {
    querySelectorAll(selector) {
      assert.equal(selector, "[id]");
      return Object.values(nodes);
    },
    createElement(tagName) { return new FakeNode(tagName); },
    createTextNode(text) {
      const node = new FakeNode("#text");
      node.textContent = text;
      return node;
    }
  };
  const context = vm.createContext({
    console,
    document,
    location: { pathname: "/test-token/", search: "" },
    history: { replaceState() {} },
    navigator: { clipboard: { writeText: async () => {} } },
    window: { confirm: () => true },
    localStorage: {
      getItem: (key) => storage.has(key) ? storage.get(key) : null,
      setItem: (key, value) => storage.set(key, String(value))
    },
    fetch(url, options = {}) {
      requests.push({ url: String(url), options });
      return environment.fetchHandler(url, options);
    },
    setTimeout(callback, delay) {
      const id = nextTimerID++;
      timeouts.set(id, { callback, delay });
      return id;
    },
    clearTimeout: (id) => timeouts.delete(id),
    setInterval(callback, delay) {
      const id = nextTimerID++;
      intervals.set(id, { callback, delay });
      return id;
    },
    clearInterval: (id) => intervals.delete(id),
    AbortController,
    Date,
    Error,
    Intl,
    Promise,
    URLSearchParams
  });
  environment.context = context;
  vm.runInContext(appSource, context, { filename: appPath });
  return environment;
}

function evaluate(environment, source) {
  return vm.runInContext(source, environment.context);
}

async function settle() {
  for (let index = 0; index < 8; index += 1) await Promise.resolve();
}

async function dispatch(environment, id, type) {
  const node = environment.nodes[id];
  const event = { currentTarget: node, target: node, preventDefault() {}, key: "" };
  const results = (node.listeners.get(type) || []).map((listener) => listener(event));
  await Promise.all(results);
  await settle();
}

function textOf(node) {
  return node.textContent + node.children.map((child) => textOf(child)).join("");
}

function childByText(parent, text) {
  return parent.children.find((child) => textOf(child) === text);
}

async function clickNode(node) {
  const event = { currentTarget: node, target: node, preventDefault() {} };
  const results = (node.listeners.get("click") || []).map((listener) => listener(event));
  await Promise.all(results);
  await settle();
}

function session(id, analysisStatus) {
  return {
    id,
    title: `Session ${id}`,
    summary: `Summary ${id}`,
    working_directory: `/${id}`,
    analysis_status: analysisStatus,
    analysis_error: "",
    agent: "codex",
    started_at: "",
    last_active_at: "",
    span_seconds: 0,
    native_session_id: id.toLowerCase(),
    source_path: `/tmp/${id}`,
    topic_count: null,
    topics: []
  };
}

function pollTimers(environment) {
  return [...environment.timeouts.values()].filter((timer) => timer.delay === 1800);
}

async function testStoredPreferences() {
  const defaultOn = createEnvironment();
  await settle();
  assert.equal(evaluate(defaultOn, "state.autoRefresh"), true);
  assert.equal(defaultOn.nodes["auto-refresh-toggle"].checked, true);
  assert.equal(defaultOn.nodes["refresh-countdown"].textContent, "in 60s");
  assert.equal(defaultOn.intervals.size, 1);

  const restoredOff = createEnvironment("off");
  await settle();
  assert.equal(evaluate(restoredOff, "state.autoRefresh"), false);
  assert.equal(restoredOff.nodes["auto-refresh-toggle"].checked, false);
  assert.equal(restoredOff.nodes["refresh-countdown"].textContent, "");
  assert.equal(restoredOff.intervals.size, 0);
}

async function testDisableAndReenable() {
  const environment = createEnvironment();
  await settle();
  evaluate(environment, 'state.selectedID = "queued"; state.selectedAnalysisStatus = "queued"; scheduleSelectedSessionPoll();');
  assert.equal(environment.intervals.size, 1);
  assert.equal(pollTimers(environment).length, 1);
  [...environment.intervals.values()][0].callback();
  assert.equal(environment.nodes["refresh-countdown"].textContent, "in 59s");

  environment.nodes["auto-refresh-toggle"].checked = false;
  await dispatch(environment, "auto-refresh-toggle", "change");
  assert.equal(environment.intervals.size, 0);
  assert.equal(environment.timeouts.size, 0);
  assert.equal(evaluate(environment, "state.pollTimer"), null);
  assert.equal(environment.storage.get("agent-history.auto-refresh"), "off");

  const requestCount = environment.requests.length;
  environment.nodes["auto-refresh-toggle"].checked = true;
  await dispatch(environment, "auto-refresh-toggle", "change");
  assert.equal(environment.nodes["refresh-countdown"].textContent, "in 60s");
  assert.equal(environment.intervals.size, 1);
  assert.equal(environment.requests.length, requestCount, "re-enabling must not fetch immediately");
  assert.equal(environment.storage.get("agent-history.auto-refresh"), "on");
}

async function testDisabledManualRefreshAndPollSuppression() {
  const environment = createEnvironment("off");
  await settle();
  environment.requests.length = 0;

  for (const status of ["queued", "running"]) {
    evaluate(environment, `state.selectedID = "${status}"; state.selectedAnalysisStatus = "${status}"; scheduleSelectedSessionPoll();`);
    assert.equal(pollTimers(environment).length, 0, `${status} must not poll while disabled`);
  }

  await dispatch(environment, "refresh-button", "click");
  assert.ok(environment.requests.some((request) => request.url.endsWith("/cmux/refresh")));
  assert.equal(environment.intervals.size, 0);
  assert.equal(environment.nodes["refresh-label"].textContent, "Refresh");
  assert.equal(environment.nodes["refresh-countdown"].textContent, "");
}

async function testStaleSelections() {
  const environment = createEnvironment();
  await settle();
  const pending = new Map();
  environment.fetchHandler = (url) => {
    const path = String(url).replace("/test-token/api", "");
    const deferred = deferredResponse();
    pending.set(path, deferred);
    return deferred.promise;
  };

  evaluate(environment, 'state.selectedAnalysisStatus = "current"');
  const selectingA = evaluate(environment, 'selectSession("A", false)');
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "", "a new selection must clear the prior status");
  const selectingB = evaluate(environment, 'selectSession("B", false)');
  pending.get("/sessions/B").resolve(session("B", "queued"));
  pending.get("/sessions/B/messages?include_tools=true").resolve({ messages: [] });
  await selectingB;
  assert.match(textOf(environment.nodes["detail-content"]), /Session B/);
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "queued");
  assert.equal(pollTimers(environment).length, 1);

  pending.get("/sessions/A").resolve(session("A", "current"));
  pending.get("/sessions/A/messages?include_tools=true").resolve({ messages: [] });
  await selectingA;
  assert.match(textOf(environment.nodes["detail-content"]), /Session B/, "stale success replaced selected detail");
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "queued", "stale success replaced selected status");
  assert.equal(pollTimers(environment).length, 1, "stale success canceled selected polling");

  pending.clear();
  const selectingFailed = evaluate(environment, 'selectSession("failed", false)');
  const selectingRunning = evaluate(environment, 'selectSession("running", false)');
  pending.get("/sessions/running").resolve(session("running", "running"));
  pending.get("/sessions/running/messages?include_tools=true").resolve({ messages: [] });
  await selectingRunning;
  pending.get("/sessions/failed").reject(new Error("stale failure"));
  pending.get("/sessions/failed/messages?include_tools=true").resolve({ messages: [] });
  await selectingFailed;
  assert.match(textOf(environment.nodes["detail-content"]), /Session running/, "stale failure replaced selected detail");
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "running", "stale failure replaced selected status");
  assert.equal(pollTimers(environment).length, 1, "stale failure canceled selected polling");
}

async function testResumePermissionActions() {
  const environment = createEnvironment("off");
  await settle();
  environment.requests.length = 0;
  environment.fetchHandler = (url) => {
    const path = String(url).replace("/test-token/api", "");
    if (path.endsWith("/launch")) {
      return Promise.resolve(fakeResponse({ mode: "copy", command: "resume command" }));
    }
    throw new Error(`unexpected fetch ${path}`);
  };

  for (const test of [
    {
      agent: "codex",
      bypassLabel: "Resume with YOLO",
      absentLabel: "Resume with dangerously skipped permissions"
    },
    {
      agent: "claude",
      bypassLabel: "Resume with dangerously skipped permissions",
      absentLabel: "Resume with YOLO"
    }
  ]) {
    const sourceSession = { ...session("resume", "current"), agent: test.agent };
    const actions = evaluate(environment, `actionButtons(${JSON.stringify(sourceSession)})`);
    const normal = childByText(actions, "Resume normally");
    const bypass = childByText(actions, test.bypassLabel);
    assert.ok(normal, `${test.agent} normal resume missing`);
    assert.ok(bypass, `${test.agent} bypass resume missing`);
    assert.equal(childByText(actions, test.absentLabel), undefined);

    await clickNode(normal);
    await clickNode(bypass);
    const recent = environment.requests.slice(-2).map((request) => JSON.parse(request.options.body));
    assert.deepEqual(recent, [
      { launcher: "auto", permissions: "normal" },
      { launcher: "auto", permissions: "bypass" }
    ]);
  }
}

async function main() {
  await testStoredPreferences();
  await testDisableAndReenable();
  await testDisabledManualRefreshAndPollSuppression();
  await testStaleSelections();
  await testResumePermissionActions();
  process.stdout.write("browser behavior assertions passed\n");
}

main().catch((error) => {
  console.error(error.stack || error);
  process.exitCode = 1;
});
