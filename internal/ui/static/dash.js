// Dashboard: four summary tiles and repository operations, with real API data.
// The former trendCard(isLive, "7d") and api("events?limit=6") sections were
// removed in the clean UI redesign; trends and logs remain on Status & Logs.
import {
  state, h, put, api, alive, uploadLink, canUpload, fmtBytes, fmtNum, FORMATS, FORMAT_NAMES,
  pageHead, loading, stat, icon,
} from "./util.js";
import { t } from "./i18n.js";
import { repositoryStatsCard } from "./ops.js";

export async function viewWelcome(view) {
  const isLive = alive();
  const me = state.me;
  const statsBox = h("div", { class: "stats" }, loading());
  const attn = h("div", { hidden: true });
  const summary = h("p");
  const act = h("div", { class: "actions" });
  put(view, pageHead(t("dash.title"), summary, act), statsBox, attn,
    me.admin ? repositoryStatsCard(isLive) : null);

  const none = () => null;
  const [d, st, checks] = await Promise.all([
    api("repositories"),
    me.admin ? api("status").catch(none) : null,
    me.admin ? api("status/checks").catch(none) : null,
  ]);
  if (!isLive()) return;
  const list = d.repositories || [];
  state.repos = list;
  if (list.some(canUpload)) {
    put(act, h("a", { class: "btn primary", href: uploadLink() }, icon("upload"), t("nav.upload")));
  }
  const own = list.filter((r) => r.type !== "group");
  const files = own.reduce((n, r) => n + (Number(r.assets) || 0), 0);
  const bytes = own.reduce((n, r) => n + (Number(r.bytes) || 0), 0);
  const nOnline = list.filter((r) => r.online !== false).length;
  const types = ["hosted", "proxy", "group"].map((ty) => [ty, list.filter((r) => r.type === ty).length]).filter(([, n]) => n);
  summary.textContent = t("dash.summary", { repos: fmtNum(list.length), files: fmtNum(files), size: fmtBytes(bytes) });
  const blobBytes = st && typeof st.blobBytes === "number" ? st.blobBytes : bytes;
  const failing = checks ? (checks.checks || []).filter((c) => !c.healthy) : [];
  const nChecks = checks ? (checks.checks || []).length : 0;
  const formats = FORMATS.filter((f) => list.some((r) => r.format === f));
  put(statsBox,
    stat({ label: t("dash.files"), value: fmtNum(files), foot: t("dash.repoCount", { n: fmtNum(own.length) }) + " · " + t("dash.filesFoot") }),
    stat({ label: t("dash.blob"), value: fmtBytes(blobBytes),
      foot: st ? t("dash.blobs", { n: fmtNum(st.blobs) }) + " · " + t("dash.dedup") : t("dash.blobVisible") }),
    stat({ label: t("dash.repos"), value: fmtNum(list.length),
      foot: types.map(([ty, n]) => ty + " " + n).join(" · "), right: t("dash.repoStatus", { on: fmtNum(nOnline), off: fmtNum(list.length - nOnline) }) }),
    me.admin
      ? stat({ label: t("dash.health"), value: [h("span", { class: "dot " + (!checks ? "" : failing.length ? "err" : "ok"), "aria-hidden": "true" }),
        checks ? (failing.length ? t("dash.healthBad") : t("dash.healthOk")) : t("dash.healthNa")],
        foot: checks ? t("dash.checks", { ok: nChecks - failing.length, n: nChecks }) : "", right: st ? "RSS " + fmtBytes(st.rssBytes) : "" })
      : stat({ label: t("dash.formats"), value: fmtNum(formats.length), foot: formats.map((f) => FORMAT_NAMES[f]).join(" · ") }));
  if (failing.length) {
    put(attn, h("div", { class: "attention", role: "status" },
      h("div", null, h("strong", null, t("dash.attention", { n: failing.length })),
        h("p", null, failing.map((c) => t("dash.attnCheck", { name: c.name })).join(" · "))),
      h("a", { class: "btn small", href: "#admin/support/status" }, t("dash.attnGo"))));
    attn.hidden = false;
  }
}
