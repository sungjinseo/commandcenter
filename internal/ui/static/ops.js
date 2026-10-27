// Operations screens (plan 11 Decision 8): cleanup policies, scheduled
// tasks, the request trend chart and the log viewer. Every number and name
// comes from the API and reaches the page through h()/textContent; the
// charts are SVG built with createElementNS from the API numbers only.
//   #admin/repository/cleanuppolicies[:create|:<name>]
//   #admin/system/tasks[:create|:<id>]
import {
  state, h, put, api, enc, encSeg, alive, adminRepoLink, safeURL, fmtBytes, fmtNum, fmtDate, FORMATS, FORMAT_NAMES,
  fmtBadge, nameCell, pageHead, loading, card, kv, tbl, handleError, showError, icon, s, markBusy, taskName, loadRepos, debounce,
} from "./util.js";
import { t, locale } from "./i18n.js";
import { confirmDelete } from "./browse.js";
import { modal, rerender } from "./shell.js";

let uidN = 0;
const uid = (p) => "op-" + p + "-" + (++uidN);
const dash = "—";
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const acts = (...b) => h("div", { class: "actions" }, b);
const sub = (text) => h("p", null, text);
const notFound = (text) => h("section", { class: "card" }, h("div", { class: "empty" }, text));
const actHead = () => ({ t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" });
const pad2 = (n) => String(n).padStart(2, "0");

// ---- form helpers -------------------------------------------------------------------

export function field(label, control, help, o) {
  o = o || {};
  if (!control.id) control.id = uid("c");
  const hid = help ? uid("h") : null;
  if (hid) control.setAttribute("aria-describedby", hid);
  return h("div", { class: "field" + (o.full ? " full" : ""), hidden: !!o.hidden },
    h("label", { for: control.id }, label, o.req ? h("span", { class: "required", "aria-hidden": "true" }, " *") : null),
    control, help ? h("small", { id: hid }, help) : null);
}
export const fields = (...list) => h("div", { class: "fields" }, list.filter(Boolean));
export function sel(options, value, attrs) {
  const el = h("select", attrs || null, options.map(([v, label]) => h("option", { value: v }, label)));
  el.value = value;
  return el;
}
export const textIn = (value, attrs) => h("input", Object.assign({ type: "text", value: value || "", autocomplete: "off", spellcheck: "false" }, attrs || {}));
const numIn = (value, min, max) => h("input", { type: "number", inputmode: "numeric", min, max, step: 1, value: String(value) });
const intOf = (el) => (/^\s*\d+\s*$/.test(el.value) ? Number(el.value) : NaN);
export const button = (label, cls) => h("button", { type: "button", class: "btn" + (cls ? " " + cls : "") }, label);

// formState wires an error box, a success box and the dirty mark of a form.
// A form with edits is in-progress work: a language change must not rebuild it.
let flash = null; // {to, text}: a one-shot success message for the screen at hash "to"
export function formState(form) {
  const err = h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
  const text = flash && flash.to === location.hash ? flash.text : "";
  flash = null;
  const ok = h("div", { class: "notice ok", role: "status", hidden: !text }, text);
  let unBusy = null;
  const dirty = () => { if (!unBusy) unBusy = markBusy("opsform"); };
  form.addEventListener("input", dirty);
  form.addEventListener("change", dirty);
  return {
    err, ok, dirty,
    clean() { if (unBusy) { unBusy(); unBusy = null; } }, // the edits are saved or dropped
    show(list, head) {
      put(err, h("strong", null, head || (list.length > 1 ? t("rf.errMany") : t("rf.errOne"))), h("ul", null, list.map((x) => h("li", null, x))));
      err.hidden = false;
      ok.hidden = true;
      err.focus();
    },
    clear() { err.hidden = true; },
    // done goes to "to" with a success message once the save has landed.
    done(to, msg) {
      if (unBusy) { unBusy(); unBusy = null; }
      flash = { to, text: msg };
      if (location.hash === to) rerender();
      else location.hash = to;
    },
  };
}
export const errList = (e) => (e.errors && e.errors.length ? e.errors : [e.message]);

// ---- request trend chart ------------------------------------------------------------

// areaChart draws /-/api/stats/requests buckets as the concept's area chart:
// hourly buckets get local times, daily (UTC) buckets get UTC dates, both
// formatted by Intl in the current language. The y axis shows the peak.
export function areaChart(d, rangeLabel) {
  const buckets = (d && d.buckets) || [];
  const hourly = Number(d && d.step) === 3600;
  const W = 620, H = 135, TOP = 12, BOT = 3;
  const vals = buckets.map((b) => Math.max(0, Number(b.requests) || 0));
  const n = vals.length;
  const max = Math.max(1, ...vals);
  const x = (i) => (n > 1 ? (i / (n - 1)) * W : W / 2);
  const y = (v) => TOP + (1 - v / max) * (H - TOP - BOT);
  const df = new Intl.DateTimeFormat(locale(), hourly
    ? { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }
    : { month: "numeric", day: "numeric", timeZone: "UTC" });
  const when = (v) => { const dt = new Date(v); return isNaN(dt) ? String(v) : df.format(dt); };
  const total = vals.reduce((a, b) => a + b, 0);
  const svg = s("svg", { viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img",
    "aria-label": t("chart.aria", { range: rangeLabel, n: fmtNum(total), max: fmtNum(Math.max(...vals, 0)) }) });
  const gid = uid("grad");
  const grad = s("linearGradient", { id: gid, x1: 0, y1: 0, x2: 0, y2: 1 });
  grad.append(s("stop", { offset: "0%", class: "c-stop0" }), s("stop", { offset: "100%", class: "c-stop1" }));
  const defs = s("defs");
  defs.append(grad);
  svg.append(defs, s("path", { class: "c-grid", d: [max, max / 2, 0].map((v) => "M0 " + y(v).toFixed(1) + "H" + W).join("") }));
  if (n) {
    const line = "M" + vals.map((v, i) => x(i).toFixed(1) + " " + y(v).toFixed(1)).join("L");
    svg.append(s("path", { class: "c-area", fill: "url(#" + gid + ")", d: line + "V" + H + "H" + x(0).toFixed(1) + "Z" }),
      s("path", { class: "c-line", d: line }));
    const w = n > 1 ? W / (n - 1) : W;
    buckets.forEach((b, i) => { // hover targets with a native tooltip
      const r = s("rect", { class: "c-hit", x: Math.max(0, x(i) - w / 2).toFixed(1), y: 0, width: w.toFixed(1), height: H });
      const tip = s("title");
      tip.textContent = when(b.t) + " · " + t("chart.point", { n: fmtNum(vals[i]) });
      r.append(tip);
      svg.append(r);
    });
  }
  // y labels at the peak and the half, x labels at about seven points
  const yl = h("div", { class: "chart-y", "aria-hidden": "true" });
  for (const v of [max, Math.round(max / 2)]) {
    const sp = h("span", null, fmtNum(v));
    sp.style.top = (y(v) / H * 100).toFixed(1) + "%";
    yl.append(sp);
  }
  const xl = h("div", { class: "chart-labels", "aria-hidden": "true" });
  const every = Math.max(1, Math.ceil(n / 7));
  buckets.forEach((b, i) => {
    const last = i === n - 1;
    if (i % every && !last) return;
    if (!last && i && n - 1 - i < every) return; // too close to the last label
    const sp = h("span", { class: i === 0 ? "first" : last ? "last" : null }, when(b.t));
    sp.style.left = (x(i) / W * 100).toFixed(2) + "%";
    xl.append(sp);
  });
  return h("div", { class: "chart-box" },
    h("div", { class: "chart-key" }, h("span", null, h("i"), t("chart.requests")),
      h("span", { class: "end" }, t("chart.total", { range: rangeLabel, n: fmtNum(total) }))),
    h("div", { class: "chart" }, svg, yl), xl);
}

// trendCard loads one range of request statistics into a card.
export function trendCard(isLive, range, title, subText) {
  const label = range === "24h" ? t("chart.24h") : t("chart.7d");
  const body = h("div", null, loading());
  api("stats/requests?range=" + range).then((d) => {
    if (isLive()) put(body, areaChart(d, label));
  }, (e) => {
    if (!isLive()) return;
    put(body, h("div", { class: "empty" }, t("chart.failed")));
    if (e.status !== 403) handleError(e);
  });
  return card(title, subText, body, badge(label));
}

// ---- repository operations ----------------------------------------------------------

const RANGE_MS = { "1h": 3600000, "24h": 86400000, "7d": 604800000 };
const rangeOptions = () => [["1h", t("repoOps.1h")], ["24h", t("repoOps.24h")], ["7d", t("repoOps.7d")]];
const metric = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0;
const percent = (v) => (v * 100).toLocaleString(locale(), { maximumFractionDigits: 2 }) + "%";

export function repositoryStatsCard(isLive) {
  const range = sel(rangeOptions(), "24h", { "aria-label": t("repoOps.range") });
  const refresh = button([icon("refresh"), t("logs.refresh")], "small");
  const body = h("div");
  const collected = h("p", { role: "status", title: t("repoOps.sub") });
  const panel = card(t("repoOps.title"), null, null, acts(range, refresh));
  panel.querySelector(".card-head > div").appendChild(collected);
  panel.appendChild(body);
  panel.hidden = true; // An unimplemented endpoint must leave no panel behind.
  let seq = 0;
  const load = async () => {
    const n = ++seq, selected = range.value;
    refresh.disabled = true;
    put(body, loading());
    put(collected);
    try {
      const d = await api("stats/repositories?range=" + selected);
      if (!isLive() || n !== seq) return;
      if (!d || !Array.isArray(d.repositories)) throw new Error("Invalid repository statistics");
      panel.hidden = false;
      put(collected, t("repoOps.collected", {
        range: rangeOptions().find(([value]) => value === selected)[1],
        at: d.collectedAt ? fmtDate(d.collectedAt) : dash,
      }));
      put(body, tbl([
        t("col.repo"), t("col.status"), ...[
          t("repoOps.requests"), t("repoOps.errorRate"), t("repoOps.upstreamFailures"), t("repoOps.upstreamLatency"),
          t("repoOps.cache"), t("repoOps.bytesIn"), t("repoOps.bytesOut"), t("repoOps.latency"),
        ].map((label) => ({ t: label, cls: "num" })),
      ], d.repositories.map((r) => {
        const href = "#admin/support/status?" + new URLSearchParams({ repo: r.name, range: selected });
        const cell = (v, format = fmtNum) => h("td", { class: "num" }, metric(v)
          ? h("a", { class: "metric-link", href, title: t("repoOps.openLogs", { repo: r.name }) }, format(v)) : dash);
        const latencyCell = (avg, max) => cell(avg, (v) => fmtNum(v) + " / " + (metric(max) ? fmtNum(max) : dash) + " ms");
        const proxy = r.type === "proxy";
        const cacheTotal = metric(r.cacheHits) && metric(r.cacheMisses) ? r.cacheHits + r.cacheMisses : 0;
        const bad = r.errorRate >= 0.01 || (proxy && r.upstreamFailures > 0);
        const status = r.requests > 0 || bad ? (bad ? t("repoOps.bad") : t("repoOps.ok")) : t("repoOps.noTraffic");
        const open = adminRepoLink(r.name);
        const row = h("tr", { class: "clickable", tabindex: 0, "aria-label": r.name,
          onclick: (ev) => { if (!ev.target.closest("a,button,.menu")) location.hash = open; },
          onkeydown: (ev) => {
            if (ev.target === ev.currentTarget && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); location.hash = open; }
          },
        },
          h("td", null, nameCell(r.format, h("a", { href: open }, h("strong", null, r.name)), r.format + " · " + r.type)),
          h("td", null, badge([h("i", { class: "dot", "aria-hidden": "true" }), status], bad ? "red" : r.requests > 0 ? "green" : null)),
          cell(r.requests), cell(r.errorRate, percent), cell(proxy ? r.upstreamFailures : null),
          latencyCell(proxy ? r.upstreamLatencyAvgMs : null, r.upstreamLatencyMaxMs),
          cell(proxy && cacheTotal > 0 ? r.cacheHits / cacheTotal : null, percent),
          cell(r.bytesIn, fmtBytes), cell(r.bytesOut, fmtBytes), latencyCell(r.latencyAvgMs, r.latencyMaxMs));
        return row;
      }), t("repoOps.none")));
    } catch (e) {
      if (!isLive() || n !== seq) return;
      panel.hidden = e.status === 404;
      if (!panel.hidden) put(body, h("div", { class: "empty", role: "alert" }, t("repoOps.failed")));
    } finally {
      if (n === seq) refresh.disabled = false;
    }
  };
  range.addEventListener("change", load);
  refresh.addEventListener("click", load);
  load();
  return panel;
}

// ---- log viewer -------------------------------------------------------------------

// logClass colours a WARN line amber and an ERROR line red.
const logClass = (level) => (level === "ERROR" ? "err" : level === "WARN" ? "warn" : null);

// The API only filters level/text. Apply repo/time locally to all retained
// records (2,000 maximum), and generate filtered downloads from the same rows.
function matchesLogRepo(attrs, repo) {
  const pairs = String(attrs || "").matchAll(/(?:^|\s)(?:[\w-]+\.)*(?:repo|repository)=("(?:\\.|[^"\\])*"|[^\s]+)/g);
  for (const [, raw] of pairs) {
    let value = raw;
    if (raw.startsWith('"')) { try { value = JSON.parse(raw); } catch (e) { continue; } }
    if (value === repo) return true;
  }
  return false;
}

export function logsCard(isLive, criteria = new URLSearchParams()) {
  const level = sel([["", t("logs.all")], ["INFO", "INFO+"], ["WARN", "WARN+"], ["ERROR", "ERROR"]], "", { "aria-label": t("logs.level") });
  const repo = textIn(criteria.get("repo"), { type: "search", placeholder: t("logs.repo"), "aria-label": t("logs.repo"), maxlength: 64 });
  const range = sel([["", t("logs.allTime")], ...rangeOptions()], RANGE_MS[criteria.get("range")] ? criteria.get("range") : "", { "aria-label": t("repoOps.range") });
  const q = h("input", { type: "search", placeholder: t("logs.filter"), "aria-label": t("logs.filter"), autocomplete: "off", maxlength: 200 });
  const refresh = button([icon("refresh"), t("logs.refresh")], "small");
  const dl = h("a", { class: "btn small", href: "/-/api/logs/download", download: "commandcenter.log" }, t("logs.download"));
  const box = h("div", { class: "log tall", role: "log", tabindex: "0", "aria-label": t("logs.title") }, loading());
  const count = h("span", { class: "end", role: "status" });
  let seq = 0;
  let downloadURL = null;
  const clearDownload = () => {
    if (downloadURL) URL.revokeObjectURL(downloadURL);
    downloadURL = null;
    dl.removeAttribute("href");
    dl.setAttribute("aria-disabled", "true");
  };
  state.leaving.push(clearDownload);
  const load = async () => {
    later.cancel();
    clearDownload();
    count.textContent = "";
    put(box, loading());
    const repository = repo.value.trim();
    const filtered = !!(repository || range.value);
    const until = Date.now(), since = range.value ? until - RANGE_MS[range.value] : -Infinity;
    const p = new URLSearchParams();
    if (level.value) p.set("level", level.value);
    if (q.value.trim()) p.set("q", q.value.trim());
    const qs = p.toString();
    const n = ++seq;
    refresh.disabled = true;
    try {
      const d = await api("logs?limit=" + (filtered ? 2000 : 500) + (qs ? "&" + qs : ""));
      if (!isLive() || n !== seq) return;
      const logs = (d.logs || []).filter((r) => (!repository || matchesLogRepo(r.attrs, repository)) &&
        (!range.value || (Date.parse(r.time) >= since && Date.parse(r.time) <= until)));
      put(box, logs.length ? logs.map((r) => h("div", { class: logClass(r.level) },
        fmtDate(r.time) + " " + r.level + " " + r.message + (r.attrs ? " " + r.attrs : ""))) : h("div", null, t("logs.none")));
      box.scrollTop = 0;
      count.textContent = t("logs.count", { n: fmtNum(logs.length) });
      if (filtered) {
        const text = logs.slice().reverse().map((r) => [r.time, r.level, r.message, r.attrs].filter(Boolean).join(" ").replace(/[\r\n]/g, " ")).join("\n");
        downloadURL = URL.createObjectURL(new Blob([text ? text + "\n" : ""], { type: "text/plain;charset=utf-8" }));
        dl.setAttribute("href", downloadURL); // trusted URL created above
      } else dl.setAttribute("href", safeURL("/-/api/logs/download" + (qs ? "?" + qs : "")));
      dl.removeAttribute("aria-disabled");
    } catch (e) {
      if (!isLive() || n !== seq) return;
      put(box, h("div", null, t("logs.failed")));
      handleError(e);
    } finally {
      if (n === seq) refresh.disabled = false;
    }
  };
  const later = debounce(load, 300);
  state.leaving.push(later.cancel);
  level.addEventListener("change", load);
  repo.addEventListener("input", later);
  range.addEventListener("change", load);
  q.addEventListener("input", later);
  refresh.addEventListener("click", load);
  load();
  const panel = card(t("logs.title"), t("logs.sub"), [h("div", { class: "toolbar" }, level, repo, range, q, refresh, dl, count),
    h("p", { class: "tiny muted" }, t("logs.retained")), box]);
  panel.id = "repository-logs";
  return panel;
}

// ---- cleanup policies ----------------------------------------------------------------

const polLink = (name) => "#admin/repository/cleanuppolicies" + (name ? ":" + enc(name) : "");
const days = (n) => (Number(n) > 0 ? t("pol.days", { n: fmtNum(n) }) : dash);
const polFormat = (f) => (f === "all" ? badge(t("pol.allFormats")) : fmtBadge(f));
// cleanable: the repositories a cleanup policy can apply to (Decision 6).
const cleanable = (r) => (r.type === "hosted" || r.type === "proxy") && !(r.type === "hosted" && r.format === "cargo");
const matches = (pf, rf) => pf === "all" || pf === rf;
const RELEASE = () => [["any", t("pol.relAny")], ["release", t("pol.relRelease")], ["prerelease", t("pol.relPre")]];
const relText = (v) => (RELEASE().find(([k]) => k === (v || "any")) || [0, v])[1];
const polEyebrow = () => ({ eyebrow: t("task.eyebrow") });

export async function viewPolicies(view, name) {
  if (name !== undefined) return policyForm(view, name === "create" ? null : name);
  const isLive = alive();
  const list = (await api("cleanup/policies")).policies || [];
  if (!isLive()) return;
  put(view,
    pageHead(t("nav.cleanup"), sub(t("pol.sub")), acts(h("a", { class: "btn primary", href: polLink("create") }, t("pol.create"))), polEyebrow()),
    h("section", { class: "card" }, tbl(
      [t("pol.policy"), t("col.format"), t("pol.age"), t("pol.lastDl"), t("pol.repos"), actHead()],
      list.map((p) => h("tr", null,
        h("td", { class: "name" }, h("a", { href: polLink(p.name) }, h("strong", null, p.name)),
          p.namePattern || p.releaseType !== "any" ? h("div", { class: "tiny muted" }, [relText(p.releaseType), p.namePattern].filter(Boolean).join(" · ")) : null),
        h("td", null, polFormat(p.format)),
        h("td", { class: "nowrap" }, days(p.componentAgeDays)),
        h("td", { class: "nowrap" }, days(p.lastDownloadedDays)),
        h("td", null, (p.repositories || []).length ? t("pol.repoCount", { n: fmtNum(p.repositories.length) }) : dash,
          (p.repositories || []).length ? h("div", { class: "tiny muted" }, p.repositories.join(", ")) : null),
        h("td", { class: "act" }, h("a", { class: "btn small", href: polLink(p.name) }, t("pol.open"))))),
      t("pol.none"))),
    h("div", { class: "notice amber" }, t("pol.note")));
}

function previewModal(name, d) {
  const close = button(t("common.close"));
  const m = modal(t("pol.listTitle", { name }), [
    tbl([t("comp.component"), t("col.version"), t("col.repo"), t("pol.createdCol"), t("pol.lastDl"), { t: t("pol.assetsCol"), cls: "num" }, { t: t("col.size"), cls: "num" }],
      (d.components || []).map((c) => h("tr", null,
        h("td", { class: "name" }, h("strong", null, c.group ? c.group + ":" + c.name : c.name)),
        h("td", null, c.version || dash),
        h("td", { class: "nowrap" }, h("a", { href: adminRepoLink(c.repo) }, c.repo)),
        h("td", { class: "nowrap" }, fmtDate(c.created)),
        h("td", { class: "nowrap" }, c.lastDownloaded ? fmtDate(c.lastDownloaded) : t("pol.never")),
        h("td", { class: "num" }, fmtNum(c.assets)),
        h("td", { class: "num" }, fmtBytes(c.bytes)))), t("pol.noCandidates")),
    d.truncated ? h("div", { class: "notice amber" }, t("pol.listTrunc", { n: fmtNum((d.components || []).length) })) : null,
    h("div", { class: "notice" }, t("pol.noDelete")),
  ], [close], { cls: "modal-wide" });
  close.addEventListener("click", m.close);
}

async function policyForm(view, name) {
  const isLive = alive();
  const creating = name === null;
  const [p, repos] = await Promise.all([creating ? null : api("cleanup/policies/" + encSeg(name)), loadRepos(true)]);
  if (!isLive()) return;
  const cur = p || { name: "", format: "all", componentAgeDays: 30, lastDownloadedDays: 0, releaseType: "any", namePattern: "", repositories: [] };
  const back = h("a", { class: "btn", href: polLink() }, t("pol.back"));

  const nameIn = textIn(cur.name, { disabled: !creating, required: true, maxlength: 64 });
  const format = sel([["all", t("pol.allFormats")]].concat(FORMATS.map((f) => [f, FORMAT_NAMES[f]])), cur.format);
  const age = numIn(cur.componentAgeDays, 0, 36500);
  const lastDl = numIn(cur.lastDownloadedDays, 0, 36500);
  const rel = sel(RELEASE(), cur.releaseType || "any");
  const pat = textIn(cur.namePattern, { maxlength: 200, placeholder: "^com\\.example:.*$", class: "mono" });
  const save = h("button", { type: "submit", class: "btn primary" }, creating ? t("pol.createBtn") : t("pol.save"));
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  put(form, fs.err,
    fields(
      field(t("pol.name"), nameIn, creating ? t("pol.nameHelp") : null, { req: creating }),
      field(t("col.format"), format, t("pol.formatHelp")),
      field(t("pol.ageField"), age, t("pol.ageHelp")),
      field(t("pol.lastDlField"), lastDl, t("pol.lastDlHelp")),
      field(t("pol.release"), rel, t("pol.releaseHelp")),
      field(t("pol.pattern"), pat, t("pol.patternHelp"))),
    h("div", { class: "notice" }, t("pol.andNote")),
    h("div", { class: "actions end" }, save));

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const b = {
      name: creating ? nameIn.value.trim() : cur.name, format: format.value,
      componentAgeDays: intOf(age), lastDownloadedDays: intOf(lastDl), releaseType: rel.value, namePattern: pat.value.trim(),
    };
    const errs = [];
    if (!b.name) errs.push(t("pol.e.name"));
    if (isNaN(b.componentAgeDays) || isNaN(b.lastDownloadedDays)) errs.push(t("pol.e.days"));
    else if (!b.componentAgeDays && !b.lastDownloadedDays) errs.push(t("pol.e.ageNone"));
    if (errs.length) { fs.show(errs); return; }
    save.disabled = true;
    try {
      const out = creating
        ? await api("cleanup/policies", { method: "POST", body: b })
        : await api("cleanup/policies/" + encSeg(cur.name), { method: "PUT", body: b });
      if (!isLive()) return;
      fs.done(polLink(out.name), creating ? t("pol.created") : t("pol.saved"));
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      fs.show(errList(e));
    }
  });

  const criteria = card(t("pol.criteria"), t("pol.criteriaSub"), form);
  if (creating) {
    put(view, pageHead(t("pol.createTitle"), sub(t("pol.createSub")), acts(back), polEyebrow()), fs.ok,
      h("div", { class: "two" }, criteria, h("div", { class: "stack" }, cargoNote())));
    nameIn.focus();
    return;
  }

  // Connected repositories.
  const connected = cur.repositories || [];
  const conn = card(t("pol.connected"), t("pol.connectedSub"), [
    connected.length ? connected.map((r) => h("div", { class: "listrow" }, h("a", { href: adminRepoLink(r) }, r), badge(t("pol.applied"), "blue")))
      : h("div", { class: "empty" }, t("pol.noRepos")),
    h("p", { class: "hint" }, t("pol.connectHint"))]);

  // Preview: every connected repository, one of them, or (none connected) one eligible repository.
  const eligible = repos.filter((r) => cleanable(r) && matches(cur.format, r.format)).map((r) => r.name);
  const choices = connected.length > 1 ? [["", t("pol.prevAll", { n: connected.length })]].concat(connected.map((r) => [r, r]))
    : connected.length ? [[connected[0], connected[0]]] : eligible.map((r) => [r, r]);
  const repoSel = choices.length > 1 ? sel(choices, choices[0][0]) : null;
  const runBtn = button(t("pol.preview"), connected.length ? null : "primary");
  runBtn.disabled = !choices.length;
  const result = h("div", { class: "pv-result", "aria-live": "polite" },
    choices.length ? h("p", { class: "hint" }, connected.length ? t("pol.prevIdle") : t("pol.prevPick")) : h("div", { class: "notice" }, t("pol.prevNoRepo")));
  let pseq = 0;
  const runPreview = async () => {
    const repo = repoSel ? repoSel.value : choices[0][0];
    const n = ++pseq;
    runBtn.disabled = true;
    put(result, loading());
    try {
      const d = await api("cleanup/policies/" + encSeg(cur.name) + "/preview" + (repo ? "?repo=" + enc(repo) : ""));
      if (!isLive() || n !== pseq) return;
      const list = button(t("pol.list"), "primary");
      list.disabled = !(d.components || []).length;
      list.addEventListener("click", () => previewModal(cur.name, d));
      put(result,
        h("div", { class: "pv-count" }, fmtNum(d.count), h("small", null, t("pol.prevUnit", { n: d.count, size: fmtBytes(d.bytes) }))),
        h("p", { class: "tiny muted" }, repo ? t("pol.prevOf", { repo }) : t("pol.prevOfAll")),
        d.truncated ? h("div", { class: "notice amber" }, t("pol.prevTrunc")) : null,
        h("div", { class: "actions" }, list));
    } catch (e) {
      if (!isLive() || n !== pseq) return;
      put(result, h("div", { class: "notice amber" }, e.message));
      if (e.status === 401 || e.status === 403) handleError(e);
    } finally {
      if (n === pseq) runBtn.disabled = false;
    }
  };
  runBtn.addEventListener("click", runPreview);
  const preview = card(t("pol.previewTitle"), t("pol.previewSub"), [
    repoSel ? field(t("col.repo"), repoSel) : null,
    h("div", { class: "actions" }, runBtn), result,
    h("p", { class: "tiny muted pv-foot" }, t("pol.compactNote"))]);

  const del = button(t("common.delete"), "danger");
  del.addEventListener("click", async () => {
    if (!(await confirmDelete(t("pol.delText", { name: cur.name }), t("pol.delTitle")))) return;
    del.disabled = true;
    try {
      await api("cleanup/policies/" + encSeg(cur.name), { method: "DELETE" });
    } catch (e) {
      del.disabled = false;
      if (e.status === 409 && e.data && Array.isArray(e.data.repositories)) showError(t("pol.inUse", { repos: e.data.repositories.join(", ") }));
      else handleError(e);
      return;
    }
    if (isLive()) location.hash = polLink();
  });

  put(view,
    pageHead(cur.name, h("p", null, polFormat(cur.format), " ", t("pol.detailSub")), acts(back, del), polEyebrow()),
    fs.ok,
    h("div", { class: "two" }, h("div", { class: "stack" }, criteria, conn), h("div", { class: "stack" }, preview, cargoNote())));
  if (connected.length) runPreview();
}

const cargoNote = () => h("div", { class: "notice" }, icon("info", "hint-ico"), t("pol.cargoNote"));

// ---- scheduled tasks ----------------------------------------------------------------

const taskLink = (id) => "#admin/system/tasks" + (id ? ":" + enc(id) : "");
const CLEANUP = "repository.cleanup", REBUILD = "repository.rebuild-metadata", COMPACT = "blobstore.compact";
const TASK_STATUS = { Running: ["task.running", "blue"], Waiting: ["task.waiting"], Disabled: ["task.disabled"] };
const taskStatus = (x) => (TASK_STATUS[x.status] ? badge(t(TASK_STATUS[x.status][0]), TASK_STATUS[x.status][1]) : badge(x.status));
const RESULTS = { OK: ["task.ok", "green"], Failed: ["task.failed", "red"], Stopped: ["task.stopped", "amber"] };
const resultText = (x) => (RESULTS[x.lastResult] ? t(RESULTS[x.lastResult][0]) : x.lastResult || dash);
const resultBadge = (x) => (x.lastResult ? badge(resultText(x), (RESULTS[x.lastResult] || [])[1]) : badge(t("task.never")));
const taskEyebrow = () => ({ eyebrow: t("task.eyebrow") });
const typeLabel = (type, fallback) => ({ [COMPACT]: t("task.tCompact"), [CLEANUP]: t("task.tCleanup"), [REBUILD]: t("task.tRebuild") }[type] || fallback || type);

// Schedules (plan 11 Decision 7), in server time.
const WEEKDAYS = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"];
const wdName = (i) => new Date(Date.UTC(2024, 0, 7 + i)).toLocaleDateString(locale(), { weekday: "long", timeZone: "UTC" });
function parseSchedule(sch) {
  const p = String(sch || "manual").split(":");
  const k = p[0].toLowerCase();
  const hm = (i) => (p[i] !== undefined && p[i + 1] !== undefined ? pad2(p[i]) + ":" + pad2(p[i + 1]) : "");
  switch (k) {
    case "once": return { kind: k, at: String(sch).slice(5, 21) };
    case "hourly": return { kind: k, minute: Number(p[1]) || 0 };
    case "daily": return { kind: k, time: hm(1) };
    case "weekly": return { kind: k, wd: WEEKDAYS.indexOf(p[1]), time: hm(2) };
    case "monthly": return { kind: k, day: Number(p[1]) || 1, time: hm(2) };
  }
  return { kind: "manual" };
}
export function scheduleText(sch) {
  const p = parseSchedule(sch);
  switch (p.kind) {
    case "manual": return t("task.manual");
    case "once": return t("sch.singleText", { at: p.at.replace("T", " ") });
    case "hourly": return t("sch.hourlyText", { m: pad2(p.minute) });
    case "daily": return t("sch.dailyText", { time: p.time });
    case "weekly": return t("sch.weeklyText", { day: p.wd >= 0 ? wdName(p.wd) : "?", time: p.time });
    case "monthly": return t("sch.monthlyText", { d: p.day, time: p.time });
  }
  return String(sch);
}

// scheduleEditor is a kind select plus the inputs that kind uses; value()
// builds the schedule string.
function scheduleEditor(sch) {
  const p = parseSchedule(sch);
  const kind = sel([["manual", t("task.manual")], ["once", t("sch.single")], ["hourly", t("sch.hourly")], ["daily", t("sch.daily")],
    ["weekly", t("sch.weekly")], ["monthly", t("sch.monthly")]], p.kind);
  const time = h("input", { type: "time", value: p.time || "03:00" });
  const minute = numIn(p.minute || 0, 0, 59);
  const wd = sel(WEEKDAYS.map((w, i) => [w, wdName(i)]), WEEKDAYS[p.wd >= 0 ? p.wd : 0]);
  const day = numIn(p.day || 1, 1, 28);
  const at = h("input", { type: "datetime-local", value: p.at || "" });
  // An untouched once time keeps the stored string (with its offset);
  // a new one is sent offset-less, which the server reads as its local time.
  const origOnce = p.kind === "once" ? String(sch) : "";
  const code = h("code", null);
  const f = {
    time: field(t("sch.time"), time), minute: field(t("sch.minute"), minute, t("sch.minuteHelp")),
    wd: field(t("sch.weekday"), wd), day: field(t("sch.day"), day, t("sch.dayHelp")), at: field(t("sch.at"), at, t("sch.atHelp")),
  };
  const uses = { manual: [], once: ["at"], hourly: ["minute"], daily: ["time"], weekly: ["wd", "time"], monthly: ["day", "time"] };
  const value = () => {
    const tm = time.value.slice(0, 5);
    switch (kind.value) {
      case "once": return origOnce && at.value === p.at ? origOnce : `once:${at.value.slice(0, 16)}`;
      case "hourly": return "hourly:" + pad2(minute.value.trim());
      case "daily": return "daily:" + tm;
      case "weekly": return "weekly:" + wd.value + ":" + tm;
      case "monthly": return "monthly:" + String(day.value).trim() + ":" + tm;
    }
    return "manual";
  };
  const sync = () => {
    for (const [k, el] of Object.entries(f)) el.hidden = !uses[kind.value].includes(k);
    code.textContent = value();
  };
  const el = h("div", { class: "sched" }, fields(field(t("sch.kind"), kind, t("sch.tzHelp")), f.at, f.minute, f.wd, f.day, f.time),
    h("p", { class: "hint" }, t("sch.string"), " ", code));
  el.addEventListener("input", sync);
  el.addEventListener("change", sync);
  sync();
  return { el, value };
}

export async function viewTasks(view, id) {
  if (id !== undefined) return taskForm(view, id === "create" ? null : id);
  const isLive = alive();
  const tasks = (await api("tasks")).tasks || [];
  if (!isLive()) return;
  put(view,
    pageHead(t("nav.tasks"), sub(t("task.sub")), acts(h("a", { class: "btn primary", href: taskLink("create") }, t("task.create"))), taskEyebrow()),
    h("section", { class: "card" }, tbl(
      [t("task.task"), t("col.status"), t("task.schedule"), t("task.next"), t("task.lastResult"), actHead()],
      tasks.map((x) => h("tr", null,
        h("td", { class: "name" }, h("a", { href: taskLink(x.id) }, h("strong", null, taskName(x))), h("div", { class: "tiny muted" }, typeLabel(x.type, x.typeName))),
        h("td", null, taskStatus(x)),
        h("td", null, scheduleText(x.schedule)),
        h("td", { class: "nowrap" }, x.nextRun ? fmtDate(x.nextRun) : dash),
        h("td", null, resultBadge(x), x.lastRun ? h("div", { class: "tiny muted" }, fmtDate(x.lastRun)) : null),
        h("td", { class: "act" }, h("a", { class: "btn small", href: taskLink(x.id) }, t("common.details"))))),
      t("task.none"))),
    h("div", { class: "notice" }, t("task.tzNote")));
}

// paramsEditor gives the type's parameter controls and how to read them.
function paramsEditor(type, params, repos, policyOf) {
  params = params || {};
  if (type === CLEANUP) {
    const list = repos.filter(cleanable);
    const picked = new Set(params.repos || []);
    const m = h("select", { multiple: true, size: Math.min(8, Math.max(3, list.length)), class: "rf-list" },
      list.map((r) => {
        const o = h("option", { value: r.name }, r.name + " · " + (policyOf[r.name] || t("task.noPolicy")));
        o.selected = picked.has(r.name);
        return o;
      }));
    return { el: field(t("task.repos"), m, t("task.reposHelp"), { full: true }), get: () => ({ repos: [...m.selectedOptions].map((o) => o.value) }) };
  }
  if (type === REBUILD) {
    const list = repos.filter((r) => r.format === "maven" && (r.type === "hosted" || r.type === "proxy")).map((r) => [r.name, r.name + " · " + r.type]);
    const cur = params.repo || "";
    if (cur && !list.some(([n]) => n === cur)) list.unshift([cur, cur]);
    const one = sel([["", t("task.pickRepo")]].concat(list), cur);
    return { el: field(t("col.repo"), one, t("task.rebuildHelp"), { req: true, full: true }), get: () => ({ repo: one.value }) };
  }
  return { el: h("div", { class: "notice" }, type === COMPACT ? t("task.compactAbout") : t("task.noParams")), get: () => ({}) };
}

// logLines shows a run log ("<RFC3339> LEVEL message" lines) in the concept's .log block.
function logLines(text) {
  const lines = String(text || "").split("\n").filter((x) => x !== "");
  if (!lines.length) return h("div", { class: "log short" }, h("div", null, t("task.noLog")));
  return h("div", { class: "log", role: "log", tabindex: "0", "aria-label": t("task.logTitle") }, lines.map((l) => {
    const m = /^\S+ (INFO|WARN|ERROR)\b/.exec(l);
    return h("div", { class: (m && logClass(m[1])) || (/ finished: OK\b/.test(l) ? "success" : null) }, l);
  }));
}

async function taskForm(view, id) {
  const isLive = alive();
  const creating = id === null;
  const none = () => null;
  const [list, got, repos, pols] = await Promise.all([
    api("tasks"),
    creating ? null : api("tasks/" + encSeg(id)).catch((e) => { if (e.status === 404) return none(); throw e; }),
    loadRepos(true),
    api("cleanup/policies").catch(none),
  ]);
  if (!isLive()) return;
  const back = h("a", { class: "btn", href: taskLink() }, t("task.back"));
  if (!creating && !got) {
    put(view, pageHead(t("nav.tasks"), null, acts(back), taskEyebrow()), notFound(t("task.notFound", { id })));
    return;
  }
  let task = got;
  const policyOf = {};
  for (const p of (pols && pols.policies) || []) for (const r of p.repositories || []) policyOf[r] = p.name;
  const types = (list.types || []).map((x) => [x.type, typeLabel(x.type, x.name)]);

  const nameIn = textIn(task ? task.name : "", { required: true, maxlength: 100 });
  const typeSel = creating ? sel(types, CLEANUP) : textIn(typeLabel(task.type, task.typeName), { readonly: true });
  const sched = scheduleEditor(task ? task.schedule : "daily:02:00");
  const enabled = h("input", { type: "checkbox", checked: task ? !!task.enabled : true });
  const notify = h("textarea", { rows: 3, spellcheck: "false", autocomplete: "off", placeholder: "ops@example.com", maxlength: 2000 }, task && task.notifyEmail ? task.notifyEmail.join("\n") : "");
  const paramsBox = h("div", null);
  let params = null;
  const setParams = () => {
    params = paramsEditor(creating ? typeSel.value : task.type, task ? task.params : null, repos, policyOf);
    put(paramsBox, params.el);
  };
  setParams();
  if (creating) typeSel.addEventListener("change", setParams);
  const save = h("button", { type: "submit", class: "btn primary" }, creating ? t("task.createBtn") : t("task.save"));
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  put(form, fs.err,
    fields(field(t("task.name"), nameIn, null, { req: true }), field(t("col.type"), typeSel)),
    h("h3", { class: "sub-title" }, t("task.schedule")), sched.el,
    h("h3", { class: "sub-title" }, t("task.params")), paramsBox,
    h("div", { class: "field" }, h("label", { class: "check" }, enabled, t("task.enabled")), h("small", null, t("task.enabledHelp"))),
    fields(field(t("task.notify"), notify, t("task.notifyHelp"), { full: true })),
    h("div", { class: "actions end" }, save));

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const b = { name: nameIn.value.trim(), schedule: sched.value(), params: params.get(), enabled: enabled.checked,
      notifyEmail: notify.value.split(/[,;\s]+/).map((x) => x.trim()).filter(Boolean) };
    if (creating) b.type = typeSel.value;
    const errs = [];
    if (!b.name) errs.push(t("task.e.name"));
    if ((creating ? b.type : task.type) === REBUILD && !b.params.repo) errs.push(t("task.e.repo"));
    if (/^once:$/.test(b.schedule)) errs.push(t("task.e.when"));
    if (errs.length) { fs.show(errs); return; }
    save.disabled = true;
    try {
      const out = creating ? await api("tasks", { method: "POST", body: b }) : await api("tasks/" + encSeg(task.id), { method: "PUT", body: b });
      if (!isLive()) return;
      fs.done(taskLink(out.id), creating ? t("task.created") : t("task.saved"));
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      fs.show(errList(e));
    }
  });

  const settings = card(t("task.settings"), t("task.settingsSub"), form);
  if (creating) {
    put(view, pageHead(t("task.createTitle"), sub(t("task.createSub")), acts(back), taskEyebrow()), fs.ok, h("div", { class: "two" }, settings, h("div", null)));
    nameIn.focus();
    return;
  }

  // Summary, last run log, run now and delete.
  const summary = h("div", null);
  const logBox = h("div", null);
  const run = h("button", { type: "button", class: "btn primary" }, t("task.run"));
  const stop = button(t("task.stop"), "danger");
  const msg = h("p", { class: "task-msg", role: "status" });
  // A rebuild of a Maven proxy removes cached files, not metadata.
  const purge = task.type === REBUILD && repos.some((r) => r.name === (task.params || {}).repo && r.type === "proxy");
  const removedKey = purge ? "task.removedFiles" : { [CLEANUP]: "task.removedComp", [REBUILD]: "task.removedMeta" }[task.type] || "task.removed";
  const show = () => {
    put(summary,
      kv(t("col.status"), taskStatus(task)),
      kv(t("task.lastResult"), resultBadge(task)),
      kv(t("task.next"), task.nextRun ? fmtDate(task.nextRun) : dash),
      kv(t("task.schedule"), scheduleText(task.schedule)),
      kv(t("task.notify"), (task.notifyEmail || []).length ? task.notifyEmail.join(", ") : dash),
      kv(t("task.lastRun"), task.lastRun ? fmtDate(task.lastRun) : dash),
      task.lastRun ? kv(t("task.duration"), fmtNum(task.lastDurationMs) + " ms") : null,
      task.lastRun ? kv(t(removedKey), fmtNum(task.lastRemoved)) : null,
      task.lastRun && task.type === CLEANUP ? kv(t("task.bytes"), fmtBytes(task.lastBytes)) : null,
      task.lastMessage ? kv(t("task.message"), task.lastMessage) : null);
    run.disabled = task.status === "Running";
    stop.hidden = task.status !== "Running";
  };
  const showLog = () => put(logBox, logLines(task.lastLog));
  let timer = 0;
  state.leaving.push(() => clearTimeout(timer));
  const sleep = (ms) => new Promise((res) => { timer = setTimeout(res, ms); });
  const done = () => t("task.result", { result: resultText(task) }) +
    (task.lastRun ? " — " + t("task.resultDetail", { n: fmtNum(task.lastRemoved), ms: fmtNum(task.lastDurationMs) }) : "");

  // track polls the task list (every 2 s, 10 minutes at most) until the run
  // that started (started: the POST promise, which answers 202 at once; null
  // for a run already going when the page loaded) has finished, then loads
  // the task again for its log. A 409 means another task is running: this
  // one did not start, so it is not followed.
  async function track(started) {
    run.disabled = true;
    msg.textContent = t("task.runningMsg");
    let settled = !started, failed = null;
    if (started) {
      started.catch((e) => { failed = e; }).finally(() => { settled = true; });
    }
    const until = Date.now() + 600000;
    for (;;) {
      await sleep(settled ? 2000 : 500);
      if (!isLive()) return;
      if (failed && failed.status === 409) {
        const d = failed.data || {};
        msg.textContent = t("task.conflict", { name: d.runningName || d.running || "?" });
        break;
      }
      if (failed) {
        msg.textContent = t("task.runFailed", { msg: failed.message });
        handleError(failed);
        break;
      }
      try {
        const x = ((await api("tasks")).tasks || []).find((y) => y.id === task.id);
        if (!isLive()) return;
        if (x) { task = Object.assign({}, task, x); show(); }
      } catch (e) {
        if (!isLive()) return;
        handleError(e);
        break;
      }
      if (settled && task.status !== "Running") {
        try {
          const full = await api("tasks/" + encSeg(task.id));
          if (!isLive()) return;
          task = full;
          show();
          showLog();
        } catch (e) { if (isLive()) handleError(e); }
        msg.textContent = done();
        break;
      }
      if (Date.now() > until) {
        msg.textContent = t("task.slow");
        break;
      }
    }
    if (isLive()) run.disabled = task.status === "Running";
  }
  run.addEventListener("click", async () => {
    if (task.type === CLEANUP && !(await confirmDelete(t("task.runCleanupText"), t("task.runCleanupTitle"), t("task.run")))) return;
    if (isLive()) track(api("tasks/" + encSeg(task.id) + "/run", { method: "POST" }));
  });
  stop.addEventListener("click", async () => {
    stop.disabled = true;
    try {
      await api("tasks/" + encSeg(task.id) + "/stop", { method: "POST" });
      if (isLive()) msg.textContent = t("task.stopping");
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 409) msg.textContent = t("task.notRunning");
      else handleError(e);
    } finally {
      stop.disabled = false;
    }
  });

  const del = button(t("common.delete"), "danger");
  del.addEventListener("click", async () => {
    if (!(await confirmDelete(t("task.delText", { name: taskName(task) }), t("task.delTitle")))) return;
    del.disabled = true;
    try {
      await api("tasks/" + encSeg(task.id), { method: "DELETE" });
    } catch (e) {
      del.disabled = false;
      if (e.status === 409) showError(t("task.delRunning"));
      else handleError(e);
      return;
    }
    if (isLive()) location.hash = taskLink();
  });

  show();
  showLog();
  put(view,
    pageHead(taskName(task), sub(typeLabel(task.type, task.typeName)), acts(back, del, stop, run), taskEyebrow()),
    fs.ok,
    h("div", { class: "two" },
      h("div", { class: "stack" }, settings),
      h("div", { class: "stack" },
        card(t("task.summary"), t("task.summarySub"), [summary, msg]),
        card(t("task.logTitle"), t("task.logSub"), logBox))));
  if (task.status === "Running") track(null); // a run started elsewhere: follow it to its end
}
