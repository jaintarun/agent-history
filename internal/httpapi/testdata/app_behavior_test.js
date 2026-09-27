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
  "include-messages-filter",
  "filter-chips", "results-heading", "results-status", "session-results", "load-more", "detail-pane",
  "detail-empty", "detail-content", "confirm-dialog", "confirm-delete", "command-dialog", "resume-command",
  "copy-status", "copy-command", "settings-dialog", "settings-form", "settings-provider", "settings-model",
  "settings-provider-status", "settings-auto", "settings-cmux-sync", "settings-cmux-status", "settings-error",
  "settings-cancel", "toast"
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

  append(...children) {
    for (const child of children) {
      if (child && typeof child === "object") child.parentNode = this;
      this.children.push(child);
    }
  }
  replaceChildren(...children) {
    this.children = [];
    this.append(...children);
  }
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

function createEnvironment(storedPreference, search = "") {
  const nodes = Object.fromEntries(elementIDs.map((id) => [id, new FakeNode("div", id)]));
  for (const id of ["agent-filter", "active-filter", "cmux-filter", "topic-filter", "status-filter", "sort-filter"]) {
    nodes[id].tagName = "SELECT";
  }
  nodes["settings-provider"].tagName = "SELECT";
  const storage = new Map();
  if (storedPreference !== undefined) storage.set("agent-history.auto-refresh", storedPreference);
  const timeouts = new Map();
  const intervals = new Map();
  const requests = [];
  const historyPaths = [];
  let nextTimerID = 1;

  const environment = {
    nodes,
    storage,
    timeouts,
    intervals,
    requests,
    historyPaths,
    fetchHandler(url) {
      const path = String(url).replace("/test-token/api", "");
      if (path === "/sessions/facets") return Promise.resolve(fakeResponse({ directories: [] }));
      if (path === "/settings") {
        return Promise.resolve(fakeResponse({
          analysis_provider: "codex-cli", analysis_model: "test", analysis_auto: false,
          analysis_providers: [
            { id: "codex-cli", name: "Codex", default_model: "gpt-5.6-luna", available: true },
            { id: "claude-cli", name: "Claude Code", default_model: "haiku", available: false }
          ],
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
    location: { pathname: "/test-token/", search },
    history: { replaceState(_state, _unused, path) { historyPaths.push(path); } },
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

function descendant(parent, predicate) {
  for (const child of parent.children) {
    if (predicate(child)) return child;
    const nested = descendant(child, predicate);
    if (nested) return nested;
  }
  return undefined;
}

async function clickNode(node) {
  const event = { currentTarget: node, target: node, preventDefault() {} };
  const results = (node.listeners.get("click") || []).map((listener) => listener(event));
  await Promise.all(results);
  await settle();
}

async function clickNodeWithBubbling(node) {
  let stopped = false;
  const event = {
    target: node,
    currentTarget: node,
    preventDefault() {},
    stopPropagation() { stopped = true; }
  };
  let current = node;
  while (current && !stopped) {
    event.currentTarget = current;
    const results = (current.listeners.get("click") || []).map((listener) => listener(event));
    await Promise.all(results);
    current = current.parentNode;
  }
  await settle();
}

function assertActionHelp(button, label, description) {
  assert.ok(button, `${label} action missing`);
  assert.match(button.className || "", /(^| )has-action-help( |$)/, `${label} help styling missing`);
  assert.equal(button["aria-label"], label);
  const help = descendant(button, (node) => node.className === "action-help");
  assert.ok(help, `${label} help icon missing`);
  const tooltip = descendant(help, (node) => node.className === "action-tooltip");
  assert.ok(tooltip, `${label} tooltip missing`);
  assert.equal(tooltip.role, "tooltip");
  assert.equal(textOf(tooltip), description);
  assert.equal(button["aria-describedby"], tooltip.id);
  return help;
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
  await selectingB;
  assert.match(textOf(environment.nodes["detail-content"]), /Session B/);
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "queued");
  assert.equal(pollTimers(environment).length, 1);

  pending.get("/sessions/A").resolve(session("A", "current"));
  await selectingA;
  assert.match(textOf(environment.nodes["detail-content"]), /Session B/, "stale success replaced selected detail");
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "queued", "stale success replaced selected status");
  assert.equal(pollTimers(environment).length, 1, "stale success canceled selected polling");

  pending.clear();
  const selectingFailed = evaluate(environment, 'selectSession("failed", false)');
  const selectingRunning = evaluate(environment, 'selectSession("running", false)');
  pending.get("/sessions/running").resolve(session("running", "running"));
  await selectingRunning;
  pending.get("/sessions/failed").reject(new Error("stale failure"));
  await selectingFailed;
  assert.match(textOf(environment.nodes["detail-content"]), /Session running/, "stale failure replaced selected detail");
  assert.equal(evaluate(environment, "state.selectedAnalysisStatus"), "running", "stale failure replaced selected status");
  assert.equal(pollTimers(environment).length, 1, "stale failure canceled selected polling");
}

async function testConversationLoadsOnlyWhenOpened() {
  const environment = createEnvironment("off");
  for (let index = 0; index < 4; index += 1) await settle();
  environment.requests.length = 0;
  environment.fetchHandler = (url) => {
    const path = String(url).replace("/test-token/api", "");
    if (path === "/sessions/A") return Promise.resolve(fakeResponse(session("A", "current")));
    if (path === "/sessions/A/messages?include_tools=false") {
      return Promise.resolve(fakeResponse({ messages: [{ role: "user", text: "Build the app", timestamp: "2026-08-02T10:00:00Z" }] }));
    }
    throw new Error(`unexpected fetch ${path}`);
  };
  await evaluate(environment, 'selectSession("A", false)');
  assert.equal(environment.requests.filter((request) => request.url.includes("/messages")).length, 0);
  const conversation = descendant(environment.nodes["detail-content"], (node) => node.tagName === "DETAILS" && textOf(node).includes("Conversation"));
  assert.ok(conversation, "conversation details should exist");
  assert.equal(conversation.open, false);
  conversation.open = true;
  await Promise.all((conversation.listeners.get("toggle") || []).map((listener) => listener()));
  await settle();
  assert.equal(environment.requests.filter((request) => request.url.endsWith("/messages?include_tools=false")).length, 1, JSON.stringify(environment.requests.map((request) => request.url)));
  assert.match(textOf(conversation), /Build the app/);
  await evaluate(environment, 'selectSession("A", false)');
  assert.equal(environment.requests.filter((request) => request.url.endsWith("/messages?include_tools=false")).length, 2);
  const refreshed = descendant(environment.nodes["detail-content"], (node) => node.tagName === "DETAILS" && textOf(node).includes("Conversation"));
  assert.equal(refreshed.open, true, "refresh should preserve expanded conversation");
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
    },
    {
      agent: "grok",
      bypassLabel: "Resume with YOLO",
      absentLabel: "Resume with dangerously skipped permissions"
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

async function testSearchScopeControl() {
  const focused = createEnvironment(undefined, "?q=ledger");
  await settle();
  assert.equal(focused.nodes["include-messages-filter"].checked, false);
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and topics");
  assert.equal(evaluate(focused, 'searchParams().has("include_messages")'), false);

  focused.nodes["include-messages-filter"].checked = true;
  await dispatch(focused, "include-messages-filter", "change");
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and conversations");
  assert.equal(evaluate(focused, 'searchParams().get("include_messages")'), "true");
  assert.match(focused.historyPaths.at(-1), /include_messages=true/);

  evaluate(focused, "clearFilters()");
  assert.equal(focused.nodes["include-messages-filter"].checked, false);
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and topics");
  assert.equal(evaluate(focused, 'searchParams().has("include_messages")'), false);

  const restored = createEnvironment("off", "?q=ledger&include_messages=true");
  await settle();
  assert.equal(restored.nodes["include-messages-filter"].checked, true);
  assert.equal(restored.nodes["session-search"].placeholder, "Search summaries and conversations");
  assert.equal(evaluate(restored, 'searchParams("next").get("include_messages")'), "true");
  assert.equal(evaluate(restored, 'searchParams("next").get("cursor")'), "next");

  restored.requests.length = 0;
  await dispatch(restored, "refresh-button", "click");
  assert.ok(restored.requests.some((request) =>
    request.url.includes("/sessions?") && request.url.includes("include_messages=true")
  ));
}

async function testAnalysisProviderSettings() {
  const environment = createEnvironment("off");
  await settle();
  await dispatch(environment, "settings-button", "click");

  const provider = environment.nodes["settings-provider"];
  assert.equal(provider.children.length, 2);
  assert.equal(provider.children[0].value, "codex-cli");
  assert.equal(textOf(provider.children[0]), "Codex");
  assert.equal(provider.children[1].value, "claude-cli");
  assert.equal(textOf(provider.children[1]), "Claude Code (Not installed)");
  assert.equal(provider.children[1].disabled, true);

  environment.nodes["settings-model"].value = "gpt-5.6-luna";
  provider.value = "claude-cli";
  await dispatch(environment, "settings-provider", "change");
  assert.equal(environment.nodes["settings-model"].value, "haiku");

  environment.nodes["settings-model"].value = "custom-model";
  provider.value = "codex-cli";
  await dispatch(environment, "settings-provider", "change");
  assert.equal(environment.nodes["settings-model"].value, "custom-model");

  environment.requests.length = 0;
  await dispatch(environment, "settings-form", "submit");
  const update = environment.requests.find((request) =>
    request.url.endsWith("/settings") && request.options.method === "PUT"
  );
  assert.ok(update, "settings update request missing");
  const body = JSON.parse(update.options.body);
  assert.equal(body.analysis_provider, "codex-cli");
  assert.equal(body.analysis_model, "custom-model");
}

async function testCmuxMetadataComparisonAndPush() {
  const environment = createEnvironment("off");
  await settle();
  const originalFetchHandler = environment.fetchHandler;
  environment.fetchHandler = (url, options) => {
    const path = String(url).replace("/test-token/api", "");
    if (path === "/sessions/cmux-session/cmux-title") {
      return Promise.resolve(fakeResponse({ status: "synced" }));
    }
    return originalFetchHandler(url, options);
  };
  const value = {
    ...session("cmux-session", "current"),
    title: "Generated title",
    summary: "Generated short summary",
    cmux: {
      open: true,
      lifecycle: "idle",
      target: "workspace",
      target_title: "Generated title",
      title_state: "synced",
      workspace_description: "Old description",
      description_state: "different",
      surface_title: "Old tab"
    }
  };
  const band = evaluate(environment, `renderCmuxComparison(${JSON.stringify(value)})`);
  const text = textOf(band);
  assert.match(text, /cmux workspace descriptionOld description/);
  assert.match(text, /Agent History descriptionGenerated short summary/);
  const push = descendant(band, (node) => textOf(node) === "Send title and description to cmux");
  assert.ok(push, "metadata push button missing");

  environment.requests.length = 0;
  await clickNode(push);
  assert.ok(environment.requests.some((request) =>
    request.url.endsWith("/sessions/cmux-session/cmux-title") && request.options.method === "POST"
  ));
}

async function testActionHelpDoesNotRunCommands() {
  const environment = createEnvironment("off");
  await settle();
  const staticHelp = [
    ["refresh-button", "Refresh", "Reloads sessions, selected-session details, settings, and cmux status. It does not scan transcript files or run AI."],
    ["retitle-weak-button", "Retitle weak titles", "Uses AI to replace short, generic, or duplicate titles. Session summaries and topics stay unchanged."],
    ["scan-button", "Scan", "Checks all Codex, Claude, and Grok transcript files and imports new or changed sessions. It does not itself run AI analysis."]
  ];
  for (const [id, label, description] of staticHelp) {
    assertActionHelp(environment.nodes[id], label, description);
  }

  const analyzed = { ...session("explained", "current"), topics: [{ title: "One topic" }] };
  const actions = evaluate(environment, `actionButtons(${JSON.stringify(analyzed)})`);
  const expected = [
    ["Reanalyze", "Uses the selected AI provider to replace this session's title, summary, and topics. Existing analysis stays if it fails."],
    ["Retitle", "Uses existing topic summaries to generate only a new title. It does not reread the full conversation."],
    ["Rescan", "Rereads this session's source transcript and imports changes. It does not run AI analysis."]
  ];
  let reanalyzeHelp;
  for (const [label, description] of expected) {
    const button = descendant(actions, (node) => node["aria-label"] === label);
    const help = assertActionHelp(button, label, description);
    if (label === "Reanalyze") reanalyzeHelp = help;
  }

  const unanalyzedActions = evaluate(environment, `actionButtons(${JSON.stringify(session("new", "none"))})`);
  const analyze = descendant(unanalyzedActions, (node) => node["aria-label"] === "Analyze");
  assertActionHelp(analyze, "Analyze", "Uses the selected AI provider to create this session's title, summary, and topics.");

  environment.requests.length = 0;
  await clickNodeWithBubbling(reanalyzeHelp);
  assert.equal(environment.requests.length, 0, "clicking help triggered reanalysis");
}

async function main() {
  await testStoredPreferences();
  await testDisableAndReenable();
  await testDisabledManualRefreshAndPollSuppression();
  await testStaleSelections();
  await testConversationLoadsOnlyWhenOpened();
  await testResumePermissionActions();
  await testSearchScopeControl();
  await testAnalysisProviderSettings();
  await testCmuxMetadataComparisonAndPush();
  await testActionHelpDoesNotRunCommands();
  process.stdout.write("browser behavior assertions passed\n");
}

main().catch((error) => {
  console.error(error.stack || error);
  process.exitCode = 1;
});
