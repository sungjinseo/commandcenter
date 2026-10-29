// Access and system screens (plan 10 Decision 8; the tasks and cleanup
// policies are in ops.js, users, roles, realms and the account page in
// access.js): API tokens, the support screens (status, logs, metrics, support
// ZIP, system information), nodes, the API view and the trust store. Admin
// screens use the routes: the list at "#admin/<group>/<screen>", a detail at
// "#admin/<group>/<screen>:<id>".
import {
  state, h, put, api, enc, encSeg, alive, filterBar, row, fmtBytes, fmtNum, fmtDate, fmtUptime, taskName,
  copyLine, copyButton, pageHead, loading, truncNote, card, kv, stat, tbl, handleError, needLogin, icon, s, markBusy,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete, viewRepos } from "./browse.js";
import { viewRepoForm, DEFAULT_RECIPE } from "./repoform.js";
import { viewTasks, viewPolicies, trendCard, logsCard, viewRoutingRules, viewSelectors, viewWebhooks, viewStaging } from "./ops.js";
import { viewUsers, viewRoles, viewRealms, viewRealmConfig, viewPrivileges, viewUserTokens } from "./access.js";
import { viewBlobStores, viewGeneral, viewVulnerability, viewQuarantine } from "./settings.js";
import { field, fields, sel, textIn, button, formState, errList } from "./ops.js";

const dash = "—";
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const acts = (...b) => h("div", { class: "actions" }, b);
const sub = (text) => h("p", null, text);

// downloadJSON saves data as a file through a Blob URL, then revokes it.
function downloadJSON(name, data) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2) + "\n"], { type: "application/json" }));
  const a = h("a", { href: url, download: name, hidden: true });
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000); // slow disks may still be reading it
}

const notFound = (text) => h("section", { class: "card" }, h("div", { class: "empty" }, text));

// ---- API tokens -------------------------------------------------------------------

// viewTokens: admin shows every user's tokens ("#admin/security/apitokens")
// and can create one for another user; otherwise only the caller's own.
export async function viewTokens(view, admin) {
  if (state.me.anonymous) return needLogin(view, t("nav.tokens"));
  const isLive = alive();
  const me = state.me;
  const created = h("div", null);
  let shownId = ""; // the token in the show-once box
  // clearShown drops the show-once box, so the secret leaves the page.
  let unBusy = null; // the secret on screen is in-progress work for a language change
  const clearShown = () => { shownId = ""; put(created); if (unBusy) { unBusy(); unBusy = null; } };
  const list = h("div", null, loading());
  const userInput = admin ? h("input", { id: "tok-user", type: "text", placeholder: me.user, autocomplete: "off" }) : null;
  const btn = h("button", { type: "submit", class: "btn primary" }, icon("token"), t("tok.create"));
  const form = h("form", null,
    userInput ? h("div", { class: "field" }, h("label", { for: "tok-user" }, t("tok.user")), userInput, h("small", null, t("tok.userHelp"))) : null,
    h("div", { class: "notice" }, t("tok.about")),
    h("div", { class: "savebar" }, btn));

  const load = async () => {
    const data = await api("tokens");
    if (!isLive()) return;
    // The API gives an admin every token; the user page keeps only theirs. Token owners are
    // owner keys ("ldap:<id>" for an LDAP user), so they are matched against me.owner, not me.user.
    const toks = (data.tokens || []).filter((x) => admin || x.user === me.owner);
    const revoke = (x) => {
      const b = h("button", { type: "button", class: "btn small danger" }, t("tok.revoke"));
      b.addEventListener("click", async () => {
        const text = x.user === me.owner ? t("tok.revokeText", { id: x.id }) : t("tok.revokeOther", { id: x.id, user: x.user });
        if (!(await confirmDelete(text, t("tok.revokeTitle"), t("tok.revoke")))) return;
        b.disabled = true;
        try {
          await api("tokens/" + enc(x.id), { method: "DELETE" });
        } catch (e) {
          b.disabled = false;
          handleError(e);
          return;
        }
        if (x.id === shownId) clearShown();
        try { await load(); } catch (e) { handleError(e); }
      });
      return b;
    };
    const rows = toks.map((x) => row([
      [h("code", null, x.id)],
      admin ? [x.user] : null,
      [fmtDate(x.created), "nowrap"],
      [x.lastUsed ? fmtDate(x.lastUsed) : badge(t("tok.never")), "nowrap"],
      [revoke(x), "act"],
    ].filter(Boolean), x.user + " " + x.id));
    const f = filterBar(rows, admin ? t("tok.filterAdmin") : t("tok.filter"), "tok.count");
    const c = card(admin ? t("tok.all") : t("tok.mine"), admin ? t("tok.allSub") : t("tok.mineSub"), null);
    c.append(f.bar, tbl([t("tok.id"), admin ? t("users.user") : null, t("tok.created"), t("tok.lastUsed"), { t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" }].filter(Boolean),
      rows, t("tok.none")), f.none);
    put(list, c, data.truncated ? truncNote(t("tok.truncated")) : null);
  };

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    btn.disabled = true;
    try {
      const u = userInput ? userInput.value.trim() : "";
      const x = await api("tokens", { method: "POST", body: u ? { user: u } : {} });
      if (!isLive()) return;
      shownId = x.id;
      if (!unBusy) unBusy = markBusy("token");
      const hide = h("button", { type: "button", class: "btn small" }, t("tok.hide"));
      hide.addEventListener("click", clearShown);
      put(created, h("div", { class: "token-box", role: "status" },
        h("strong", null, t("tok.showTitle")),
        h("p", { class: "sub" }, t("tok.showText", { user: x.user, id: x.id })),
        copyLine(x.token, t("tok.copy")),
        h("div", { class: "actions end" }, hide)));
      if (userInput) userInput.value = "";
      await load();
    } catch (e) {
      handleError(e);
    } finally {
      btn.disabled = false;
    }
  });

  put(view,
    pageHead(t("nav.tokens"), sub(admin ? t("tok.subAdmin") : t("tok.sub", { user: me.user })), null, { eyebrow: t("users.eyebrow") }),
    created,
    h("div", { class: "stack" }, list, card(t("tok.new"), t("tok.newSub"), form)));
  await load();
}

// ---- metrics: runtime figures, Prometheus, thread dump ---------------------------------
//   #admin/support/metrics

const rssSamples = []; // last 60 RSS samples, kept across visits
let lastMetrics = null;
let statusUser = null; // whose samples these are

function sparkline(values) {
  const W = 300, H = 60;
  const svg = s("svg", { class: "spark", viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img", "aria-label": t("st.rssAria") });
  if (!values.length) return h("div", { class: "empty" }, t("st.noRss"));
  let min = Math.min(...values), max = Math.max(...values);
  if (max - min < 1) { max += 1; min -= 1; }
  const pad = (max - min) * 0.1;
  min -= pad; max += pad;
  const step = W / Math.max(1, values.length - 1);
  const x0 = 0;
  const pts = values.map((v, i) => (x0 + i * step).toFixed(1) + "," + (H - (v - min) / (max - min) * H).toFixed(1));
  if (values.length === 1) pts.push(W + "," + (H - (values[0] - min) / (max - min) * H).toFixed(1));
  svg.appendChild(s("polygon", { class: "area", points: x0.toFixed(1) + "," + H + " " + pts.join(" ") + " " + W + "," + H }));
  svg.appendChild(s("polyline", { class: "line", points: pts.join(" ") }));
  return svg;
}

// metrics returns the metrics nodes; it polls GET /-/metrics every 5 s
// while the page is visible, until the route changes.
function metrics(isLive) {
  if (statusUser !== state.me.user) { // another user signed in: drop the old samples
    rssSamples.length = 0;
    lastMetrics = null;
    statusUser = state.me.user;
  }
  const stats = h("div", { class: "stats" }, loading());
  const spark = h("div", null, h("div", { class: "empty" }, t("st.noRss")));
  const updated = h("span", { class: "badge", "aria-live": "polite" }, "");
  const render = (st) => {
    const last = rssSamples.length ? rssSamples[rssSamples.length - 1] : 0;
    put(stats,
      stat({ label: "RSS", value: st.rssBytes ? fmtBytes(st.rssBytes) : dash, foot: t("st.rssFoot") }),
      stat({ label: "Heap", value: fmtBytes(st.heapAllocBytes), foot: t("st.heapFoot", { n: fmtBytes(st.heapSysBytes) }) }),
      stat({ label: "Goroutines", value: fmtNum(st.goroutines), foot: t("st.gcFoot", { n: fmtNum(st.numGC) }) }),
      stat({ label: "Sys", value: fmtBytes(st.sysBytes), foot: t("met.sysFoot") }));
    put(spark, sparkline(rssSamples),
      h("div", { class: "spark-meta" },
        h("span", null, rssSamples.length ? t("st.min", { n: fmtBytes(Math.min(...rssSamples)) }) : ""),
        h("span", null, last ? t("st.now", { n: fmtBytes(last) }) : t("st.noRss")),
        h("span", null, rssSamples.length ? t("st.max", { n: fmtBytes(Math.max(...rssSamples)) }) : "")));
  };
  let busy = false, stopped = false;
  async function poll() {
    if (busy || stopped || !isLive()) return;
    busy = true;
    try {
      const st = await api("/-/metrics");
      if (!isLive()) return;
      if (st.rssBytes) {
        rssSamples.push(st.rssBytes);
        if (rssSamples.length > 60) rssSamples.splice(0, rssSamples.length - 60);
      }
      lastMetrics = st;
      render(st);
      updated.textContent = t("st.updated", { time: fmtDate(new Date()) });
    } catch (e) {
      if (isLive()) {
        if (e.status === 401 || e.status === 403) stop(); // signed out or not allowed: polling cannot succeed
        handleError(e);
      }
    } finally {
      busy = false;
    }
  }
  const timer = setInterval(() => { if (document.visibilityState === "visible") poll(); }, 5000);
  const onVis = () => { if (document.visibilityState === "visible") poll(); };
  document.addEventListener("visibilitychange", onVis);
  const stop = () => { stopped = true; clearInterval(timer); document.removeEventListener("visibilitychange", onVis); };
  state.leaving.push(stop);
  if (lastMetrics) render(lastMetrics); // no flash when coming back
  poll();
  return { stats, card: card(t("st.rssTitle"), t("st.rssSub"), spark, updated) };
}

// fetchText gets a text/plain API answer (api() expects JSON).
async function fetchText(path) {
  const res = await fetch("/-/api/" + path, { credentials: "same-origin", headers: { "X-Command-Center-UI": "1" } });
  if (!res.ok) {
    let msg = res.status + " " + res.statusText;
    try { const d = await res.json(); if (d && d.error) msg = d.error; } catch (e) { /* not JSON */ }
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return res.text();
}

async function viewMetrics(view) {
  const isLive = alive();
  const st = await api("settings").catch(() => null);
  if (!isLive()) return;
  const m = metrics(isLive);

  // thread dump
  const pre = h("pre", { class: "code threads", tabindex: "0", hidden: true });
  const tools = h("div", { class: "actions", hidden: true });
  const load = button(t("sup.threadsLoad"), "primary");
  let dumpURL = null;
  state.leaving.push(() => { if (dumpURL) URL.revokeObjectURL(dumpURL); });
  load.addEventListener("click", async () => {
    load.disabled = true;
    try {
      const text = await fetchText("support/threads");
      if (!isLive()) return;
      pre.textContent = text;
      pre.hidden = false;
      if (dumpURL) URL.revokeObjectURL(dumpURL);
      dumpURL = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }));
      const dl = h("a", { class: "btn small", download: "threads.txt" }, t("sup.threadsSave"));
      dl.setAttribute("href", dumpURL); // trusted URL created above
      put(tools, copyButton(text, pre, t("sup.threadsCopy")), dl, h("span", { class: "tiny muted" }, t("sup.threadsSize", { n: fmtNum(text.split("\n").length) })));
      tools.hidden = false;
    } catch (e) {
      if (isLive()) handleError(e);
    } finally { load.disabled = false; }
  });
  const threads = card(t("sup.threadsTitle"), t("sup.threadsSub"), [h("div", { class: "actions" }, load, tools), pre]);

  // Prometheus endpoint and the anonymous-scrape setting
  const anon = h("input", { type: "checkbox", checked: !!(st && st.metricsAnonymous), disabled: !st });
  const anonMsg = h("p", { class: "tiny muted", role: "status" });
  anon.addEventListener("change", async () => {
    anon.disabled = true;
    try {
      await api("settings", { method: "PUT", body: { metricsAnonymous: anon.checked } });
      if (isLive()) anonMsg.textContent = t("gen.saved");
    } catch (e) {
      if (!isLive()) return;
      anon.checked = !anon.checked;
      handleError(e);
    } finally { anon.disabled = false; }
  });
  const prom = card(t("sup.metricsTitle"), t("sup.metricsSub"), [
    copyLine(location.origin + "/-/metrics/prometheus", t("sup.metricsCopy")),
    h("p", { class: "tiny muted metrics-auth" }, t("sup.metricsAuth")),
    h("div", { class: "field" }, h("label", { class: "check" }, anon, t("sup.metricsAnon")), h("small", null, t("sup.metricsAnonHelp"))), anonMsg]);

  put(view, pageHead(t("nav.metrics"), sub(t("met.sub"))),
    m.stats,
    h("div", { class: "two" }, trendCard(isLive, "24h", t("st.trend"), t("st.trendSub")), m.card),
    h("div", { class: "two" }, prom, threads));
}

// ---- status: the health checks ---------------------------------------------------------
//   #admin/support/status[:<check>]

const statusLink = (name) => "#admin/support/status" + (name ? ":" + enc(name) : "");
const healthBadge = (ok) => badge([h("span", { class: "dot " + (ok ? "ok" : "err"), "aria-hidden": "true" }), ok ? t("st.ok") : t("st.bad")]);

async function viewStatus(view, name) {
  const isLive = alive();
  let checks = null, info = null;
  const box = h("div", null, loading());
  const about = h("span");
  const refresh = h("button", { type: "button", class: "btn" }, icon("refresh"), t("st.refresh"));
  const dl = h("button", { type: "button", class: "btn", disabled: true }, t("common.download"));

  const render = () => {
    if (name !== undefined) {
      const c = checks.find((x) => x.name === name);
      put(box, c ? card(c.name, t("st.checkSub"), [
        kv(t("col.status"), healthBadge(c.healthy)),
        kv(t("st.message"), c.message || dash),
        kv(t("st.duration"), fmtNum(c.durationMs) + " ms"),
        kv(t("st.lastChecked"), fmtDate(c.lastChecked)),
      ], h("a", { class: "btn small", href: statusLink() }, t("st.allChecks"))) : notFound(t("st.notFound", { name })));
      return;
    }
    const bad = checks.filter((c) => !c.healthy).length;
    const table = card(t("st.checks"), null, null);
    table.appendChild(tbl([t("col.name"), t("col.status"), t("st.message"), t("st.lastChecked")], checks.map((c) => row([
      [h("a", { href: statusLink(c.name) }, c.name)], [healthBadge(c.healthy)], [c.message || dash],
      [h("span", { title: t("st.duration") + ": " + fmtNum(c.durationMs) + " ms" }, fmtDate(c.lastChecked)), "nowrap"],
    ])), t("st.none")));
    put(box, checks.length ? h("p", { class: "status-summary" + (bad ? " bad" : ""), role: "status" },
      bad ? t("st.sumBad", { n: fmtNum(bad) }) : t("st.sumOk", { n: fmtNum(checks.length) })) : null, table);
  };

  const load = async () => {
    refresh.disabled = true;
    try {
      const [d, st] = await Promise.all([api("status/checks"), api("status").catch(() => null)]);
      if (!isLive()) return;
      checks = d.checks || [];
      info = st;
      put(about, st ? " " + [st.version ? t("st.version", { v: st.version }) : "", t("st.uptime") + " " + fmtUptime(st.uptimeSeconds)].filter(Boolean).join(" · ") : null);
      dl.disabled = false;
      render();
    } catch (e) {
      if (!isLive()) return;
      put(box, notFound(t("st.loadFailed")));
      handleError(e);
    } finally {
      refresh.disabled = false;
    }
  };
  refresh.addEventListener("click", load);
  dl.addEventListener("click", () => downloadJSON("status.json", { generated: new Date().toISOString(), status: info, checks }));
  put(view, pageHead(t("nav.status"), h("p", null, t("st.sub"), about), acts(refresh, dl)), box);
  await load();
}

// ---- logs and the log level ------------------------------------------------------------
//   #admin/support/logs[?repo=&range=]   #admin/support/logging

async function viewLogs(view, criteria) {
  const isLive = alive();
  put(view, pageHead(t("nav.logs")), logsCard(isLive, criteria));
}

const LOG_LEVELS = ["debug", "info", "warn", "error"]; // what PUT /-/api/logging accepts

async function viewLogging(view) {
  const isLive = alive();
  const lg = await api("logging").catch((e) => ({ error: e }));
  if (!isLive()) return;
  const level = sel(LOG_LEVELS.map((l) => [l, l.toUpperCase()]), lg && LOG_LEVELS.includes(lg.level) ? lg.level : "info", { "aria-label": t("sup.level") });
  const levelMsg = h("p", { class: "tiny muted", role: "status" });
  const unavailable = lg && lg.error;
  if (unavailable) {
    level.disabled = true;
    levelMsg.textContent = lg.error.status === 501 ? t("sup.levelFixed") : lg.error.message;
  }
  level.addEventListener("change", async () => {
    level.disabled = true;
    try {
      const out = await api("logging", { method: "PUT", body: { level: level.value } });
      if (!isLive()) return;
      if (out && LOG_LEVELS.includes(out.level)) level.value = out.level;
      levelMsg.textContent = t("sup.levelSet", { level: level.value.toUpperCase() });
    } catch (e) {
      if (!isLive()) return;
      levelMsg.textContent = "";
      handleError(e);
    } finally { level.disabled = !!unavailable; }
  });
  put(view, pageHead(t("nav.logging"), sub(t("sup.levelSub"))),
    card(t("sup.levelTitle"), null, [
      fields(field(t("sup.level"), level, t("sup.levelHelp"), { full: true })), levelMsg,
      h("div", { class: "notice amber" }, t("sup.levelNote"))]));
}

// ---- support ZIP -----------------------------------------------------------------------
//   #admin/support/supportzip

const ZIP_ENTRIES = ["info.json", "config.json", "repositories.json", "blobstores.json", "status.json", "sysinfo.json", "tasks.json", "webhooks.json", "request-stats.json", "events.json", "logs.txt", "threads.txt", "metrics.txt"];

async function viewSupportZip(view) {
  // a plain link, so the browser saves the zip
  put(view, pageHead(t("nav.supportzip"), sub(t("sup.zipSub"))),
    card(t("sup.zipTitle"), null, [
      h("p", { class: "tiny" }, t("sup.zipContains")),
      h("p", { class: "tiny mono zip-entries" }, ZIP_ENTRIES.join(" · ")),
      h("div", { class: "notice" }, t("sup.zipRedacted")),
      h("div", { class: "actions" }, h("a", { class: "btn primary", href: "/-/api/support/zip", download: "" }, t("sup.zipDownload")))]));
}

// ---- system information ---------------------------------------------------------------
//   #admin/support/systeminformation: Nexus's sections (GET /service/rest/atlas/system-information)

const BYTE_KEY = /bytes$|^heap|^sys$|memory$|space$/i;
const SYS_ORDER = ["nexus-status", "nexus-node", "nexus-configuration", "nexus-properties", "system-time", "system-runtime", "system-filestores"]; // Nexus's order

function sysValue(k, v) {
  if (v === "" || v === null || v === undefined) return dash;
  if (typeof v === "object") return Object.entries(v).map(([a, b]) => a + "=" + (b === null ? dash : b)).join(", ") || dash;
  if (typeof v === "number" && BYTE_KEY.test(k) && v >= 1024) return fmtNum(v) + " (" + fmtBytes(v) + ")";
  return String(v);
}

async function viewSysInfo(view) {
  const isLive = alive();
  const d = await api("/service/rest/atlas/system-information");
  if (!isLive()) return;
  const dl = h("button", { type: "button", class: "btn" }, t("common.download"));
  dl.addEventListener("click", () => downloadJSON("sysinfo.json", d));
  const copy = copyButton(JSON.stringify(d, null, 2), null, t("sys.copy"), t("sys.copy"));
  copy.classList.remove("small");
  put(view, pageHead(t("nav.sysinfo"), sub(t("sys.sub")), acts(copy, dl)),
    h("div", { class: "stack sysinfo" }, SYS_ORDER.filter((n) => d[n]).concat(Object.keys(d).filter((n) => !SYS_ORDER.includes(n))).map((n) => {
      const c = card(n, null, null);
      c.appendChild(tbl([t("col.name"), t("sys.value")], Object.entries(d[n] || {}).map(([k, v]) => row([[k, "mono"], [sysValue(k, v)]])), t("common.noItems")));
      return c;
    })));
}

// ---- nodes ----------------------------------------------------------------------------
//   #admin/system/nodes

async function viewNodes(view) {
  const isLive = alive();
  const [d, td] = await Promise.all([api("nodes"), api("tasks").catch(() => null)]);
  if (!isLive()) return;
  const tasks = new Map(((td && td.tasks) || []).map((x) => [x.id, x]));
  const taskCell = (id) => (!id ? dash : tasks.has(id) ? h("a", { href: "#admin/system/tasks:" + enc(id) }, taskName(tasks.get(id))) : h("code", null, id));
  const yes = (on, cls) => (on ? dotBadge(t("nodes.yes"), cls) : h("span", { class: "muted" }, t("nodes.no")));
  const c = card(t("nav.nodes"), t("nodes.cardSub"), null);
  c.appendChild(tbl([t("nodes.id"), t("nodes.host"), t("col.version"), t("nodes.schema"), t("nodes.started"), t("nodes.heartbeat"), t("nodes.alive"), t("nodes.draining"), t("nodes.leader"), t("nodes.task")],
    (d.nodes || []).map((n) => row([
      [[h("code", null, n.id), n.id === d.self ? [" ", badge(t("nodes.self"), "blue")] : null], "name"],
      [n.hostname || dash], [n.version || dash], [String(n.schema), "num"],
      [fmtDate(n.startedAt), "nowrap"], [fmtDate(n.heartbeatAt), "nowrap"],
      [yes(n.alive, "green")], [yes(n.draining, "amber")], [yes(n.leader, null)],
      [taskCell(n.runningTask)],
    ])), t("nodes.none")));
  put(view, pageHead(t("nav.nodes"), sub(t("nodes.sub"))), c);
}

// ---- API: a readable view of the OpenAPI document ---------------------------------------
//   #admin/system/api

const METHODS = ["get", "post", "put", "delete"];

async function viewApi(view) {
  const isLive = alive();
  const doc = await api("openapi.json");
  if (!isLive()) return;
  const groups = new Map();
  for (const [path, ops] of Object.entries(doc.paths || {})) {
    for (const [method, op] of Object.entries(ops || {})) {
      const tag = (op.tags && op.tags[0]) || "other";
      if (!groups.has(tag)) groups.set(tag, []);
      groups.get(tag).push({ path, method, op });
    }
  }
  const items = [];
  const ref = (o) => { const r = o && o.content && o.content["application/json"] && o.content["application/json"].schema; return r && r.$ref ? r.$ref.split("/").pop() : ""; };
  const cards = [...groups.keys()].sort().map((tag) => {
    const list = groups.get(tag).sort((a, b) => a.path.localeCompare(b.path) || METHODS.indexOf(a.method) - METHODS.indexOf(b.method));
    const els = list.map(({ path, method, op }) => {
      const query = (op.parameters || []).filter((p) => p.in === "query").map((p) => p.name);
      const body = ref(op.requestBody);
      const el = h("div", { class: "api-op" },
        h("div", { class: "api-line" }, h("span", { class: "api-m m-" + method }, method.toUpperCase()), h("code", null, path)),
        h("p", null, op.summary || dash),
        h("p", { class: "tiny muted" }, [
          op.description ? op.description.replace(/^Requires: /, t("api.auth") + ": ").replace(/\.$/, "") : t("api.public"),
          query.length ? t("api.query") + ": " + query.join(", ") : "",
          body ? t("api.body") + ": " + body : "",
          t("api.replies") + ": " + Object.keys(op.responses || {}).join(", "),
        ].filter(Boolean).join(" · ")));
      el.dataset.key = (method + " " + path + " " + (op.summary || "") + " " + tag).toLowerCase();
      items.push(el);
      return el;
    });
    // a group opens on a click, or by itself while a filter matches in it
    return h("details", { class: "card system-info api-group" },
      h("summary", { class: "card-head" }, h("div", null, h("h2", null, tag), h("p", null, t("api.count", { n: fmtNum(els.length) }))), icon("chevron", "api-chev")),
      h("div", { class: "card-body" }, els));
  });
  const f = filterBar(items, t("api.filter"), "api.shown");
  f.bar.addEventListener("input", (ev) => {
    const q = ev.target.value.trim();
    for (const c of cards) {
      c.hidden = ![...c.querySelectorAll(".api-op")].some((x) => !x.hidden);
      c.open = !!q && !c.hidden;
    }
  });
  put(view, pageHead(t("nav.api"), sub(t("api.sub", { v: (doc.info && doc.info.version) || "" })),
    acts(h("a", { class: "btn", href: "/-/api/openapi.json", download: "openapi.json" }, t("api.download")))),
    h("section", { class: "card" }, f.bar), f.none, h("div", { class: "stack api-list" }, cards));
}

// ---- SSL certificates (the trust store) ------------------------------------------------
//   #admin/security/sslcertificates

const certName = (c) => c.subjectCommonName || c.subject || c.id;
const fp = (id) => h("code", { class: "fp", title: id }, String(id || "").replace(/(.{2})(?=.)/g, "$1:"));
const dotBadge = (text, cls) => badge([h("span", { class: "dot", "aria-hidden": "true" }), text], cls);
const expiry = (c) => (c.expired ? dotBadge(t("ssl.expired"), "red") : h("span", null, fmtDate(c.notAfter)));
const certMeta = (c) => h("div", { class: "tiny muted" }, [c.isCA ? t("ssl.ca") : t("ssl.leaf"), c.issuerCommonName || c.issuer].filter(Boolean).join(" · "));

async function viewCertificates(view) {
  const isLive = alive();
  const eb = { eyebrow: t("users.eyebrow") };
  const listBox = h("div", { class: "stack" }, loading()); // a stack, so the card in it has no bottom margin
  const fs = formState(h("form"));
  let known = new Set();

  // addPEM stores the certificate(s) of a PEM text; 409 means every one was there.
  const addPEM = async (pem) => {
    try {
      const out = await api("ssl/certificates", { method: "POST", body: { pem } });
      if (!isLive()) return false;
      put(fs.ok, t("ssl.added", { n: fmtNum((out.certificates || []).length) }));
      fs.ok.hidden = false;
      await load();
      return true;
    } catch (e) {
      if (!isLive()) return false;
      if (e.status === 401 || e.status === 403) { handleError(e); return false; }
      fs.show(e.status === 409 ? [t("ssl.exists")] : errList(e));
      return false;
    }
  };
  const load = async () => {
    const d = await api("ssl/certificates");
    if (!isLive()) return;
    const certs = d.certificates || [];
    known = new Set(certs.map((c) => c.id));
    const rows = certs.map((c) => {
      const del = button(t("common.delete"), "small danger");
      del.addEventListener("click", async () => {
        if (!(await confirmDelete(t("ssl.delText", { name: certName(c) }), t("ssl.delTitle")))) return;
        del.disabled = true;
        try {
          await api("ssl/certificates/" + encSeg(c.id), { method: "DELETE" });
        } catch (e) {
          del.disabled = false;
          handleError(e);
          return;
        }
        if (isLive()) { put(fs.ok, t("ssl.deleted", { name: certName(c) })); fs.ok.hidden = false; load().catch(handleError); }
      });
      return row([
        [[h("strong", null, certName(c)), certMeta(c)], "name"],
        [c.subjectOrganization || c.issuerOrganization || dash],
        [expiry(c), "nowrap"],
        [fp(c.id)],
        [del, "act"],
      ], certName(c) + " " + c.subject + " " + c.issuer + " " + c.id);
    });
    const f = filterBar(rows, t("ssl.filter"), "ssl.count");
    const c = card(t("ssl.listTitle"), t("ssl.listSub"), null);
    c.append(f.bar, tbl([t("ssl.subject"), t("ssl.org"), t("ssl.expires"), t("ssl.fingerprint"), { t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" }], rows, t("ssl.none")), f.none);
    put(listBox, c);
    chainRows.querySelectorAll("button[data-id]").forEach((b) => { if (known.has(b.dataset.id)) b.replaceWith(dotBadge(t("ssl.trusted"), "green")); });
  };

  // retrieve a server's chain
  const hostIn = textIn("", { placeholder: "ldap.corp.local", maxlength: 253, class: "mono", autocomplete: "off" });
  const portIn = h("input", { type: "number", inputmode: "numeric", min: 1, max: 65535, step: 1, value: "443" });
  const run = h("button", { type: "submit", class: "btn primary" }, t("ssl.fetch"));
  const chainRows = h("div", { class: "chain" });
  const fetchForm = h("form", { novalidate: true });
  const ffs = formState(fetchForm);
  put(fetchForm, ffs.err, fields(field(t("ssl.host"), hostIn, t("ssl.hostHelp"), { req: true }), field(t("ssl.port"), portIn, t("ssl.portHelp"))),
    h("div", { class: "actions end" }, run), chainRows);
  fetchForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    ffs.clear();
    fs.clear();
    const host = hostIn.value.trim(), port = Number(portIn.value) || 443;
    if (!host) { ffs.show([t("ssl.e.host")]); return; }
    run.disabled = true;
    put(chainRows, loading());
    try {
      const d = await api("ssl/retrieve", { method: "POST", body: { host, port } });
      if (!isLive()) return;
      const certs = d.certificates || [];
      put(chainRows, h("p", { class: "tiny muted" }, t("ssl.chainOf", { host: d.host + ":" + d.port, n: fmtNum(certs.length) })),
        certs.map((c) => {
          let act;
          if (c.trusted || known.has(c.id)) act = dotBadge(t("ssl.trusted"), "green");
          else {
            act = button(t("ssl.add"), "small primary");
            act.dataset.id = c.id;
            act.addEventListener("click", async () => { act.disabled = true; if (!(await addPEM(c.pem))) act.disabled = false; });
          }
          return h("div", { class: "listrow" }, h("div", null, h("strong", null, certName(c)), certMeta(c), h("div", { class: "tiny" }, t("ssl.expires"), ": ", expiry(c)), fp(c.id)), act);
        }),
        h("div", { class: "notice amber" }, t("ssl.verifyNote")));
    } catch (e) {
      if (!isLive()) return;
      put(chainRows);
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      ffs.show(errList(e));
    } finally { run.disabled = false; }
  });

  // paste a PEM
  const pem = h("textarea", { class: "mono", rows: 7, spellcheck: "false", autocomplete: "off", placeholder: "-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----" });
  const addBtn = h("button", { type: "submit", class: "btn primary" }, t("ssl.add"));
  const pemForm = h("form", { novalidate: true }, fields(field(t("ssl.pem"), pem, t("ssl.pemHelp"), { full: true })), h("div", { class: "actions end" }, addBtn));
  pemForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    if (!/-----BEGIN CERTIFICATE-----/.test(pem.value)) { fs.show([t("ssl.e.pem")]); return; }
    addBtn.disabled = true;
    if (await addPEM(pem.value)) pem.value = "";
    addBtn.disabled = false;
  });

  put(view, pageHead(t("nav.certificates"), sub(t("ssl.sub")), null, eb), fs.err, fs.ok,
    h("div", { class: "two" }, h("div", { class: "stack" }, listBox, h("div", { class: "notice" }, t("ssl.about"))),
      h("div", { class: "stack" }, card(t("ssl.fetchTitle"), t("ssl.fetchSub"), fetchForm), card(t("ssl.pemTitle"), t("ssl.pemSub"), pemForm))));
  await load();
}

// ---- settings router ------------------------------------------------------------------

// showSettings renders an admin screen; the caller has checked admin.
export function showSettings(view, key, p0, p1, criteria) {
  switch (key) {
    case "admin/repository/repositories":
      if (p0 === undefined) return viewRepos(view, true);
      if (p0 === "create") return viewRepoForm(view, { recipe: p1 === undefined ? DEFAULT_RECIPE : p1 });
      return viewRepoForm(view, { name: p0 });
    case "admin/repository/blobstores": return viewBlobStores(view, p0);
    case "admin/repository/selectors": return viewSelectors(view, p0);
    case "admin/repository/cleanuppolicies": return viewPolicies(view, p0);
    case "admin/repository/routingrules": return viewRoutingRules(view, p0);
    case "admin/repository/staging": return viewStaging(view);
    case "admin/security/privileges": return viewPrivileges(view);
    case "admin/security/roles": return viewRoles(view, p0);
    case "admin/security/users": return viewUsers(view, p0);
    case "admin/security/anonymous": return viewGeneral(view, "anonymous");
    case "admin/security/ldap": return viewRealmConfig(view, "ldap");
    case "admin/security/realms": return viewRealms(view);
    case "admin/security/sslcertificates": return viewCertificates(view);
    case "admin/security/oidc": return viewRealmConfig(view, "oidc");
    case "admin/security/atlassiancrowd": return viewRealmConfig(view, "crowd");
    case "admin/security/saml": return viewRealmConfig(view, "saml");
    case "admin/security/rutauth": return viewRealmConfig(view, "rutauth");
    case "admin/security/usertoken": { // every user's token, then the user token settings
      const settings = h("div", { class: "ut-settings" });
      return Promise.all([viewUserTokens(view, true, settings), viewGeneral(settings, "usertoken")]);
    }
    case "admin/security/apitokens": return viewTokens(view, true);
    case "admin/firewall/settings": return viewVulnerability(view);
    case "admin/firewall/quarantine": return viewQuarantine(view);
    case "admin/support/logs": return viewLogs(view, criteria);
    case "admin/support/logging": return viewLogging(view);
    case "admin/support/metrics": return viewMetrics(view);
    case "admin/support/supportzip": return viewSupportZip(view);
    case "admin/support/systeminformation": return viewSysInfo(view);
    case "admin/support/status": return viewStatus(view, p0);
    case "admin/system/api": return viewApi(view);
    case "admin/system/emailserver": return viewGeneral(view, "email");
    case "admin/system/http": return viewGeneral(view, "http");
    case "admin/system/nodes": return viewNodes(view);
    case "admin/system/tasks": return viewTasks(view, p0);
    case "admin/system/webhooks": return viewWebhooks(view, p0);
    case "admin/system/general": return viewGeneral(view, "general");
  }
  return null;
}
