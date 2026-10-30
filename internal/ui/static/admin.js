// Access and system screens (plan 10 Decision 8; the tasks and cleanup
// policies are in ops.js, users, roles, realms and the account page in
// access.js): API tokens, the support screens (status, logs, metrics, support
// ZIP, system information), nodes, the API view and the trust store. Admin
// screens use the routes: the list at "#admin/<group>/<screen>", a detail at
// "#admin/<group>/<screen>:<id>".
import {
  state, h, put, api, enc, encSeg, alive, filterBar, row, fmtBytes, fmtNum, fmtDate, fmtUptime, taskName,
  copyLine, copyButton, pageHead, loading, truncNote, card, kv, stat, tbl, handleError, needLogin, icon, s, markBusy,
  debounce, FORMATS, FORMAT_NAMES, typeBadge, sevBadge, sevText, osvLink, repoFlags, SEVERITIES, saveBlob, browseLink,
} from "./util.js";
import { t, locale } from "./i18n.js";
import { confirmDelete, viewRepos } from "./browse.js";
import { viewRepoForm, DEFAULT_RECIPE } from "./repoform.js";
import { viewTasks, viewPolicies, trendCard, logsCard, viewRoutingRules, viewSelectors, viewWebhooks, viewStaging } from "./ops.js";
import { viewUsers, viewRoles, viewRealms, viewRealmConfig, viewPrivileges, viewUserTokens } from "./access.js";
import { viewBlobStores, viewGeneral, viewVulnerability, viewQuarantine } from "./settings.js";
import { field, fields, sel, textIn, button, formState, errList, checks, RPLAN, REXEC } from "./ops.js";
import { showRecovery, rerender } from "./shell.js";

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

// ---- activity: the recorded events, newest first (GET /-/api/events) ---------------------
//   #admin/support/activity

async function viewActivity(view) {
  const isLive = alive();
  const evd = await api("webhooks/events").catch(() => ({})); // the kinds to filter by
  if (!isLive()) return;
  const kind = sel([["", t("act.allKinds")]].concat((evd.events || []).map((e) => [e.kind, e.kind])), "", { "aria-label": t("act.kind") });
  const refresh = button([icon("refresh"), t("logs.refresh")], "small");
  const more = button(t("search.loadMore"));
  const body = h("tbody"), box = h("div", null, loading());
  const head = h("thead", null, h("tr", null, [t("act.at"), t("act.kind"), t("act.actor"), t("col.repo"), t("act.subject"), "IP", t("act.node")].map((x) => h("th", { scope: "col" }, x))));
  const rowOf = (e) => h("tr", null,
    h("td", { class: "nowrap" }, fmtDate(e.at)), h("td", { class: "nowrap" }, h("code", null, e.kind)), h("td", null, e.actor || dash),
    h("td", { class: "nowrap" }, e.repo ? h("a", { href: browseLink(e.repo) }, e.repo) : dash),
    h("td", { class: "wrap" }, e.subject || dash, e.detail ? h("div", { class: "tiny muted" }, e.detail) : null),
    h("td", { class: "mono nowrap" }, e.ip || dash), h("td", { class: "mono nowrap" }, e.node || dash));
  let next = 0, seq = 0;
  more.hidden = true;
  const load = async (append) => {
    const n = ++seq;
    const q = new URLSearchParams({ limit: "100" });
    if (kind.value) q.set("kind", kind.value);
    if (append) q.set("before", String(next));
    more.disabled = refresh.disabled = true;
    try {
      const d = await api("events?" + q);
      if (!isLive() || n !== seq) return;
      if (!append) put(body);
      body.append(...(d.events || []).map(rowOf));
      next = d.next || 0;
      more.hidden = !next;
      put(box, body.children.length ? h("div", { class: "tablewrap" }, h("table", null, head, body)) : h("div", { class: "empty" }, t("act.none")));
    } catch (e) {
      if (!isLive() || n !== seq) return;
      put(box, h("div", { class: "empty" }, t("act.failed")));
      handleError(e);
    } finally {
      if (n === seq) more.disabled = refresh.disabled = false;
    }
  };
  kind.addEventListener("change", () => load(false));
  refresh.addEventListener("click", () => load(false));
  more.addEventListener("click", () => load(true));
  const c = card(t("act.title"), t("act.cardSub"), null);
  c.append(h("div", { class: "toolbar" }, kind, refresh), box);
  put(view, pageHead(t("nav.activity"), sub(t("act.sub"))), c, h("div", { class: "toolbar end" }, more));
  await load(false);
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
// ---- repository dashboard and monthly usage ---------------------------------------
//   #admin/repository/dashboard[:<repository>], #admin/support/usage: admin, or the
//   read privilege on "metrics" (the server answers 403 otherwise).

const dashLink = (name) => "#admin/repository/dashboard" + (name ? ":" + enc(name) : "");
const notice = (text, cls) => h("div", { class: "notice" + (cls ? " " + cls : "") }, text);
const when = (v) => (v ? fmtDate(v) : dash);
// day is a date for a narrow column; the full time is its tooltip.
const stamp = (v) => { const d = new Date(v); return !v ? dash : isNaN(d) ? String(v) : d.toLocaleString(locale(), { dateStyle: "short", timeStyle: "short", hourCycle: "h23" }); };
const day = (v) => { const d = new Date(v); return !v ? dash : isNaN(d) ? String(v) : h("span", { title: fmtDate(v) }, d.toLocaleDateString(locale())); };
function metricsDenied(view, title) {
  put(view, pageHead(title), h("section", { class: "card" }, h("p", null, t("dash.denied")), h("a", { href: "#browse/welcome" }, t("notFound.home"))));
}
// pager pages an offset/limit list; go(offset) loads another page.
function pager(d, go) {
  const prev = button(t("pg.prev"), "small"), next = button(t("pg.next"), "small");
  prev.disabled = d.offset <= 0;
  next.disabled = d.offset + d.limit >= d.total;
  prev.addEventListener("click", () => go(Math.max(0, d.offset - d.limit)));
  next.addEventListener("click", () => go(d.offset + d.limit));
  return h("div", { class: "pager" }, h("span", null, d.total ? t("pg.range", { a: fmtNum(d.offset + 1), b: fmtNum(Math.min(d.total, d.offset + d.limit)), n: fmtNum(d.total) }) : ""), prev, next);
}
const table = (heads, rows, emptyText) => h("div", { class: "tablewrap" }, rows.length
  ? h("table", null, h("thead", null, h("tr", null, heads)), h("tbody", null, rows)) : h("div", { class: "empty" }, emptyText));
// The dashboard's columns: [sort key, label key, cell class].
const DASH_COLS = [["name", "col.name"], ["format", "col.format"], ["type", "col.type"], ["components", "dash.components", "num"], ["assets", "col.files", "num"],
  ["bytes", "col.size", "num"], ["lastUpload", "dash.lastUpload"], ["lastDownload", "comp.lastDownloaded"], ["requests24h", "dash.req24h", "num"],
  ["requests30d", "dash.req30d", "num"], ["vulnerable", "dash.vulns"]];

async function viewDashboard(view, name) {
  if (name !== undefined) return viewVulnerable(view, name);
  const isLive = alive();
  const title = t("nav.repoDashboard");
  const q = { sort: "name", order: "", offset: 0 };
  const nameIn = h("input", { type: "search", placeholder: t("dash.filter"), "aria-label": t("dash.filter"), autocomplete: "off" });
  const fmt = sel([["", t("task.anyFormat")]].concat(FORMATS.map((f) => [f, FORMAT_NAMES[f]])), "", { "aria-label": t("col.format") });
  const typ = sel([["", t("dash.anyType")], ["hosted", "hosted"], ["proxy", "proxy"], ["group", "group"]], "", { "aria-label": t("col.type") });
  const vul = h("input", { type: "checkbox" });
  const box = h("div", null, loading()), note = h("div");
  let seq = 0;
  const load = async () => {
    const n = ++seq;
    const p = new URLSearchParams({ sort: q.sort, limit: "50", offset: String(q.offset) });
    if (q.order) p.set("order", q.order);
    if (fmt.value) p.set("format", fmt.value);
    if (typ.value) p.set("type", typ.value);
    if (nameIn.value.trim()) p.set("q", nameIn.value.trim());
    if (vul.checked) p.set("vulnerable", "true");
    let d;
    try {
      d = await api("dashboard/repositories?" + p);
    } catch (e) {
      if (!isLive() || n !== seq) return;
      if (e.status === 403) metricsDenied(view, title);
      else { put(box, h("div", { class: "empty" }, e.message)); if (e.status === 401) handleError(e); }
      return;
    }
    if (!isLive() || n !== seq) return;
    q.sort = d.sort;
    q.order = d.order;
    const heads = DASH_COLS.map(([key, label, cls]) => {
      const on = q.sort === key;
      const b = h("button", { type: "button", class: "sort" + (on ? " on" : "") }, t(label), on ? (q.order === "asc" ? " ↑" : " ↓") : "");
      b.addEventListener("click", () => {
        if (on) q.order = q.order === "asc" ? "desc" : "asc";
        else { q.sort = key; q.order = ""; }
        q.offset = 0;
        load();
      });
      return h("th", { scope: "col", class: cls || null, "aria-sort": on ? (q.order === "asc" ? "ascending" : "descending") : null }, b);
    });
    const rows = (d.items || []).map((r) => {
      const e = r.evaluation || {}, bs = e.bySeverity || {};
      const vulns = !e.covered ? dash : !e.lastEvaluated ? badge(t("dash.notEvaluated"))
        : e.vulnerable || e.malicious ? h("a", { href: dashLink(r.name) }, repoFlags({ vulnerabilities: { malicious: e.malicious, vulnerable: e.vulnerable, critical: bs.critical, high: bs.high, medium: bs.medium, low: bs.low } }))
          : badge(t("dash.clean"), "green");
      return h("tr", null,
        h("td", { class: "name" }, h("a", { href: browseLink(r.name) }, h("strong", null, r.name)), r.online ? null : h("div", { class: "tiny muted" }, t("repo.down"))),
        h("td", null, FORMAT_NAMES[r.format] || r.format), h("td", null, typeBadge(r.type)),
        h("td", { class: "num", title: r.componentsCounted ? t("dash.counted", { time: fmtDate(r.componentsCounted) }) : null }, fmtNum(r.components), r.componentsCurrent ? null : h("span", { class: "muted" }, " *")),
        h("td", { class: "num" }, fmtNum(r.assets)),
        h("td", { class: "num" }, fmtBytes(r.bytes), r.uniqueBytes !== r.bytes ? h("div", { class: "tiny muted" }, t("dash.unique", { n: fmtBytes(r.uniqueBytes) })) : null),
        h("td", { class: "nowrap" }, day(r.lastUpload)), h("td", { class: "nowrap" }, day(r.lastDownload)),
        h("td", { class: "num" }, fmtNum(r.requests24h)), h("td", { class: "num" }, fmtNum(r.requests30d)),
        h("td", { class: "nowrap" }, vulns, e.covered && !e.monitored ? h("div", { class: "tiny muted" }, t("dash.notMonitored")) : null));
    });
    put(box, table(heads, rows, t("dash.none")), pager(d, (o) => { q.offset = o; load(); }));
    const unc = d.uncoveredFormats || [];
    put(note, notice([t("dash.note"), unc.length ? " " + t("dash.uncovered", { list: unc.map((f) => FORMAT_NAMES[f] || f).join(", ") }) : null]));
  };
  const reload = () => { q.offset = 0; load(); };
  const typed = debounce(reload, 300);
  nameIn.addEventListener("input", typed);
  state.leaving.push(typed.cancel);
  for (const el of [fmt, typ, vul]) el.addEventListener("change", reload);
  const c = card(title, t("dash.cardSub"), null);
  c.append(h("div", { class: "toolbar dash-tools" }, nameIn, fmt, typ, h("label", { class: "check" }, vul, t("dash.vulnOnly"))), box);
  put(view, pageHead(title, sub(t("dash.sub")), acts(h("a", { class: "btn", href: "#admin/support/usage" }, icon("chart"), t("nav.usage")))), c, note);
  await load();
}

// viewVulnerable lists one repository's vulnerable components from its last evaluation.
async function viewVulnerable(view, name) {
  const isLive = alive();
  const back = h("a", { class: "btn", href: dashLink() }, t("dash.back"));
  const sev = sel([["", t("dash.sevAny")]].concat(SEVERITIES.slice().reverse().map((x) => [x, t("rf.fwSevAtLeast", { sev: sevText(x) })])), "", { "aria-label": t("vul.severity") });
  const head = h("div"), box = h("div", null, loading());
  let offset = 0, seq = 0;
  const load = async () => {
    const n = ++seq;
    const p = new URLSearchParams({ limit: "50", offset: String(offset) });
    if (sev.value) p.set("severity", sev.value);
    let d;
    try {
      d = await api("dashboard/repositories/" + encSeg(name) + "/vulnerabilities?" + p);
    } catch (e) {
      if (!isLive() || n !== seq) return;
      if (e.status === 403) metricsDenied(view, t("nav.repoDashboard"));
      else if (e.status === 404) put(view, pageHead(name, null, acts(back)), notFound(t("dash.notFound", { name })));
      else { put(box, h("div", { class: "empty" }, e.message)); if (e.status === 401) handleError(e); }
      return;
    }
    if (!isLive() || n !== seq) return;
    put(head, h("p", null, typeBadge(d.type), " ", FORMAT_NAMES[d.format] || d.format, " · ",
      d.monitored ? t("dash.monitored") : t("dash.notMonitored"), " · ", t("dash.evaluated", { time: when(d.lastEvaluated) })));
    const rows = (d.items || []).map((x) => h("tr", null,
      h("td", { class: "name" }, h("strong", null, x.component),
        h("div", { class: "tiny muted mono" }, x.ecosystem, x.package && !x.component.startsWith(x.package) ? " · " + x.package : null)),
      h("td", { class: "nowrap" }, x.malicious ? [badge(t("vul.malicious"), "red"), " "] : null, sevBadge(x.severity), x.unchecked ? [" ", badge(t("rf.vulnUnchecked"))] : null),
      h("td", null, (Array.isArray(x.advisories) ? x.advisories : []).map((a) => h("div", { class: "adv" },
        osvLink(a.id), " ", sevBadge(a.severity), a.malicious ? [" ", badge(t("vul.malicious"), "red")] : null,
        (a.fixedIn || []).length ? h("span", { class: "tiny muted" }, " " + t("dash.fixedIn", { v: a.fixedIn.join(", ") })) : null,
        a.summary ? h("div", { class: "tiny muted" }, a.summary) : null))),
      h("td", { class: "nowrap" }, day(x.evaluated))));
    put(box, table([h("th", { scope: "col" }, t("comp.component")), h("th", { scope: "col" }, t("vul.severity")), h("th", { scope: "col" }, t("dash.advisories")),
      h("th", { scope: "col" }, t("dash.evaluatedCol"))], rows, d.lastEvaluated ? t("vul.none") : t("dash.neverEvaluated")), pager(d, (o) => { offset = o; load(); }));
  };
  sev.addEventListener("change", () => { offset = 0; load(); });
  const c = card(t("dash.vulnTitle"), t("dash.vulnSub"), null);
  c.append(h("div", { class: "toolbar" }, sev), box);
  put(view, pageHead(name, head, acts(back)), c);
  await load();
}

// barChart draws one monthly figure as bars, the current month in the accent; the
// table below holds every number.
function barChart(months, key, label) {
  const W = 620, H = 135, TOP = 12;
  const vals = months.map((m) => Math.max(0, Number(m[key]) || 0));
  const max = Math.max(1, ...vals);
  const show = key === "requests" ? fmtNum : fmtBytes;
  const n = months.length, slot = W / Math.max(1, n), bw = Math.max(2, Math.min(36, slot - 6));
  const y = (v) => H - (v / max) * (H - TOP);
  const svg = s("svg", { viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img", "aria-label": t("use.chartAria", { what: label, max: show(Math.max(0, ...vals)) }) });
  svg.append(s("path", { class: "c-grid", d: [max, max / 2].map((v) => "M0 " + y(v).toFixed(1) + "H" + W).join("") + "M0 " + (H - 0.5) + "H" + W }));
  months.forEach((m, i) => {
    const g = s("g");
    const tip = s("title");
    tip.textContent = m.month + " · " + (m[key] === null ? dash : show(vals[i])) + (m.current ? " (" + t("use.current") + ")" : "");
    g.append(tip, s("rect", { class: "c-hit", x: (i * slot).toFixed(1), y: 0, width: slot.toFixed(1), height: H }));
    if (vals[i]) g.append(s("rect", { class: "c-bar" + (m.current ? " cur" : ""), x: (i * slot + (slot - bw) / 2).toFixed(1), y: y(vals[i]).toFixed(1), width: bw.toFixed(1), height: Math.max(1, H - y(vals[i])).toFixed(1) }));
    svg.append(g);
  });
  const yl = h("div", { class: "chart-y", "aria-hidden": "true" });
  for (const v of [max, max / 2]) {
    const sp = h("span", null, show(Math.round(v)));
    sp.style.top = (y(v) / H * 100).toFixed(1) + "%";
    yl.append(sp);
  }
  const xl = h("div", { class: "chart-labels", "aria-hidden": "true" });
  const every = Math.max(1, Math.ceil(n / 8));
  months.forEach((m, i) => {
    if (i % every && i !== n - 1) return;
    if (i !== n - 1 && n - 1 - i < every) return;
    const sp = h("span", null, m.month);
    sp.style.left = (((i + 0.5) * slot) / W * 100).toFixed(2) + "%";
    xl.append(sp);
  });
  return h("div", { class: "chart-box" }, h("div", { class: "chart-key" }, h("span", null, h("i"), label),
    h("span", { class: "end" }, t("use.peak", { v: show(Math.max(0, ...vals)) }))), h("div", { class: "chart" }, svg, yl), xl);
}

async function viewUsage(view) {
  const isLive = alive();
  const title = t("nav.usage");
  const from = h("input", { type: "month", "aria-label": t("use.from"), placeholder: "YYYY-MM" });
  const to = h("input", { type: "month", "aria-label": t("use.to"), placeholder: "YYYY-MM" });
  const go = button(t("use.show"), "small");
  const metric = sel([["requests", t("use.requests")], ["egressBytes", t("use.egress")], ["storageBytes", t("use.storage")]], "requests", { "aria-label": t("use.metric") });
  const chartBox = h("div", null, loading()), tableBox = h("div"), foot = h("p", { class: "hint" });
  let d = null;
  const draw = () => put(chartBox, barChart(d.months || [], metric.value, metric.selectedOptions[0].textContent));
  const num = (v, show) => (v === null || v === undefined ? dash : (show || fmtNum)(v));
  const load = async () => {
    const p = new URLSearchParams();
    if (from.value) p.set("from", from.value);
    if (to.value) p.set("to", to.value);
    go.disabled = true;
    try {
      d = await api("metrics/monthly" + (p.toString() ? "?" + p : ""));
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 403) metricsDenied(view, title);
      else { put(chartBox, notice(e.message, "amber")); put(tableBox); if (e.status === 401) handleError(e); }
      return;
    } finally { go.disabled = false; }
    if (!isLive()) return;
    from.value = d.from;
    to.value = d.to;
    draw();
    const rows = (d.months || []).slice().reverse().map((m) => h("tr", null,
      h("td", { class: "nowrap mono" }, m.month, m.current ? [" ", badge(t("use.current"), "blue")] : null),
      h("td", { class: "num" }, fmtNum(m.requests)), h("td", { class: "num" }, fmtNum(m.errors)),
      h("td", { class: "num" }, fmtNum(m.downloads)), h("td", { class: "num" }, fmtNum(m.uploads)),
      h("td", { class: "num" }, fmtBytes(m.egressBytes)), h("td", { class: "num" }, fmtBytes(m.ingressBytes)),
      h("td", { class: "num" }, num(m.storageBytes, fmtBytes), m.peakStorageBytes ? h("div", { class: "tiny muted" }, t("use.peakShort", { v: fmtBytes(m.peakStorageBytes) })) : null),
      h("td", { class: "num" }, num(m.components), m.peakComponents ? h("div", { class: "tiny muted" }, t("use.peakShort", { v: fmtNum(m.peakComponents) })) : null),
      h("td", { class: "num" }, fmtNum(m.uniqueUsers)), h("td", { class: "num" }, fmtNum(m.uniqueAddresses))));
    const col = (k, cls) => h("th", { scope: "col", class: cls || null }, t(k));
    put(tableBox, table([col("use.month"), col("use.requests", "num"), col("use.errors", "num"), col("use.downloads", "num"), col("use.uploads", "num"),
      col("use.egress", "num"), col("use.ingress", "num"), col("use.storage", "num"), col("dash.components", "num"), col("use.users", "num"), col("use.addresses", "num")], rows, t("common.noItems")));
    foot.textContent = t("use.foot", { n: d.retentionMonths, at: fmtDate(d.collectedAt) });
  };
  go.addEventListener("click", load);
  metric.addEventListener("change", () => { if (d) draw(); });
  const tc = card(t("use.tableTitle"), t("use.tableSub"), null);
  tc.append(tableBox);
  put(view, pageHead(title, sub(t("use.sub")), acts(h("a", { class: "btn", href: dashLink() }, icon("dashboard"), t("nav.repoDashboard")))),
    h("div", { class: "toolbar" }, h("label", { class: "check" }, t("use.from"), from), h("label", { class: "check" }, t("use.to"), to), go),
    card(t("use.chartTitle"), t("use.chartSub"), chartBox, metric), tc, foot);
  await load();
}

// ---- recovery mode and reconcile plans ------------------------------------------------
//   #admin/system/recovery[:<plan id>]

const recLink = (id) => "#admin/system/recovery" + (id ? ":" + enc(id) : "");
const PLAN_STATE = { PLANNED: ["rec.planned", "blue"], EXECUTED: ["rec.executed", "green"], FAILED: ["rec.failed", "red"] };
const planBadge = (st) => (PLAN_STATE[st] ? badge(t(PLAN_STATE[st][0]), PLAN_STATE[st][1]) : badge(st));
const ITEM_CLS = { ADD: "blue", DELETE: "amber", CONFLICT: "red", DONE: "green", FAILED: "red" };
const ui = { [RPLAN]: "Reconcile plan (UI)", [REXEC]: "Reconcile execute (UI)" }; // the tasks these screens run
const sleep = (ms) => new Promise((res) => setTimeout(res, ms));

// runTask runs the manual task of type named ui[type] (made on first use, its
// params replaced after) and resolves with it once the run has ended (null when
// the screen was left or it ran for 10 minutes); started(task) is told first.
async function runTask(type, params, isLive, started) {
  const def = { name: ui[type], schedule: "manual", params, enabled: true };
  const old = ((await api("tasks")).tasks || []).find((x) => x.type === type && x.name === def.name);
  const tk = old ? await api("tasks/" + encSeg(old.id), { method: "PUT", body: def }) : await api("tasks", { method: "POST", body: Object.assign({ type }, def) });
  await api("tasks/" + encSeg(tk.id) + "/run", { method: "POST" });
  started(tk);
  for (let i = 0; i < 600 && isLive(); i++) {
    await sleep(1000);
    if (!isLive()) break;
    const x = await api("tasks/" + encSeg(tk.id));
    if (x.status !== "Running" && x.lastRun && x.lastRun !== tk.lastRun) return x;
  }
  return null;
}
// taskOut shows a run that runTask follows in el.
function taskOut(el, isLive) {
  return {
    started: (tk) => put(el, badge(t("task.running"), "blue"), " ", h("a", { href: "#admin/system/tasks:" + enc(tk.id) }, t("rec.taskLink"))),
    done(x) {
      if (!isLive()) return;
      if (x) put(el, badge(x.lastResult === "OK" ? t("task.ok") : t("task.failed"), x.lastResult === "OK" ? "green" : "red"), " ", x.lastMessage || "", " ",
        h("a", { href: "#admin/system/tasks:" + enc(x.id) }, t("rec.taskLink")));
    },
    failed(e) {
      if (!isLive()) return;
      put(el, notice(e.status === 409 && e.data && (e.data.runningName || e.data.running) ? t("task.conflict", { name: e.data.runningName || e.data.running }) : errList(e).join(" "), "amber"));
      if (e.status === 401 || e.status === 403) handleError(e);
    },
  };
}
const planCounts = (sm) => [sm.add, sm.delete, sm.skip, sm.conflict].map((n) => h("td", { class: "num" }, fmtNum(n || 0)));

async function viewRecovery(view, id) {
  if (id !== undefined) return viewPlan(view, id);
  const isLive = alive();
  const [rec, bs, plans] = await Promise.all([api("recovery"), api("blobstores").catch(() => null), api("reconcile/plans?limit=20")]);
  if (!isLive()) return;
  showRecovery(rec);
  const j = rec.journal;

  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const on = h("input", { type: "checkbox", checked: !!rec.enabled });
  const msg = textIn(rec.message, { maxlength: 500, placeholder: t("rec.msgPh") });
  const days = h("input", { type: "number", inputmode: "numeric", min: 1, max: 3650, step: 1, value: String(rec.journalRetentionDays) });
  const save = h("button", { type: "submit", class: "btn primary" }, t("gen.save"));
  put(form, fs.err, fields(
    h("div", { class: "field full" }, h("label", { class: "check" }, on, t("rec.enable")), h("small", null, t("rec.enableHelp"))),
    field(t("rec.msg"), msg, t("rec.msgHelp"), { full: true }),
    field(t("rec.days"), days, t("rec.daysHelp"))), h("div", { class: "actions end" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const b = { enabled: on.checked, message: msg.value.trim(), journalRetentionDays: Number(days.value) };
    if (!(Number.isInteger(b.journalRetentionDays) && b.journalRetentionDays >= 1 && b.journalRetentionDays <= 3650)) { fs.show([t("rec.e.days")]); return; }
    if (b.enabled && !rec.enabled && !(await confirmDelete(t("rec.enableText"), t("rec.enableTitle"), t("rec.enableBtn")))) return;
    save.disabled = true;
    try {
      const d = await api("recovery", { method: "PUT", body: b });
      showRecovery(d);
      if (isLive()) fs.done(recLink(), t("gen.saved"));
    } catch (e) {
      save.disabled = false;
      if (isLive()) { if (e.status === 401 || e.status === 403) handleError(e); else fs.show(errList(e)); }
    }
  });

  // a new plan: the blob store and the journal window
  const store = sel([["", t("task.allStores")]].concat(((bs && bs.blobstores) || []).filter((x) => x.type !== "group").map((x) => [x.name, x.name])), "", null);
  const win = sel([["hours", t("rec.winHours")], ["since", t("rec.winSince")], ["all", t("rec.winAll")]], "hours");
  const hours = h("input", { type: "number", inputmode: "numeric", min: 1, step: 1, value: "24" });
  const since = h("input", { type: "datetime-local" });
  const fHours = field(t("task.p.sinceHours"), hours, t("rec.hoursHelp")), fSince = field(t("rec.since"), since, t("rec.sinceHelp"));
  const syncWin = () => { fHours.hidden = win.value !== "hours"; fSince.hidden = win.value !== "since"; };
  win.addEventListener("change", syncWin);
  syncWin();
  const make = button(t("rec.makePlan"), "primary");
  const out = h("div", { class: "rec-out", "aria-live": "polite" });
  make.addEventListener("click", async () => {
    const p = {};
    if (store.value) p.blobStore = store.value;
    if (win.value === "hours") {
      p.sinceHours = Number(hours.value);
      if (!(Number.isInteger(p.sinceHours) && p.sinceHours >= 1)) { put(out, notice(t("rec.e.hours"), "amber")); return; }
    } else if (win.value === "since") {
      const at = new Date(since.value);
      if (!since.value || isNaN(at)) { put(out, notice(t("rec.e.since"), "amber")); return; }
      p.since = at.toISOString().replace(/\.\d+Z$/, "Z");
    } else p.all = true;
    make.disabled = true;
    const o = taskOut(out, isLive);
    try {
      o.done(await runTask(RPLAN, p, isLive, o.started));
      if (isLive()) loadPlans();
    } catch (e) { o.failed(e); } finally { make.disabled = false; }
  });

  // the plans, newest first, a page at a time
  const plansBox = h("div");
  let token = "";
  const planRows = h("tbody");
  const more = button(t("search.loadMore"), "small");
  const addPlans = (d) => {
    for (const p of d.items || []) {
      const sm = p.summary || {};
      planRows.append(h("tr", null, h("td", { class: "nowrap" }, h("a", { class: "mono", href: recLink(p.id) }, h("strong", null, p.id))),
        h("td", null, p.blobStore || t("rec.allStoresShort")), h("td", null, planBadge(p.state)),
        h("td", { class: "nowrap" }, p.since ? stamp(p.since) : t("rec.wholeJournal")),
        h("td", { class: "nowrap" }, stamp(p.created), h("div", { class: "tiny muted" }, p.createdBy)), planCounts(sm)));
    }
    token = d.continuationToken || "";
    more.hidden = !token;
  };
  const loadPlans = async () => {
    planRows.replaceChildren();
    try { addPlans(await api("reconcile/plans?limit=20")); } catch (e) { handleError(e); }
    put(plansBox, planRows.children.length ? plansTable() : h("div", { class: "empty" }, t("rec.noPlans")));
  };
  const col = (k, cls) => h("th", { scope: "col", class: cls || null }, t(k));
  const plansTable = () => [h("div", { class: "tablewrap" }, h("table", null, h("thead", null, h("tr", null, col("rec.plan"), col("task.p.blobStore"), col("col.status"),
    col("rec.window"), col("rec.created"), col("rec.add", "num"), col("rec.delete", "num"), col("rec.skip", "num"), col("rec.conflict", "num"))), planRows)), h("div", { class: "pager" }, more)];
  more.addEventListener("click", async () => {
    more.disabled = true;
    try { addPlans(await api("reconcile/plans?limit=20&continuationToken=" + enc(token))); } catch (e) { handleError(e); } finally { more.disabled = false; }
  });
  addPlans(plans);
  put(plansBox, planRows.children.length ? plansTable() : h("div", { class: "empty" }, t("rec.noPlans")));
  const pc = card(t("rec.plans"), t("rec.plansSub"), null);
  pc.append(plansBox);

  put(view, pageHead(t("nav.recovery"), sub(t("rec.sub"))), fs.ok,
    h("div", { class: "two" },
      card(t("rec.mode"), t("rec.modeSub"), form),
      h("div", { class: "stack" },
        card(t("rec.state"), t("rec.stateSub"), [
          kv(t("col.status"), rec.enabled ? badge(t("rec.isOn"), "red") : badge(t("rec.isOff"))),
          rec.enabled ? [kv(t("rec.sinceAt"), fmtDate(rec.since)), kv(t("rec.by"), rec.by)] : null,
          j ? [kv(t("rec.written"), fmtNum(j.written)), kv(t("rec.writeFailed"), j.failed ? badge(fmtNum(j.failed), "red") : "0"), kv(t("rec.queued"), fmtNum(j.queued))] : h("p", { class: "hint" }, t("rec.noJournal"))]),
        card(t("rec.newPlan"), t("rec.newPlanSub"), [fields(field(t("task.p.blobStore"), store, t("task.ph.blobStore")), field(t("rec.window"), win), fHours, fSince),
          rec.enabled ? null : notice(t("rec.offNote"), "amber"), h("div", { class: "actions" }, make), out]))), pc);
}

async function viewPlan(view, id) {
  const isLive = alive();
  const back = h("a", { class: "btn", href: recLink() }, t("rec.back"));
  let p;
  try {
    p = await api("reconcile/plans/" + encSeg(id));
  } catch (e) {
    if (e.status !== 404) throw e;
    if (isLive()) put(view, pageHead(t("nav.recovery"), null, acts(back)), notFound(t("rec.notFound", { id })));
    return;
  }
  const rec = await api("recovery").catch(() => null);
  if (!isLive()) return;
  const sm = p.summary || {};
  const out = h("div", { class: "rec-out", "aria-live": "polite" });
  const exec = button(t("rec.execute"), "primary");
  exec.disabled = p.state === "EXECUTED" || !(sm.add || sm.delete);
  exec.addEventListener("click", async () => {
    if (!(await confirmDelete(t("rec.execText", { add: fmtNum(sm.add || 0), del: fmtNum(sm.delete || 0) }), t("rec.execTitle"), t("rec.execute")))) return;
    exec.disabled = true;
    const o = taskOut(out, isLive);
    try {
      const x = await runTask(REXEC, { planId: p.id }, isLive, o.started);
      if (isLive() && x) { o.done(x); setTimeout(() => { if (isLive()) rerender(); }, 1500); }
    } catch (e) { o.failed(e); exec.disabled = false; }
  });
  const del = button(t("common.delete"), "danger");
  del.addEventListener("click", async () => {
    if (!(await confirmDelete(t("rec.delText", { id: p.id }), t("rec.delTitle")))) return;
    del.disabled = true;
    try {
      await api("reconcile/plans/" + encSeg(p.id), { method: "DELETE" });
      if (isLive()) location.hash = recLink();
    } catch (e) { del.disabled = false; handleError(e); }
  });

  // items: by action and state, a page at a time
  const act = sel([["", t("rec.anyAction")]].concat(["ADD", "DELETE", "SKIP", "CONFLICT"].map((x) => [x, x])), "", { "aria-label": t("rec.action") });
  const st = sel([["", t("rec.anyState")]].concat(["PLANNED", "DONE", "FAILED", "SKIPPED"].map((x) => [x, x])), "", { "aria-label": t("col.status") });
  const body = h("tbody"), box = h("div");
  const more = button(t("search.loadMore"), "small");
  let token = "", seq = 0;
  const col = (k, cls) => h("th", { scope: "col", class: cls || null }, t(k));
  const load = async (next) => {
    const n = ++seq;
    const q = new URLSearchParams({ limit: "100" });
    if (act.value) q.set("action", act.value);
    if (st.value) q.set("state", st.value);
    if (next) q.set("continuationToken", token);
    let d;
    try { d = await api("reconcile/plans/" + encSeg(p.id) + "/items?" + q); } catch (e) { if (isLive()) handleError(e); return; }
    if (!isLive() || n !== seq) return;
    if (!next) body.replaceChildren();
    for (const x of d.items || []) {
      body.append(h("tr", null, h("td", { class: "num" }, String(x.seq)), h("td", { class: "nowrap" }, x.repository), h("td", null, x.kind),
        h("td", { class: "name mono tiny" }, x.path, x.blobId ? h("div", { class: "muted" }, x.blobId.slice(0, 12)) : null),
        h("td", null, badge(x.action, ITEM_CLS[x.action])), h("td", null, badge(x.state, ITEM_CLS[x.state])),
        h("td", { class: "tiny" }, x.reason || dash), h("td", { class: "nowrap" }, stamp(x.at))));
    }
    token = d.continuationToken || "";
    more.hidden = !token;
    put(box, body.children.length ? [h("div", { class: "tablewrap" }, h("table", null, h("thead", null, h("tr", null, col("rec.seq", "num"), col("col.repo"),
      col("rec.kind"), col("rec.path"), col("rec.action"), col("col.status"), col("rec.reason"), col("rec.at"))), body)), h("div", { class: "pager" }, more)]
      : h("div", { class: "empty" }, t("common.noItems")));
  };
  for (const el of [act, st]) el.addEventListener("change", () => load(false));
  more.addEventListener("click", () => load(true));
  const tile = (k, v, foot) => stat({ label: t(k), value: fmtNum(v || 0), foot });
  const ic = card(t("rec.items"), t("rec.itemsSub"), null);
  ic.append(h("div", { class: "toolbar" }, act, st), box);
  put(view, pageHead(t("rec.planTitle", { id: p.id }), h("p", null, planBadge(p.state), " ", t("rec.planSub")), acts(back, del, exec)),
    rec && !rec.enabled && p.state !== "EXECUTED" ? notice([t("rec.offNote"), " ", h("a", { href: recLink() }, t("rec.mode"))], "amber") : null,
    out,
    h("div", { class: "stats" }, tile("rec.add", sm.add), tile("rec.delete", sm.delete), tile("rec.skip", sm.skip), tile("rec.conflict", sm.conflict, t("rec.unchanged", { n: fmtNum(sm.unchanged || 0) }))),
    sm.executions ? h("div", { class: "stats" }, tile("rec.done", sm.done), tile("rec.failedItems", sm.failed), tile("rec.pending", sm.pending), tile("rec.executions", sm.executions)) : null,
    h("div", { class: "stack" }, card(t("rec.details"), null, [
      kv(t("task.p.blobStore"), (sm.stores || []).join(", ") || p.blobStore || t("rec.allStoresShort")),
      kv(t("rec.window"), p.since ? t("rec.from", { time: fmtDate(p.since) }) : t("rec.wholeJournal")),
      kv(t("rec.created"), fmtDate(p.created) + " · " + p.createdBy), kv(t("rec.executedAt"), when(p.executed)),
      kv(t("rec.records"), fmtNum(sm.records || 0) + (sm.skippedRecords ? " (" + t("rec.undecoded", { n: fmtNum(sm.skippedRecords) }) + ")" : "")),
      kv(t("rec.rows"), fmtNum(sm.rows || 0)),
      (sm.rebuilt || []).length ? kv(t("rec.rebuilt"), sm.rebuilt.join(", ")) : null]), ic));
  await load(false);
}

// ---- configuration as code ------------------------------------------------------------
//   #admin/system/configuration: export the document, then a dry run and an apply

const CONF_SECTIONS = ["trustStore", "blobStores", "routingRules", "contentSelectors", "cleanupPolicies", "repositories", "roles", "users", "settings", "smtp",
  "ldap", "oidc", "saml", "crowd", "rutauth", "realms", "tasks", "webhooks", "vulnerability", "logging"];
// sectionPicker selects the document's sections; set(q) adds include= or exclude=, the shorter.
function sectionPicker() {
  const c = checks(CONF_SECTIONS.map((x) => [x, x]), CONF_SECTIONS);
  return {
    el: h("div", { class: "field full", role: "group" }, h("span", { class: "flabel" }, t("cc.sections")), c.el, h("small", null, t("cc.sectionsHelp"))),
    none: () => !c.values().length,
    set(q) {
      const on = c.values(), off = CONF_SECTIONS.filter((x) => !on.includes(x));
      if (off.length) q.set(on.length <= off.length ? "include" : "exclude", (on.length <= off.length ? on : off).join(","));
    },
  };
}
const CC_A = "cc.a.", CC_S = "cc.s."; // + the step's action or status
const CC_ACTION = { create: "blue", update: "amber", delete: "red" };
const CC_STATUS = { applied: "green", failed: "red", rolledBack: "amber", rollbackFailed: "red" };
const ccValue = (v) => { if (v === undefined) return dash; const x = JSON.stringify(v); return x.length > 140 ? x.slice(0, 137) + "…" : x; };

// confResult shows a dry run's plan or an apply's step statuses.
function confResult(d) {
  const sm = d.summary || {};
  const all = h("input", { type: "checkbox" });
  const rows = (d.steps || []).map((st) => {
    const tr = h("tr", null, h("td", { class: "mono" }, st.section), h("td", { class: "name mono" }, st.key || dash),
      h("td", null, badge(t(CC_A + st.action), CC_ACTION[st.action])),
      h("td", { class: "cc-changes" }, (st.changes || []).map((c) => h("div", { class: "cc-change" }, h("code", null, c.field), " ",
        c.secret ? badge(t("cc.secretChanged"), "amber") : [h("span", { class: "muted" }, ccValue(c.from)), " → ", ccValue(c.to)])),
        (st.warnings || []).map((w) => h("div", { class: "tiny warn" }, w)),
        st.error || (st.errors || []).length ? h("div", { class: "tiny err" }, [st.error].concat(st.errors || []).filter(Boolean).join(" · ")) : null),
      d.dryRun ? null : h("td", null, st.status ? badge(t(CC_S + st.status), CC_STATUS[st.status]) : dash));
    tr.hidden = st.action === "unchanged";
    return { tr, st };
  });
  const col = (k) => h("th", { scope: "col" }, t(k));
  const changed = rows.some((r) => r.st.action !== "unchanged");
  const nothing = h("p", { class: "hint", hidden: changed }, t("cc.nothing"));
  const tw = h("div", { class: "tablewrap", hidden: !changed }, h("table", null, h("thead", null, h("tr", null, col("cc.section"), col("cc.key"), col("cc.action"), col("cc.changes"), d.dryRun ? null : col("col.status"))),
    h("tbody", null, rows.map((r) => r.tr))));
  all.addEventListener("change", () => {
    for (const r of rows) r.tr.hidden = !all.checked && r.st.action === "unchanged";
    tw.hidden = !changed && !all.checked;
    nothing.hidden = changed || all.checked;
  });
  return [
    h("div", { class: "cc-summary" }, ["create", "update", "delete", "unchanged"].map((a) => h("span", null, badge(t(CC_A + a), CC_ACTION[a]), " ", fmtNum(sm[a] || 0))),
      h("label", { class: "check end" }, all, t("cc.showUnchanged"))),
    d.error ? notice([h("strong", null, t("cc.failedAt")), " ", d.error, " ", d.rolledBack ? t("cc.rolledBack") : t("cc.notRolledBack")], "amber") : null,
    (d.warnings || []).map((w) => notice(w, "amber")),
    nothing, tw,
  ];
}

async function viewConfiguration(view) {
  const isLive = alive();
  // export
  const ef = h("form", { novalidate: true });
  const efs = formState(ef);
  const format = sel([["yaml", "YAML"], ["json", "JSON"]], "yaml");
  const secrets = sel([["omit", t("cc.secOmit")], ["encrypted", t("cc.secEnc")]], "omit");
  const ePass = h("input", { type: "password", autocomplete: "new-password", minlength: 12 });
  const fPass = field(t("cc.passphrase"), ePass, t("cc.passHelp"));
  const pw = h("input", { type: "checkbox" });
  const eSec = sectionPicker();
  const dl = h("button", { type: "submit", class: "btn primary" }, icon("file"), t("cc.download"));
  secrets.addEventListener("change", () => { fPass.hidden = secrets.value !== "encrypted"; if (fPass.hidden) ePass.value = ""; });
  fPass.hidden = true;
  put(ef, efs.err, fields(field(t("cc.format"), format), field(t("cc.secrets"), secrets, t("cc.secretsHelp")), fPass,
    h("div", { class: "field full" }, h("label", { class: "check" }, pw, t("cc.passwords")), h("small", null, t("cc.passwordsHelp"))), eSec.el),
    h("div", { class: "actions end" }, dl));
  ef.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    efs.clear();
    const q = new URLSearchParams({ format: format.value, secrets: secrets.value });
    if (pw.checked) q.set("passwords", "true");
    eSec.set(q);
    const pass = ePass.value;
    if (eSec.none()) { efs.show([t("cc.e.sections")]); return; }
    if (secrets.value === "encrypted" && [...pass].length < 12) { efs.show([t("cc.e.pass")]); return; }
    ePass.value = "";
    dl.disabled = true;
    try {
      const blob = await api("configuration?" + q, { headers: pass ? { "X-Config-Passphrase": pass } : undefined, blob: true });
      if (!isLive()) return;
      saveBlob(blob, "commandcenter-config." + format.value);
      efs.clean();
    } catch (e) {
      if (isLive()) { if (e.status === 401 || e.status === 403) handleError(e); else efs.show(errList(e)); }
    } finally { dl.disabled = false; }
  });

  // apply: a dry run first, then the apply of that same input
  const af = h("form", { novalidate: true });
  const afs = formState(af);
  const doc = h("textarea", { class: "mono cc-doc", rows: 12, spellcheck: "false", autocomplete: "off", placeholder: "version: 1\nrepositories:\n  - name: maven-releases\n    …" });
  const file = h("input", { type: "file", accept: ".yaml,.yml,.json,application/json,application/yaml,text/yaml" });
  file.addEventListener("change", async () => {
    const f = file.files && file.files[0];
    if (!f) return;
    doc.value = await f.text();
    file.value = "";
    doc.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const aPass = h("input", { type: "password", autocomplete: "off" });
  const prune = h("input", { type: "checkbox" });
  const aSec = sectionPicker();
  const dry = h("button", { type: "submit", class: "btn primary" }, t("cc.dryRun"));
  const apply = button(t("cc.apply"), "danger");
  apply.hidden = true;
  const result = h("div", { "aria-live": "polite" });
  put(af, afs.err, fields(field(t("cc.doc"), doc, t("cc.docHelp"), { full: true }),
    h("div", { class: "field full" }, h("span", { class: "flabel" }, t("cc.file")), file),
    field(t("cc.passphrase"), aPass, t("cc.applyPassHelp")),
    h("div", { class: "field" }, h("span", { class: "flabel" }, t("cc.options")), h("label", { class: "check" }, prune, t("cc.prune")), h("small", null, t("cc.pruneHelp"))),
    aSec.el), h("div", { class: "actions end" }, dry, apply));
  // Any edit after the dry run asks for a new one before Apply.
  const stale = () => { apply.hidden = true; };
  af.addEventListener("input", stale);
  af.addEventListener("change", stale);
  const send = async (dryRun) => {
    afs.clear();
    if (!doc.value.trim()) { afs.show([t("cc.e.doc")]); return; }
    if (aSec.none()) { afs.show([t("cc.e.sections")]); return; }
    const q = new URLSearchParams();
    if (dryRun) q.set("dryRun", "true");
    if (prune.checked) q.set("prune", "true");
    aSec.set(q);
    dry.disabled = apply.disabled = true;
    let d;
    try {
      d = await api("configuration" + (q.toString() ? "?" + q : ""), { method: "POST", raw: doc.value, headers: aPass.value ? { "X-Config-Passphrase": aPass.value } : undefined });
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      if (e.status === 409 && e.data && Array.isArray(e.data.steps)) d = e.data; // a step failed: the statuses say what was undone
      else { apply.hidden = true; put(result); afs.show(errList(e), t("cc.invalid")); return; }
    } finally { dry.disabled = apply.disabled = false; }
    if (!isLive()) return;
    put(result, card(dryRun ? t("cc.planTitle") : t("cc.resultTitle"), dryRun ? t("cc.planSub") : d.ok ? t("cc.appliedSub") : t("cc.failedSub"), confResult(d)));
    apply.hidden = !dryRun || !d.ok || !(d.steps || []).some((x) => x.action !== "unchanged");
    if (!dryRun) {
      aPass.value = "";
      afs.clean();
      if (d.ok) { put(afs.ok, t("cc.applied")); afs.ok.hidden = false; }
    }
    result.scrollIntoView({ block: "nearest" });
  };
  af.addEventListener("submit", (ev) => { ev.preventDefault(); send(true); });
  apply.addEventListener("click", async () => {
    if (await confirmDelete(t("cc.applyText"), t("cc.applyTitle"), t("cc.apply"))) send(false);
  });

  put(view, pageHead(t("nav.confcode"), sub(t("cc.sub"))), afs.ok,
    h("div", { class: "two" }, card(t("cc.applyCard"), t("cc.applySub"), af), card(t("cc.exportCard"), t("cc.exportSub"), ef)),
    result);
}

export function showSettings(view, key, p0, p1, criteria) {
  switch (key) {
    case "admin/repository/repositories":
      if (p0 === undefined) return viewRepos(view, true);
      if (p0 === "create") return viewRepoForm(view, { recipe: p1 === undefined ? DEFAULT_RECIPE : p1 });
      return viewRepoForm(view, { name: p0 });
    case "admin/repository/dashboard": return viewDashboard(view, p0);
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
    case "admin/support/activity": return viewActivity(view);
    case "admin/support/logging": return viewLogging(view);
    case "admin/support/metrics": return viewMetrics(view);
    case "admin/support/usage": return viewUsage(view);
    case "admin/support/supportzip": return viewSupportZip(view);
    case "admin/support/systeminformation": return viewSysInfo(view);
    case "admin/support/status": return viewStatus(view, p0);
    case "admin/system/api": return viewApi(view);
    case "admin/system/configuration": return viewConfiguration(view);
    case "admin/system/recovery": return viewRecovery(view, p0);
    case "admin/system/emailserver": return viewGeneral(view, "email");
    case "admin/system/http": return viewGeneral(view, "http");
    case "admin/system/nodes": return viewNodes(view);
    case "admin/system/tasks": return viewTasks(view, p0);
    case "admin/system/webhooks": return viewWebhooks(view, p0);
    case "admin/system/general": return viewGeneral(view, "general");
  }
  return null;
}
