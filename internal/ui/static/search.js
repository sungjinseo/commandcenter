// Search: the generic, custom and per-format component searches over
// /-/api/search/components, filtering live as the criteria change, with a
// drilldown to component and asset details (plan 10 Decision 8).
import {
  h, put, api, apiRepo, enc, APIError, browseLink, searchLink, fmtDate, nameCell, FORMATS, FORMAT_NAMES,
  tbl, pageHead, loading, truncNote, debounce, handleError, icon, loadRepos, state,
} from "./util.js";
import { t } from "./i18n.js";
import { componentView, assetView, deriveComp } from "./browse.js";

// kinds holds the per-format criteria (Decision 4): [hash key, label, API
// parameter]. Coordinates keep their technical names in both languages.
function kinds() {
  const nv = (nameLabel) => [["name", nameLabel || t("col.name"), "name"], ["version", t("col.version"), "version"]];
  const of = (f) => t("search.fmt", { f });
  return {
    generic: { title: t("nav.search") },
    custom: { title: t("nav.searchCustom") },
    maven: { title: of("Maven"), fmt: "maven", fields: [["groupId", "Group ID", "group"], ["artifactId", "Artifact ID", "name"],
      ["version", t("col.version"), "version"], ["baseVersion", t("search.baseVersion"), "baseVersion"], ["classifier", "Classifier", "classifier"],
      ["extension", "Extension", "extension"]] },
    npm: { title: of("npm"), fmt: "npm", fields: [["scope", "Scope", "group"]].concat(nv()) },
    pypi: { title: of("PyPI"), fmt: "pypi", fields: nv() },
    golang: { title: of("Go"), fmt: "go", fields: nv(t("search.module")) },
    cargo: { title: of("Cargo"), fmt: "cargo", fields: nv(t("search.crate")) },
  };
}

// viewSearch: #browse/search/<kind>[=<urlencoded criteria>].
export async function viewSearch(view, kind, criteria) {
  const k = kinds()[kind];
  const generic = !k.fields;
  const more = [["group", t("comp.group"), "group"], ["name", t("col.name"), "name"], ["version", t("col.version"), "version"]];
  const textFields = generic ? [["keyword", t("search.keyword"), "keyword"]].concat(more) : k.fields;
  const inputs = {};
  const field = ([key, label], full, ph) => {
    const id = "sc-" + key;
    inputs[key] = h("input", { id, type: key === "keyword" ? "search" : "text", value: criteria.get(key) || "", autocomplete: "off", placeholder: ph || null });
    return h("div", { class: "field" + (full ? " full" : "") }, h("label", { for: id }, label), inputs[key]);
  };
  const select = (id, label, opts) => {
    const el = h("select", { id }, opts);
    return [el, h("div", { class: "field" }, h("label", { for: id }, label), el)];
  };
  let fmtSel = null, repoSel = null, body;
  if (generic) {
    const repos = (await loadRepos()).filter((r) => r.type !== "group");
    const want = criteria.get("repo") || "";
    const repoNames = repos.map((r) => r.name);
    if (want && !repoNames.includes(want)) repoNames.push(want);
    let repoField, fmtField;
    [repoSel, repoField] = select("sc-repo", t("col.repo"), [h("option", { value: "" }, t("search.allRepos")),
      repoNames.map((n) => h("option", { value: n, selected: n === want }, n))]);
    [fmtSel, fmtField] = select("sc-format", t("col.format"), [h("option", { value: "" }, t("repos.allFormats")),
      FORMATS.map((f) => h("option", { value: f, selected: criteria.get("format") === f }, FORMAT_NAMES[f]))]);
    const open = kind === "custom" || more.some(([key]) => criteria.get(key));
    const moreBox = h("div", { id: "sc-more", class: "fields crit-more", hidden: !open }, more.map((x) => field(x)));
    const toggle = h("button", { type: "button", class: "link-btn", "aria-expanded": String(open), "aria-controls": "sc-more" },
      icon("chevron", "chev"), t("search.more"));
    toggle.addEventListener("click", () => {
      const o = toggle.getAttribute("aria-expanded") !== "true";
      toggle.setAttribute("aria-expanded", String(o));
      moreBox.hidden = !o;
    });
    body = [h("div", { class: "fields" }, field(textFields[0], true, t("search.keywordPh")), repoField, fmtField),
      h("div", { class: "crit-toggle" }, toggle), moreBox];
  } else {
    body = h("div", { class: "fields" }, k.fields.map((x) => field(x)));
  }
  const reset = h("button", { type: "button", class: "btn" }, t("search.reset"));
  // The submit button lets Enter search at once; typing searches after a pause.
  const form = h("form", { class: "card", role: "search", "aria-label": k.title },
    h("div", { class: "card-body" }, body, h("button", { type: "submit", class: "sr-only", tabindex: "-1" }, t("nav.search"))));
  const out = h("div", { class: "search-out" });
  put(view, pageHead(k.title, h("p", null, t("search.sub")), h("div", { class: "actions" }, reset), { eyebrow: t("search.eyebrow") }), form, out);

  // current reads the form into hash criteria.
  const current = () => {
    const p = new URLSearchParams();
    for (const [key] of textFields) {
      const v = inputs[key].value.trim();
      if (v) p.set(key, v);
    }
    if (fmtSel && fmtSel.value) p.set("format", fmtSel.value);
    if (repoSel && repoSel.value) p.set("repo", repoSel.value);
    return p;
  };

  let saved = null; // the results nodes, restored by "Search results"
  let lastCriteria = criteria; // what the shown results were produced from
  let seq = 0; // shared by searches and drilldowns: only the latest may render
  const fmtOf = (f) => (FORMATS.includes(f) ? f : "");
  const live = () => view.isConnected;

  async function run(c) {
    lastCriteria = c;
    const q = new URLSearchParams();
    const texts = [];
    for (const [key, , param] of textFields) {
      const v = (c.get(key) || "").trim();
      if (v) { q.set(param, v); texts.push(v); }
    }
    if (k.fmt) q.set("format", k.fmt);
    else {
      if (fmtOf(c.get("format"))) q.set("format", fmtOf(c.get("format")));
      if (c.get("repo")) q.set("repo", c.get("repo"));
    }
    saved = null;
    const n = ++seq;
    if (!texts.length || !texts.some((v) => v.length >= 2)) {
      put(out, h("section", { class: "card" }, h("div", { class: "empty" }, texts.length ? t("search.min") : t("search.prompt"))));
      return;
    }
    put(out, loading());
    let data;
    try {
      data = await api("search/components?" + q.toString());
    } catch (e) {
      if (!live() || n !== seq) return;
      if (e.status === 400) { put(out, h("section", { class: "card" }, h("div", { class: "empty" }, e.message))); return; }
      put(out, h("section", { class: "card" }, h("div", { class: "empty" }, t("search.failed"))));
      handleError(e);
      return;
    }
    if (!live() || n !== seq) return;
    const comps = data.components || [];
    const rowOf = (comp) => {
      const nameBtn = h("button", { type: "button", class: "link-btn" }, h("strong", null, comp.name));
      nameBtn.addEventListener("click", () => drill(comp));
      const tr = h("tr", { class: "clickable" },
        h("td", { class: "name" }, nameCell(comp.format, nameBtn, comp.group || null)),
        h("td", null, h("span", { class: "badge blue" }, comp.version)),
        h("td", null, h("a", { href: browseLink(comp.repo) }, comp.repo)),
        h("td", { class: "nowrap" }, fmtDate(comp.lastUpdated)),
        h("td", { class: "act muted", "aria-hidden": "true" }, "→"));
      tr.addEventListener("click", (ev) => { if (!ev.target.closest("a, button")) drill(comp); });
      return tr;
    };
    const table = tbl([t("comp.component"), t("col.version"), t("col.repo"), t("col.updated"), { t: h("span", { class: "sr-only" }, t("col.open")), cls: "act" }],
      comps.map(rowOf), t("search.none"));
    // Pages of 100 (QA L-23): "more" appends the next page while the API
    // returns a continuationToken; the note shows once the last page says
    // the search reached its limits.
    const count = h("span", { class: "small muted", role: "status" });
    const more = h("button", { type: "button", class: "btn" }, t("search.loadMore"));
    const foot = h("div", null, h("div", { class: "toolbar end" }, more));
    let shown = comps.length, token = "";
    const tail = (d) => {
      token = d.continuationToken || "";
      count.textContent = t("search.count", { n: shown + (d.truncated ? "+" : "") });
      if (!token) put(foot, d.truncated ? truncNote(t("search.truncated")) : null);
    };
    more.addEventListener("click", async () => {
      more.disabled = true;
      let d;
      try {
        d = await api("search/components?" + q.toString() + "&continuationToken=" + enc(token));
      } catch (e) {
        more.disabled = false;
        if (more.isConnected) handleError(e);
        return;
      }
      const rows = (d.components || []).map(rowOf);
      table.querySelector("tbody").append(...rows);
      shown += rows.length;
      more.disabled = false;
      tail(d);
      // The last page removes the focused button: keep focus in the results.
      if (!token && rows.length) rows[0].querySelector("button").focus();
    });
    tail(data);
    saved = [
      h("section", { class: "card" },
        h("div", { class: "toolbar" }, count, h("span", { class: "end" }, t("search.pick"))), table),
      foot,
    ];
    put(out, saved);
  }

  const backBtn = () => {
    const b = h("button", { type: "button", class: "link-btn" }, "← " + t("search.results"));
    b.addEventListener("click", () => { if (saved) put(out, saved); else run(current()); });
    return b;
  };
  const sep = () => h("span", { class: "sep" }, "/");
  const label = (comp) => comp.name + " " + comp.version;
  const focusHead = () => { const x = out.querySelector(".componenthero h2"); if (x) x.focus(); };

  async function repoOf(comp) {
    const r = (await loadRepos()).find((x) => x.name === comp.repo);
    return r || { name: comp.repo, format: comp.format, type: "hosted", canWrite: false, url: "" };
  }

  // folderOf maps a component to the folder that holds all of its files, or
  // "" when the format has no such folder. Maven deletes file by file, and a
  // Maven component lives in group/artifact/version/.
  const folderOf = (comp) => (comp.format === "maven" && comp.name && comp.version
    ? (comp.group ? comp.group.split(".").join("/") + "/" : "") + comp.name + "/" + comp.version : "");

  // freshAssets lists the component's version folder again and returns exactly
  // the files that derive to the same component. The search result may hold a
  // capped asset list, and files may have changed since. A listing that is
  // truncated, or a component without a folder, blocks the deletion.
  const freshAssets = (comp) => async () => {
    const blocked = (msg) => Object.assign(new APIError(0, msg), { blocked: true });
    const folder = folderOf(comp);
    if (!folder) throw blocked(t("del.noFolder"));
    const data = await api(apiRepo(comp.repo, "/browse?path=" + enc(folder + "/")));
    if (data.truncated) throw blocked(t("del.searchTruncated"));
    return (data.entries || []).filter((e) => {
      if (e.dir) return false;
      const c = deriveComp(comp.format, folder + "/" + e.name);
      return c && c.group === (comp.group || "") && c.name === comp.name && c.version === comp.version;
    }).map((e) => ({ path: folder + "/" + e.name, size: e.size }));
  };

  const optsFor = (r, comp) => ({
    assetHref: (p) => browseLink(comp.repo, p),
    onAsset: (p) => drillAsset(r, comp, p),
    onDeleted: () => run(lastCriteria),
  });

  async function drill(comp) {
    const n = ++seq;
    const r = await repoOf(comp);
    if (!live() || n !== seq) return;
    const assets = comp.assets || [];
    // The result holds at most 50 assets per component, so deleting a Maven
    // component re-lists its folder instead of trusting it; the other formats
    // delete the whole version on the server.
    const opts = comp.format === "maven" ? Object.assign(optsFor(r, comp), { fresh: freshAssets(comp) }) : optsFor(r, comp);
    put(out,
      h("nav", { class: "crumbs", "aria-label": t("crumb.aria") }, backBtn(), sep(), h("span", { "aria-current": "page" }, label(comp))),
      componentView(r, comp, assets, opts));
    focusHead();
  }

  async function drillAsset(r, comp, p) {
    const n = ++seq;
    const up = h("button", { type: "button", class: "link-btn" }, label(comp));
    up.addEventListener("click", () => drill(comp));
    const box = h("div", null, loading());
    put(out, h("nav", { class: "crumbs", "aria-label": t("crumb.aria") }, backBtn(), sep(), up, sep(),
      h("span", { "aria-current": "page" }, p.slice(p.lastIndexOf("/") + 1))), box);
    try {
      const nodes = await assetView(r, p, optsFor(r, comp));
      if (box.isConnected && n === seq) { put(box, nodes); focusHead(); }
    } catch (e) {
      if (box.isConnected && n === seq) { put(box, h("div", { class: "empty" }, t("detail.loadFailed"))); handleError(e); }
    }
  }

  // Live filtering: the hash follows the criteria (replaceState, so typing
  // does not fill the history or re-render the screen).
  const go = () => {
    later.cancel();
    const c = current();
    const to = searchLink(kind, c.toString() ? c : null);
    if (location.hash !== to) history.replaceState(null, "", to);
    run(c);
  };
  const later = debounce(go, 350);
  state.leaving.push(later.cancel);
  for (const [key] of textFields) inputs[key].addEventListener("input", later);
  for (const el of [fmtSel, repoSel]) if (el) el.addEventListener("change", go);
  form.addEventListener("submit", (ev) => { ev.preventDefault(); go(); });
  reset.addEventListener("click", () => {
    for (const [key] of textFields) inputs[key].value = "";
    for (const el of [fmtSel, repoSel]) if (el) el.value = "";
    go();
    inputs[textFields[0][0]].focus();
  });

  const first = textFields.find(([key]) => criteria.get(key));
  if (!first) inputs[textFields[0][0]].focus();
  await run(criteria);
}
