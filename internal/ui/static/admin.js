// Access and system screens (plan 10 Decision 8; the tasks and cleanup
// policies are in ops.js, users, roles, realms and the account page in
// access.js): API tokens, status and system information. Admin screens use the routes: the list at "#admin/<screen>",
// a detail at "#admin/<screen>:<id>".
import {
  state, h, put, api, enc, alive, filterBar, row, fmtBytes, fmtNum, fmtDate, fmtUptime,
  copyLine, pageHead, loading, truncNote, card, kv, stat, tbl, handleError, needLogin, icon, s, markBusy,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete, viewRepos } from "./browse.js";
import { viewRepoForm, DEFAULT_RECIPE } from "./repoform.js";
import { viewTasks, viewPolicies, trendCard, logsCard } from "./ops.js";
import { viewUsers, viewRoles, viewRealms } from "./access.js";
import { viewBlobStores, viewGeneral } from "./settings.js";

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
    h("div", { class: "actions end" }, btn));

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
    h("div", { class: "two" }, list, card(t("tok.new"), t("tok.newSub"), form)));
  await load();
}

// ---- status --------------------------------------------------------------------------

const rssSamples = []; // last 60 RSS samples, kept across visits
let lastStatus = null;
let statusUser = null; // whose samples these are

function sparkline(values) {
  const W = 300, H = 60;
  const svg = s("svg", { class: "spark", viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img", "aria-label": t("st.rssAria") });
  if (values.length < 2) return svg;
  let min = Math.min(...values), max = Math.max(...values);
  if (max - min < 1) { max += 1; min -= 1; }
  const pad = (max - min) * 0.1;
  min -= pad; max += pad;
  const step = W / 59;
  const x0 = W - (values.length - 1) * step;
  const pts = values.map((v, i) => (x0 + i * step).toFixed(1) + "," + (H - (v - min) / (max - min) * H).toFixed(1));
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
  const spark = h("div", null);
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
const healthBadge = (ok) => badge("● " + (ok ? t("st.ok") : t("st.bad")), ok ? "green" : "red");

function healthBox(c) {
  return h("div", { class: "healthbox" + (c.healthy ? "" : " bad") },
    h("h3", null, h("a", { href: statusLink(c.name) }, c.name)),
    healthBadge(c.healthy),
    h("p", { class: "tiny muted hb-msg" }, c.message || dash),
    h("p", { class: "tiny muted" }, t("st.checked", { time: fmtDate(c.lastChecked), ms: fmtNum(c.durationMs) })));
}

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
    put(box,
      h("div", { class: bad ? "attention" : "notice green", role: "status" },
        h("strong", null, bad ? t("st.sumBad", { n: fmtNum(bad) }) : t("st.sumOk", { n: fmtNum(checks.length) }))),
      checks.length ? h("div", { class: "health-grid" }, checks.map(healthBox)) : notFound(t("st.none")));
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
    h("h2", { class: "section-title" }, t("st.checks")), box,
    ops ? [h("h2", { class: "section-title" }, t("st.activity")),
      h("div", { class: "two" }, trendCard(isLive, "24h", t("st.trend"), t("st.trendSub")), m.card),
      logsCard(isLive, criteria)] : null,
    h("h2", { class: "section-title" }, t("st.metrics")), m.stats, ops ? null : m.card);
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

async function viewSysInfo(view) {
  const isLive = alive();
  const d = await api("systeminformation");
  if (!isLive()) return;
  const dl = h("button", { type: "button", class: "btn" }, t("common.download"));
  dl.addEventListener("click", () => downloadJSON("sysinfo.json", d));
  const names = SYS_SECTIONS.filter((n) => d[n]).concat(Object.keys(d).filter((n) => !SYS_SECTIONS.includes(n)).sort());
  const title = (n) => (SYS_SECTIONS.includes(n) ? tk("sys", n.replace(/^[a-z]+-/, "")) : n);
  put(view,
    pageHead(t("nav.sysinfo"), sub(t("sys.sub")), acts(dl), { eyebrow: t("sys.eyebrow") }),
    h("div", { class: "equal sysinfo" }, names.map((n) => card(title(n), n,
      Object.entries(d[n] || {}).map(([k, v]) => kv(k, h("span", { class: "mono" }, sysValue(k, v))))))));
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
  }
  return null;
}
