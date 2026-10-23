// Dashboard (#browse/welcome), plan 10 Decision 8 and plan 11 Decision 8:
// stats, an attention bar, the request trend and recent activity (admins),
// the major repositories, storage per format and quick links. Only real API
// data is shown; there are no limits.
import {
  state, h, put, api, alive, browseLink, uploadLink, canUpload, fmtBytes, fmtNum, fmtDate, FORMATS, FORMAT_NAMES,
  pageHead, loading, card, stat, bar, tbl, nameCell, onlineBadge, icon, taskName,
} from "./util.js";
import { t } from "./i18n.js";
import { trendCard } from "./ops.js";

// Recent activity (plan 11 Decision 3): each event kind has its own label.
const EV = {
  upload: "ev.upload", delete: "ev.delete", "repo.create": "ev.repoCreate", "repo.update": "ev.repoUpdate", "repo.delete": "ev.repoDelete",
  "token.create": "ev.tokenCreate", "token.revoke": "ev.tokenRevoke", "task.create": "ev.taskCreate", "task.update": "ev.taskUpdate",
  "task.delete": "ev.taskDelete", cleanup: "ev.cleanup", "policy.create": "ev.policyCreate", "policy.update": "ev.policyUpdate",
  "policy.delete": "ev.policyDelete",
};

// storeRows is one row per blob store: used and free space with a meter
// (used share of the filesystem, or of all stores when the size is unknown).
function storeRows(stores) {
  const sum = stores.reduce((n, s) => n + (Number(s.usedBytes) || 0), 0);
  return h("div", { class: "store-rows" }, h("h3", { class: "card-title" }, t("dash.stores")),
    stores.map((s) => {
      const known = s.fsTotal > 0;
      const p = known ? pct(Number(s.usedBytes) || 0, s.fsTotal) : pct(Number(s.usedBytes) || 0, sum);
      return h("div", { class: "store-row" },
        h("div", { class: "listrow" }, h("a", { href: "#admin/repository/blobstores:" + encodeURIComponent(s.name) }, h("strong", null, s.name)),
          h("span", { class: "tiny muted" }, t("dash.storeUsed", { size: fmtBytes(s.usedBytes || 0) }))),
        bar(p, known && p > 70 ? "amber" : null),
        h("div", { class: "tiny muted" }, known ? t("dash.storeFree", { size: fmtBytes(s.fsFree || 0) }) : t("dash.storeUnknown")));
    }));
}

function eventItem(e, taskNames) {
  const byTask = e.kind.startsWith("task.") || e.kind === "cleanup";
  const subject = byTask ? taskNames[e.subject] || e.subject : e.subject;
  const failed = e.kind === "task.run" && !/^OK\b/.test(e.detail || "");
  const label = e.kind === "task.run" ? t(failed ? "ev.taskFailed" : "ev.taskOk", { subject })
    : EV[e.kind] ? t(EV[e.kind], { subject }) : e.kind + " · " + subject;
  const m = e.kind === "cleanup" ? /components=(\d+).*bytes=(\d+)/.exec(e.detail || "") : null;
  return h("div", { class: "event" + (failed ? " bad" : "") }, label,
    h("small", null, [m ? t("ev.cleanupDetail", { n: fmtNum(m[1]), size: fmtBytes(Number(m[2])) }) : null,
      e.repo && e.repo !== e.subject ? e.repo : null, e.actor || null, fmtDate(e.at)].filter(Boolean).join(" · ")));
}

function timeline(evs, tasks) {
  if (!evs) return h("div", { class: "empty" }, t("dash.activityFailed"));
  const names = {};
  for (const x of (tasks && tasks.tasks) || []) names[x.id] = taskName(x);
  const list = (evs.events || []).slice(0, 6);
  return list.length ? h("div", { class: "timeline" }, list.map((e) => eventItem(e, names))) : h("div", { class: "empty" }, t("dash.noActivity"));
}

const pct = (n, total) => (total > 0 ? (n / total) * 100 : 0);
const pctText = (p) => (p >= 10 || p === 0 ? Math.round(p) : p.toFixed(1));

function quick(href, title, sub) {
  return h("a", { class: "quick", href }, h("span", null, h("strong", null, title), h("small", null, sub)),
    h("span", { class: "muted", "aria-hidden": "true" }, "→"));
}

export async function viewWelcome(view) {
  const isLive = alive();
  const me = state.me;
  const statsBox = h("div", { class: "stats" }, loading());
  const attn = h("div", { hidden: true });
  const major = h("div", null, loading());
  const storage = h("div", null, loading());
  const act = h("div", { class: "actions" });
  const head = pageHead(t("dash.title"), h("p", null, t("dash.sub")), act, { eyebrow: t("dash.eyebrow") });
  const majorCard = card(t("dash.major"), t("dash.majorSub"), null, h("a", { class: "btn small", href: "#browse/browse" }, t("dash.viewAll")));
  majorCard.appendChild(major);
  const storageCard = card(t("dash.storage"), t("dash.storageSub"), storage);
  const activity = h("div", null, loading());
  put(view, head, statsBox, attn,
    me.admin
      ? h("div", { class: "two" },
        h("div", { class: "stack" }, trendCard(isLive, "7d", t("dash.trend"), t("dash.trendSub")), majorCard),
        h("div", { class: "stack" }, storageCard, card(t("dash.activity"), t("dash.activitySub"), activity,
          h("a", { class: "btn small", href: "#admin/support/status" }, t("dash.logs")))))
      : h("div", { class: "two" }, majorCard, storageCard),
    h("div", { class: "quicklinks" },
      quick("#browse/browse", t("nav.browse"), t("quick.browse")),
      quick("#browse/search/generic", t("nav.search"), t("quick.search")),
      me.admin ? quick("#admin/repository/cleanuppolicies", t("nav.cleanup"), t("quick.cleanup")) : null,
      me.admin ? quick("#admin/support/status", t("nav.status"), t("quick.status"))
        : me.anonymous ? null : quick("#user/usertoken", t("nav.tokens"), t("quick.tokens"))));

  const none = () => null;
  const [d, st, checks, tasks, evs, stores] = await Promise.all([
    api("repositories"),
    me.admin ? api("status").catch(none) : null,
    me.admin ? api("status/checks").catch(none) : null,
    me.admin ? api("tasks").catch(none) : null,
    me.admin ? api("events?limit=6").catch(none) : null,
    me.admin ? api("blobstores").catch(none) : null,
  ]);
  if (!isLive()) return;
  if (me.admin) put(activity, timeline(evs, tasks));
  const list = d.repositories || [];
  state.repos = list;
  if (list.some(canUpload)) {
    put(act, h("a", { class: "btn primary", href: uploadLink() }, icon("upload"), t("nav.upload")));
  }
  const own = list.filter((r) => r.type !== "group"); // group stats repeat their members
  const files = own.reduce((n, r) => n + (Number(r.assets) || 0), 0);
  const bytes = own.reduce((n, r) => n + (Number(r.bytes) || 0), 0);
  const nOnline = list.filter((r) => r.online !== false).length;
  const types = ["hosted", "proxy", "group"].map((ty) => [ty, list.filter((r) => r.type === ty).length]).filter(([, n]) => n);

  // Stats: files, blob storage, repositories online, system health (admins)
  // or the formats in use (everyone else).
  const blobBytes = st && typeof st.blobBytes === "number" ? st.blobBytes : bytes;
  const failing = checks ? (checks.checks || []).filter((c) => !c.healthy) : [];
  const nChecks = checks ? (checks.checks || []).length : 0;
  const formats = FORMATS.filter((f) => list.some((r) => r.format === f));
  put(statsBox,
    stat({ label: t("dash.files"), value: fmtNum(files), foot: t("dash.filesFoot"), right: t("dash.repoCount", { n: fmtNum(own.length) }) }),
    stat({ label: t("dash.blob"), value: fmtBytes(blobBytes),
      foot: st ? t("dash.blobs", { n: fmtNum(st.blobs) }) : t("dash.blobVisible"), right: st ? t("dash.dedup") : "" }),
    stat({ label: t("dash.repos"), value: fmtNum(list.length), unit: t("dash.unitOnline"), pct: pct(nOnline, list.length), color: nOnline === list.length ? "green" : "amber",
      foot: types.map(([ty, n]) => ty + " " + n).join(" · "), right: t("dash.repoStatus", { on: fmtNum(nOnline), off: fmtNum(list.length - nOnline) }) }),
    me.admin
      ? stat({ label: t("dash.health"), value: checks ? (failing.length ? t("dash.healthBad") : t("dash.healthOk")) : t("dash.healthNa"),
        pct: checks ? pct(nChecks - failing.length, nChecks) : 0, color: failing.length || !checks ? "amber" : "green",
        foot: checks ? t("dash.checks", { ok: nChecks - failing.length, n: nChecks }) : "", right: st ? "RSS " + fmtBytes(st.rssBytes) : "" })
      : stat({ label: t("dash.formats"), value: fmtNum(formats.length), foot: formats.map((f) => FORMAT_NAMES[f]).join(" · ") }));

  // Attention: failing health checks and failed tasks (admins).
  const failedTasks = tasks ? (tasks.tasks || []).filter((x) => x.lastResult === "Failed") : [];
  const items = failing.map((c) => t("dash.attnCheck", { name: c.name })).concat(failedTasks.map((x) => t("dash.attnTask", { name: taskName(x) })));
  if (items.length) {
    put(attn, h("div", { class: "attention", role: "status" },
      h("div", null, h("strong", null, t("dash.attention", { n: items.length })), h("p", null, items.join(" · "))),
      h("a", { class: "btn small", href: failing.length ? "#admin/support/status" : "#admin/system/tasks" }, t("dash.attnGo"))));
    attn.hidden = false;
  }

  // Major repositories: the six with the most files.
  const top = list.slice().sort((a, b) => (Number(b.assets) || 0) - (Number(a.assets) || 0) || a.name.localeCompare(b.name)).slice(0, 6);
  put(major, tbl([t("col.repo"), t("col.status"), { t: t("col.files"), cls: "num" }, { t: t("col.size"), cls: "num" }, { t: h("span", { class: "sr-only" }, t("col.open")), cls: "act" }],
    top.map((r) => {
      const tr = h("tr", { class: "clickable" },
        h("td", null, nameCell(r.format, h("a", { href: browseLink(r.name) }, h("strong", null, r.name)), r.format + " · " + r.type)),
        h("td", null, onlineBadge(r.online)),
        h("td", { class: "num" }, r.type === "group" ? "—" : fmtNum(r.assets)),
        h("td", { class: "num" }, r.type === "group" ? "—" : fmtBytes(r.bytes)),
        h("td", { class: "act muted", "aria-hidden": "true" }, "→"));
      tr.addEventListener("click", (ev) => { if (!ev.target.closest("a, button")) location.hash = browseLink(r.name); });
      return tr;
    }), me.anonymous ? t("repo.noneAnon") : t("repo.none")));

  // Storage per format, as a share of the visible total.
  const per = formats.map((f) => {
    const rs = own.filter((r) => r.format === f);
    return { f, bytes: rs.reduce((n, r) => n + (Number(r.bytes) || 0), 0), files: rs.reduce((n, r) => n + (Number(r.assets) || 0), 0) };
  }).filter((x) => x.files > 0).sort((a, b) => b.bytes - a.bytes);
  put(storage,
    st ? h("div", { class: "kv" }, h("span", null, t("dash.blobTotal")), h("span", null, fmtBytes(st.blobBytes) + " · " + t("dash.blobs", { n: fmtNum(st.blobs) }))) : null,
    per.length ? per.map((x) => {
      const p = pct(x.bytes, bytes);
      return h("div", { class: "store-row" },
        h("div", { class: "listrow" }, h("strong", null, FORMAT_NAMES[x.f]), h("span", { class: "tiny muted" }, fmtBytes(x.bytes))),
        bar(p),
        h("div", { class: "tiny muted" }, t("dash.share", { pct: pctText(p), n: fmtNum(x.files) })));
    }) : h("div", { class: "empty" }, t("dash.noFiles")),
    stores && (stores.blobstores || []).length ? storeRows(stores.blobstores) : null);
}
