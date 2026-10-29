// Access and system screens (plan 10 Decision 8; the tasks and cleanup
// policies are in ops.js, users, roles, realms and the account page in
// access.js): API tokens, status and system information. Admin screens use the routes: the list at "#admin/<screen>",
// a detail at "#admin/<screen>:<id>".
import {
  state, h, put, api, enc, encSeg, alive, filterBar, row, fmtBytes, fmtNum, fmtDate, fmtUptime,
  copyLine, copyButton, pageHead, loading, truncNote, card, kv, stat, tbl, handleError, needLogin, icon, s, markBusy,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete, viewRepos } from "./browse.js";
import { viewRepoForm, DEFAULT_RECIPE } from "./repoform.js";
import { viewTasks, viewPolicies, trendCard, logsCard, viewRoutingRules, viewSelectors, viewWebhooks, viewTags, viewStaging } from "./ops.js";
import { viewUsers, viewRoles, viewRealms, viewUserTokens } from "./access.js";
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

// viewTokens: admin shows every user's tokens ("#admin/security/usertoken")
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

// ---- status --------------------------------------------------------------------------

const rssSamples = []; // last 60 RSS samples, kept across visits
let lastStatus = null;
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

// metrics returns the metrics nodes; it polls /-/api/status every 5 s
// while the page is visible, until the route changes.
function metrics(isLive, onData) {
  if (statusUser !== state.me.user) { // another user signed in: drop the old samples
    rssSamples.length = 0;
    lastStatus = null;
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
      stat({ label: t("st.uptime"), value: fmtUptime(st.uptimeSeconds), foot: st.version ? t("st.version", { v: st.version }) : "", right: t("st.runtime", { n: fmtBytes(st.sysBytes) }) }));
    put(spark, sparkline(rssSamples),
      h("div", { class: "spark-meta" },
        h("span", null, rssSamples.length ? t("st.min", { n: fmtBytes(Math.min(...rssSamples)) }) : ""),
        h("span", null, last ? t("st.now", { n: fmtBytes(last) }) : t("st.noRss")),
        h("span", null, rssSamples.length ? t("st.max", { n: fmtBytes(Math.max(...rssSamples)) }) : "")),
      h("p", { class: "hint" }, t("st.blobs", { n: fmtNum(st.blobs), size: fmtBytes(st.blobBytes) })));
  };
  let busy = false, stopped = false;
  async function poll() {
    if (busy || stopped || !isLive()) return;
    busy = true;
    try {
      const st = await api("status");
      if (!isLive()) return;
      if (st.rssBytes) {
        rssSamples.push(st.rssBytes);
        if (rssSamples.length > 60) rssSamples.splice(0, rssSamples.length - 60);
      }
      lastStatus = st;
      render(st);
      updated.textContent = t("st.updated", { time: fmtDate(new Date()) });
      if (onData) onData();
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
  if (lastStatus) render(lastStatus); // no flash when moving between list and detail
  poll();
  return { stats, card: card(t("st.rssTitle"), t("st.rssSub"), spark, updated), poll };
}

const statusLink = (name) => "#admin/support/status" + (name ? ":" + enc(name) : "");
const healthBadge = (ok) => badge([h("span", { class: "dot " + (ok ? "ok" : "err"), "aria-hidden": "true" }), ok ? t("st.ok") : t("st.bad")]);

async function viewStatus(view, name, criteria) {
  const isLive = alive();
  let checks = null;
  // Download needs both the metrics and the health checks, so it waits for the first poll.
  const syncDl = () => { dl.disabled = !(checks && lastStatus); };
  const m = metrics(isLive, () => syncDl());
  const box = h("div", null, loading());
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
      const d = await api("status/checks");
      if (!isLive()) return;
      checks = d.checks || [];
      syncDl();
      render();
    } catch (e) {
      if (!isLive()) return;
      put(box, notFound(t("st.loadFailed")));
      handleError(e);
    } finally {
      refresh.disabled = false;
    }
  };
  refresh.addEventListener("click", () => { m.poll(); load(); });
  dl.addEventListener("click", () => downloadJSON("status.json", { generated: new Date().toISOString(), metrics: lastStatus, checks }));

  const ops = name === undefined; // the request chart and the logs belong to the overview
  put(view, pageHead(t("nav.status"), sub(t("st.sub")), acts(refresh, dl), { eyebrow: t("sys.eyebrow") }),
    box,
    ops ? [
      h("div", { class: "two" }, trendCard(isLive, "24h", t("st.trend"), t("st.trendSub")), m.card),
      logsCard(isLive, criteria)] : null,
    h("h2", { class: "section-title" }, t("st.metrics")), m.stats, ops ? null : m.card,
    ops ? systemInfoDisclosure(isLive) : null);
  if (criteria && criteria.has("repo")) view.querySelector("#repository-logs").scrollIntoView({ block: "start" });
  await load();
}

// ---- system information ---------------------------------------------------------------

const SYS_SECTIONS = ["commandcenter-status", "commandcenter-configuration", "system-runtime", "system-environment", "system-filestores"];
// tk looks up "<base>.<k>"; here the known sections ("sys.status", …).
const tk = (base, k) => t(base + "." + k);
const BYTE_KEY = /bytes$|^heap|^sys$/i;

function sysValue(k, v) {
  if (v === "" || v === null || v === undefined) return dash;
  if (typeof v === "object") return Object.entries(v).map(([a, b]) => a + "=" + b).join(", ") || dash;
  if (typeof v === "number" && BYTE_KEY.test(k) && v >= 1024) return fmtNum(v) + " (" + fmtBytes(v) + ")";
  return String(v);
}

function systemInfoTable(d) {
  const names = SYS_SECTIONS.filter((n) => d[n]).concat(Object.keys(d).filter((n) => !SYS_SECTIONS.includes(n)).sort());
  return tbl([t("sys.section"), t("col.name"), t("sys.value")], names.flatMap((n) =>
    Object.entries(d[n] || {}).map(([k, v]) => row([
      [SYS_SECTIONS.includes(n) ? tk("sys", n.replace(/^[a-z]+-/, "")) : n],
      [k], [sysValue(k, v)],
    ]))));
}

function systemInfoDisclosure(isLive) {
  const body = h("div", null);
  const details = h("details", { class: "card system-info" },
    h("summary", { class: "card-head" }, t("nav.sysinfo")), body);
  let loaded = false, busy = false;
  details.addEventListener("toggle", async () => {
    if (!details.open || loaded || busy) return;
    busy = true;
    put(body, loading());
    try {
      const d = await api("systeminformation");
      if (!isLive()) return;
      const dl = h("button", { type: "button", class: "btn small", onclick: () => downloadJSON("sysinfo.json", d) }, t("common.download"));
      put(body, h("div", { class: "toolbar" }, h("span", { class: "grow" }, t("sys.sub")), dl), systemInfoTable(d));
      loaded = true;
    } catch (e) {
      if (isLive()) { put(body, h("div", { class: "empty" }, e.message)); handleError(e); }
    } finally { busy = false; }
  });
  return details;
}

async function viewSysInfo(view) {
  const isLive = alive();
  const d = await api("systeminformation");
  if (!isLive()) return;
  const dl = h("button", { type: "button", class: "btn" }, t("common.download"));
  dl.addEventListener("click", () => downloadJSON("sysinfo.json", d));
  const panel = card(t("nav.sysinfo"), t("sys.sub"), null);
  panel.appendChild(systemInfoTable(d));
  put(view,
    pageHead(t("nav.sysinfo"), sub(t("sys.sub")), acts(dl), { eyebrow: t("sys.eyebrow") }),
    panel);
}

// ---- support: bundle, thread dump, log level, metrics ---------------------------------
//   #admin/system/support

const ZIP_ENTRIES = ["info.json", "config.json", "repositories.json", "blobstores.json", "status.json", "sysinfo.json", "tasks.json", "webhooks.json", "request-stats.json", "events.json", "logs.txt", "threads.txt", "metrics.txt"];
const LOG_LEVELS = ["debug", "info", "warn", "error"]; // what PUT /-/api/logging accepts

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

async function viewSupport(view) {
  const isLive = alive();
  const eb = { eyebrow: t("sys.eyebrow") };
  const [lg, st] = await Promise.all([api("logging").catch((e) => ({ error: e })), api("settings").catch(() => null)]);
  if (!isLive()) return;

  // support bundle: a plain link, so the browser saves the zip
  const zip = card(t("sup.zipTitle"), t("sup.zipSub"), [
    h("p", { class: "tiny" }, t("sup.zipContains")),
    h("p", { class: "tiny mono zip-entries" }, ZIP_ENTRIES.join(" · ")),
    h("div", { class: "notice" }, t("sup.zipRedacted")),
    h("div", { class: "actions" }, h("a", { class: "btn primary", href: "/-/api/support/zip", download: "" }, t("sup.zipDownload")))]);

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

  // runtime log level
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
  const logging = card(t("sup.levelTitle"), t("sup.levelSub"), [
    fields(field(t("sup.level"), level, t("sup.levelHelp"), { full: true })), levelMsg,
    h("div", { class: "notice amber" }, t("sup.levelNote"))]);

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
  const metrics = card(t("sup.metricsTitle"), t("sup.metricsSub"), [
    copyLine(location.origin + "/-/metrics/prometheus", t("sup.metricsCopy")),
    h("p", { class: "tiny muted metrics-auth" }, t("sup.metricsAuth")),
    h("div", { class: "field" }, h("label", { class: "check" }, anon, t("sup.metricsAnon")), h("small", null, t("sup.metricsAnonHelp"))), anonMsg]);

  put(view, pageHead(t("nav.support"), sub(t("sup.sub")), null, eb),
    h("div", { class: "two" }, h("div", { class: "stack" }, zip, threads), h("div", { class: "stack" }, logging, metrics)));
}

// ---- SSL certificates (the trust store) ------------------------------------------------
//   #admin/security/certificates

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
    case "admin/security/users": return viewUsers(view, p0);
    case "admin/security/roles": return viewRoles(view, p0);
    case "admin/security/realms": return viewRealms(view);
    case "admin/security/usertoken": return viewTokens(view, true);
    case "admin/support/status": return viewStatus(view, p0, criteria);
    case "admin/support/systeminformation": return viewSysInfo(view);
    case "admin/system/tasks": return viewTasks(view, p0);
    case "admin/repository/cleanuppolicies": return viewPolicies(view, p0);
    case "admin/repository/blobstores": return viewBlobStores(view, p0);
    case "admin/system/general": return viewGeneral(view);
    case "admin/repository/routingrules": return viewRoutingRules(view, p0);
    case "admin/security/contentselectors": return viewSelectors(view, p0);
    case "admin/system/webhooks": return viewWebhooks(view, p0);
    case "admin/system/support": return viewSupport(view);
    case "admin/security/certificates": return viewCertificates(view);
    case "admin/repository/tags": return viewTags(view, p0);
    case "admin/repository/staging": return viewStaging(view);
    case "admin/repository/quarantine": return viewQuarantine(view);
    case "admin/security/user-tokens": return viewUserTokens(view, true);
    case "admin/system/vulnerability": return viewVulnerability(view);
  }
  return null;
}
