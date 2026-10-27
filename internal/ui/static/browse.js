// Browse: the repository list, the lazy tree browser, and the component and
// asset detail views (search reuses the detail views). Plan 10 Decision 8
// gives the layout; the tree, deletion and history rules are plan 8's.
import {
  state, h, put, api, APIError, apiRepo, enc, encPath, browseLink, searchLink, adminRepoLink, fmtBytes, fmtNum, fmtDate,
  fmtBadge, typeBadge, fmtTile, nameCell, onlineBadge, FORMATS, FORMAT_NAMES, canUpload, uploadLink, repoBase, copyButton, copyLine,
  pageHead, loading, truncNote, card, kv, tbl, handleError, showError, icon,
} from "./util.js";
import { modal } from "./shell.js";
import { t } from "./i18n.js";

let uidN = 0;
const uid = (p) => p + "-" + (++uidN);
const blue = (text) => h("span", { class: "badge blue" }, text);
const full = (b) => { b.classList.remove("small"); return b; }; // a full-size button

// ---- repository list -------------------------------------------------------------

// viewRepos: #browse/browse — the concept table with text, format and type filters.
// With admin it is the settings list (#admin/repository/repositories): names
// open the settings form, and the head offers "Create repository".
export async function viewRepos(view, admin) {
  const data = await api("repositories");
  const repos = data.repositories || [];
  state.repos = repos;
  const q = h("input", { id: "repo-filter", type: "search", placeholder: t("repos.filter"), autocomplete: "off" });
  const fmtSel = h("select", { id: "repo-format" }, h("option", { value: "" }, t("repos.allFormats")),
    FORMATS.filter((f) => repos.some((r) => r.format === f)).map((f) => h("option", { value: f }, FORMAT_NAMES[f])));
  const typeSel = h("select", { id: "repo-type" }, h("option", { value: "" }, t("repos.allTypes")),
    ["hosted", "proxy", "group"].filter((ty) => repos.some((r) => r.type === ty)).map((ty) => h("option", { value: ty }, ty)));
  const count = h("span", { class: "end", role: "status" });
  const rows = repos.map((r) => {
    const tr = h("tr", null,
      h("td", { class: "name" }, nameCell(r.format, h("a", { href: admin ? adminRepoLink(r.name) : browseLink(r.name) }, h("strong", null, r.name)), FORMAT_NAMES[r.format] || r.format)),
      h("td", null, typeBadge(r.type)),
      admin ? h("td", null, r.type === "group" ? "—" : r.blobStore || "default") : null,
      h("td", null, onlineBadge(r.online)),
      h("td", { class: "num" }, r.type === "group" ? "—" : fmtNum(r.assets)),
      h("td", { class: "num" }, r.type === "group" ? "—" : fmtBytes(r.bytes)),
      h("td", { class: "act" }, h("div", { class: "actions" },
        admin ? h("a", { class: "btn small", href: adminRepoLink(r.name) }, t("repos.settings")) : null,
        h("a", { class: "btn small", href: browseLink(r.name) }, t("repos.browse")),
        copyButton(r.url, null, t("repos.copyUrlFor", { name: r.name }), t("repos.copyUrl")))));
    tr.dataset.name = r.name.toLowerCase();
    tr.dataset.format = r.format;
    tr.dataset.type = r.type;
    return tr;
  });
  const none = h("div", { class: "empty", hidden: true });
  const apply = () => {
    const s = q.value.trim().toLowerCase();
    let shown = 0;
    for (const tr of rows) {
      tr.hidden = !(!s || tr.dataset.name.includes(s)) || (fmtSel.value && tr.dataset.format !== fmtSel.value) || (typeSel.value && tr.dataset.type !== typeSel.value);
      if (!tr.hidden) shown++;
    }
    count.textContent = t("repos.count", { n: fmtNum(shown) });
    none.textContent = t("repos.noMatch");
    none.hidden = shown > 0 || !rows.length;
  };
  q.addEventListener("input", apply);
  fmtSel.addEventListener("change", apply);
  typeSel.addEventListener("change", apply);
  const up = admin ? h("a", { class: "btn primary", href: adminRepoLink("create") }, "＋ ", t("rf.create"))
    : repos.some(canUpload) ? h("a", { class: "btn primary", href: uploadLink() }, icon("upload"), t("nav.upload")) : null;
  put(view,
    pageHead(admin ? t("nav.repositories") : t("nav.browse"), h("p", null, admin ? t("repos.adminSub") : t("repos.sub")),
      up ? h("div", { class: "actions" }, up) : null, { eyebrow: admin ? t("rf.eyebrow") : t("repos.eyebrow") }),
    h("section", { class: "card" },
      h("div", { class: "toolbar" },
        h("label", { for: "repo-filter", class: "sr-only" }, t("repos.filter")), q,
        h("label", { for: "repo-format", class: "sr-only" }, t("col.format")), fmtSel,
        h("label", { for: "repo-type", class: "sr-only" }, t("col.type")), typeSel, count),
      tbl([t("col.repo"), t("col.type"), admin ? t("col.blobStore") : null, t("col.status"), { t: t("col.files"), cls: "num" }, { t: t("col.size"), cls: "num" }, { t: t("col.actions"), cls: "act" }].filter(Boolean),
        rows, state.me.anonymous ? t("repo.noneAnon") : t("repo.none")),
      none),
    h("div", { class: "notice" }, t("repos.typesNote"), admin ? [" ", t("repos.liveNote")] : null));
  apply();
}

// ---- component derivation (Decision 3, mirrored from components.go) ------------

const PY_EXTS = [".whl", ".tar.gz", ".tar.bz2", ".zip", ".egg"];
const normPy = (s) => s.toLowerCase().replace(/[-_.]+/g, "-");
const pyExt = (f) => PY_EXTS.find((e) => f.endsWith(e) && f.length > e.length);

function mavenFileMatches(file, name, version) {
  const boundary = (rest) => rest === "" || rest[0] === "-" || rest[0] === ".";
  let pre = name + "-" + version;
  if (file.startsWith(pre) && boundary(file.slice(pre.length))) return true;
  if (!version.endsWith("-SNAPSHOT")) return false;
  pre = name + "-" + version.slice(0, -9) + "-";
  if (!file.startsWith(pre)) return false;
  const m = /^[0-9]{8}[.][0-9]+-[0-9]+/.exec(file.slice(pre.length));
  return !!m && boundary(file.slice(pre.length + m[0].length));
}

function pyVersion(project, file) {
  const ext = pyExt(file) || "";
  const base = file.slice(0, file.length - ext.length);
  for (let i = 0; i < base.length - 1; i++) {
    if (base[i] === "-" && base[i + 1] >= "0" && base[i + 1] <= "9" && normPy(base.slice(0, i)) === project) {
      const v = base.slice(i + 1);
      return ext === ".whl" ? v.split("-")[0] : v;
    }
  }
  const f = base.split("-");
  return ext === ".whl" && f.length >= 2 && f[1] ? f[1] : file;
}

// unbang reverses Go's case-encoding ("!b" is "B"); text that is not
// validly encoded is kept as is. bang is the encoding itself.
function unbang(s) {
  if (/[A-Z]/.test(s)) return s;
  const r = s.replace(/!([a-z])/g, (_, ch) => ch.toUpperCase());
  return r.includes("!") ? s : r;
}
const bang = (s) => s.replace(/[A-Z]/g, (ch) => "!" + ch.toLowerCase());

// deriveComp maps a stored file path to its component {group, name,
// version}, or null when the file is not part of one.
export function deriveComp(format, p) {
  const segs = p.split("/"), n = segs.length, file = segs[n - 1];
  const c = (group, name, version) => ({ group, name, version });
  switch (format) {
    case "maven": {
      if (n < 4 || file.startsWith("maven-metadata.xml")) return null;
      const name = segs[n - 3], version = segs[n - 2], g = segs.slice(0, n - 3);
      if (!name || !version || g.some((x) => !x) || !mavenFileMatches(file, name, version)) return null;
      return c(g.join("."), name, version);
    }
    case "npm": {
      const i = p.indexOf("/-/");
      if (i <= 0) return null;
      const pkg = p.slice(0, i), f = p.slice(i + 3);
      if (f.includes("/")) return null;
      let group = "", name = pkg;
      if (pkg.startsWith("@")) {
        const j = pkg.indexOf("/");
        if (j < 0) return null;
        group = pkg.slice(0, j);
        name = pkg.slice(j + 1);
        if (!name || name.includes("/")) return null;
      } else if (pkg.includes("/")) return null;
      if (!f.startsWith(name + "-") || !f.endsWith(".tgz")) return null;
      const v = f.slice(name.length + 1, -4);
      return v ? c(group, name, v) : null;
    }
    case "go": {
      const i = p.indexOf("/@v/");
      if (i <= 0) return null;
      const m = /^([^/]+)\.(zip|mod|info)$/.exec(p.slice(i + 4));
      return m ? c("", unbang(p.slice(0, i)), unbang(m[1])) : null;
    }
    case "pypi":
      if (segs[0] !== "packages" || (n !== 3 && n !== 4) || !pyExt(file)) return null;
      return c("", segs[1], pyVersion(normPy(segs[1]), file));
    case "cargo": {
      const m = n === 3 && segs[0] === "crates" ? /^(.+)\.crate$/.exec(file) : null;
      return m ? c("", segs[1], m[1]) : null;
    }
  }
  return null;
}

// xml escapes text placed in an XML snippet so the snippet stays valid XML.
const XML_ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&apos;" };
const xml = (v) => String(v).replace(/[&<>"']/g, (ch) => XML_ESC[ch]);

const coord = (c) => (c.group ? c.group + ":" : "") + c.name + ":" + c.version;
const baseName = (p) => p.slice(p.lastIndexOf("/") + 1);
// absURL is the client-facing URL of a stored file.
const absURL = (r, p) => (r.url || location.origin + repoBase(r)) + encPath(p);
const sameOrigin = (r, p) => repoBase(r) + encPath(p);

// ---- detail building blocks -------------------------------------------------------

// tabs renders WAI-ARIA tabs in the concept's subtab style; each entry is
// [label, render], render returning the panel's nodes when it is picked.
function tabs(list) {
  const panel = h("div", { role: "tabpanel", class: "tabpanel" });
  const btns = list.map(([label]) => h("button", { type: "button", class: "subtab", role: "tab", id: uid("tab"), "aria-selected": "false", tabindex: "-1" }, label));
  const pick = (i, focus) => {
    btns.forEach((b, j) => { b.setAttribute("aria-selected", String(i === j)); b.tabIndex = i === j ? 0 : -1; });
    panel.setAttribute("aria-labelledby", btns[i].id);
    put(panel, list[i][1]());
    if (focus) btns[i].focus();
  };
  btns.forEach((b, i) => b.addEventListener("click", () => pick(i)));
  const bar = h("div", { class: "subtabs", role: "tablist" }, btns);
  bar.addEventListener("keydown", (ev) => {
    const i = btns.indexOf(document.activeElement);
    if (i < 0) return;
    const to = { ArrowRight: (i + 1) % btns.length, ArrowLeft: (i + btns.length - 1) % btns.length, Home: 0, End: btns.length - 1 }[ev.key];
    if (to === undefined) return;
    ev.preventDefault();
    pick(to, true);
  });
  pick(0);
  return [bar, panel];
}
const once = (fn) => { let v = null; return () => v || (v = fn()); };

function hero(format, title, badge, sub) {
  return h("div", { class: "componenthero" }, fmtTile(format),
    h("div", { class: "hero-text" }, h("h2", { tabindex: "-1" }, title, badge ? [" ", badge] : null), sub ? h("p", null, sub) : null));
}

function usageTools(format, c) {
  const full = c.group ? c.group + "/" + c.name : c.name;
  switch (format) {
    case "maven": return [
      ["Apache Maven", "<dependency>\n  <groupId>" + xml(c.group) + "</groupId>\n  <artifactId>" + xml(c.name) + "</artifactId>\n  <version>" + xml(c.version) + "</version>\n</dependency>"],
      ["Gradle", "implementation '" + c.group + ":" + c.name + ":" + c.version + "'"]];
    case "npm": return [["npm", "npm install " + full + "@" + c.version]];
    case "pypi": return [["pip", "pip install " + c.name + "==" + c.version]];
    case "go": return [["Go", "go get " + c.name + "@" + c.version]];
    case "cargo": return [["Cargo", "cargo add " + c.name + "@" + c.version + " --registry commandcenter"]];
  }
  return [];
}

// usageBlock is the concept's code block with a tool picker and copy.
function usageBlock(format, c) {
  const tools = c ? usageTools(format, c) : [];
  if (!tools.length) return null;
  const id = uid("tool");
  const sel = tools.length > 1 ? h("select", { id, class: "tool-sel", "aria-label": t("usage.tool") }, tools.map(([name], i) => h("option", { value: String(i) }, name))) : null;
  const code = h("pre", { class: "code" });
  const slot = h("span");
  const show = () => {
    const text = tools[sel ? Number(sel.value) || 0 : 0][1];
    code.textContent = text;
    put(slot, copyButton(text, code, t("usage.copy")));
  };
  if (sel) sel.addEventListener("change", show);
  show();
  return h("div", { class: "usage" },
    h("div", { class: "codebar" }, h("span", null, t("usage.title"), sel ? null : " · " + tools[0][0]), h("span", { class: "actions" }, sel, slot)), code);
}

// Deleting needs the delete privilege on a hosted repository (canDelete).
// Maven deletes file by file through the repository; the other formats delete
// a whole version on the server (versionDelete), as their clients cannot.
const canDelete = (r) => r.type === "hosted" && !!r.canDelete;

async function deletePaths(r, paths) {
  for (const p of paths) {
    let res;
    try {
      res = await fetch(repoBase(r) + encPath(p), { method: "DELETE", credentials: "same-origin", headers: { "X-Command-Center-UI": "1" } });
    } catch (e) {
      throw new APIError(0, t("del.connect"));
    }
    if (!res.ok && res.status !== 404) {
      let msg = "";
      try { msg = (await res.text()).slice(0, 300); } catch (e) { /* ignore */ }
      throw new APIError(res.status, t("del.failed", { status: res.status, msg }));
    }
  }
}

// versionDelete is the "Delete component" button of a format whose versions
// are deleted whole: every file of c goes, also those a listing left out.
const versionDelete = (r, c, refresh) => deleteButton(t("del.component"), async () => ({
  text: t("del.versionText", { coord: coord(c) }),
  del: () => api(apiRepo(r.name, "/components?" + new URLSearchParams({ group: c.group || "", name: c.name, version: c.version })), { method: "DELETE" }),
}), refresh);

export function confirmDelete(text, title, yesLabel) {
  return new Promise((resolve) => {
    let ok = false;
    const no = h("button", { type: "button", class: "btn", autofocus: true }, t("common.cancel"));
    const yes = h("button", { type: "button", class: "btn danger" }, yesLabel || t("common.delete"));
    const m = modal(title || t("del.title"), h("p", { class: "confirm-text" }, text), [no, yes],
      { cls: "dlg-small", onClose: () => resolve(ok) });
    no.addEventListener("click", m.close);
    yes.addEventListener("click", () => { ok = true; m.close(); });
  });
}

// deleteButton deletes after confirmation, then refreshes. prepare() runs
// first and returns {text, del}: the confirmation text and the delete action.
// If it throws, the error is shown; one with blocked set also disables the
// button for good, with the message as its tooltip. blocked, when set,
// disables the button from the start and explains why. After a successful
// delete the button stays disabled even when the refresh fails.
function deleteButton(label, prepare, refresh, blocked) {
  const b = h("button", { type: "button", class: "btn danger", disabled: !!blocked, title: blocked || null }, label);
  b.addEventListener("click", async () => {
    let plan;
    b.disabled = true;
    try {
      plan = await prepare();
    } catch (e) {
      if (e && e.blocked) b.title = e.message;
      else b.disabled = false;
      handleError(e);
      return;
    }
    b.disabled = false;
    if (!(await confirmDelete(plan.text))) return;
    b.disabled = true;
    try {
      await plan.del();
    } catch (e) {
      b.disabled = false;
      handleError(e);
      return;
    }
    try {
      await refresh();
    } catch (e) {
      showError(t("del.refreshFailed", { msg: (e && e.message) || e }));
    }
  });
  return [b, blocked ? h("small", { class: "muted" }, blocked) : null];
}

const repoLink = (name) => h("a", { href: browseLink(name) }, name);

// pathLink links a repository path; a plain click is handled in place by
// handle (the tree reveals it, search opens the asset).
function pathLink(href, text, handle) {
  return h("a", {
    href,
    onclick: handle ? (ev) => {
      if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.button) return;
      ev.preventDefault();
      handle();
    } : null,
  }, text);
}
const assetLink = (path, opts) => pathLink(opts.assetHref(path), baseName(path), () => opts.onAsset(path));

// mainAsset is the file a component's download URL points at: the first one
// that is not metadata, a checksum or a signature.
const mainAsset = (assets) => assets.find((a) => !/\.(pom|mod|info|sha1|sha256|sha512|md5|asc)$/.test(a.path)) || assets[0];

// versionList loads the versions of c's package, newest first where the
// server sorts them: [{version, created, yanked, path}] and truncated.
async function versionList(r, c) {
  const pkgVersions = async (name) => {
    const d = await api(apiRepo(r.name, "/packages/" + encPath(name)));
    return { list: d.versions || [], truncated: !!d.truncated };
  };
  const listDir = async (dir) => {
    const d = await api(apiRepo(r.name, "/browse?path=" + enc(dir)));
    return { entries: d.entries || [], truncated: !!d.truncated };
  };
  switch (r.format) {
    case "maven": {
      const dir = (c.group ? c.group.split(".").join("/") + "/" : "") + c.name + "/";
      const d = await listDir(dir);
      return { truncated: d.truncated, list: d.entries.filter((e) => e.dir).map((e) => ({ version: e.name, path: dir + e.name })).reverse() };
    }
    case "npm": {
      const name = c.group ? c.group + "/" + c.name : c.name;
      const d = await pkgVersions(name);
      return { truncated: d.truncated, list: d.list.map((v) => Object.assign({}, v, { path: name + "/-/" + v.version })) };
    }
    case "cargo": { // c.name is the published spelling; stored paths are lowercase
      const d = await pkgVersions(c.name);
      return { truncated: d.truncated, list: d.list.map((v) => Object.assign({}, v, { path: "crates/" + c.name.toLowerCase() + "/" + v.version + ".crate" })) };
    }
    case "pypi": { // the rows are files: one version per wheel/sdist group
      const d = await pkgVersions(c.name);
      const by = new Map();
      for (const f of d.list) {
        const v = pyVersion(normPy(c.name), f.version);
        if (!by.has(v)) by.set(v, { version: v, created: f.created, yanked: f.yanked, path: "packages/" + c.name + "/" + f.version });
      }
      return { truncated: d.truncated, list: [...by.values()] };
    }
    case "go": {
      const dir = bang(c.name) + "/@v/";
      const d = await listDir(dir);
      const by = new Map();
      for (const e of d.entries) {
        const x = e.dir ? null : deriveComp("go", dir + e.name);
        if (!x) continue;
        const cur = by.get(x.version);
        if (!cur || (e.lastModified || "") > (cur.created || "")) by.set(x.version, { version: x.version, created: e.lastModified, path: dir + x.version });
      }
      return { truncated: d.truncated, list: [...by.values()].sort((a, b) => (b.created || "").localeCompare(a.created || "")) };
    }
  }
  return { list: [], truncated: false };
}

function versionsBox(r, c, opts) {
  const box = h("div", null, loading());
  versionList(r, c).then((d) => {
    put(box, tbl([t("col.version"), t("col.updated"), { t: h("span", { class: "sr-only" }, t("col.open")), cls: "act" }],
      d.list.map((v) => h("tr", null,
        h("td", null, v.version === c.version ? [blue(v.version), " ", h("small", { class: "muted" }, t("comp.current"))] : v.version,
          v.yanked ? [" ", h("span", { class: "badge amber" }, t("comp.yanked"))] : null),
        h("td", { class: "nowrap" }, v.created ? fmtDate(v.created) : "—"),
        h("td", { class: "act" }, pathLink(browseLink(r.name, v.path), t("comp.view"), opts.onPath ? () => opts.onPath(v.path) : null)))),
      t("comp.noVersions")), d.truncated ? truncNote(t("comp.versionsTruncated")) : null);
  }, (e) => {
    put(box, h("div", { class: "empty" }, e && e.status === 404 ? t("comp.noVersions") : t("detail.loadFailed")));
    if (!e || e.status !== 404) handleError(e);
  });
  return box;
}

// componentView renders a component in the concept's layout: a hero, tabs
// (overview, assets, versions), then resource status and actions. assets are
// {path, size, lastModified}; opts gives assetHref, onAsset, onPath (to open
// a version in place), (to allow deletion) onDeleted, and for Maven, which
// deletes file by file, incomplete: a reason the asset list may be partial,
// which blocks "Delete component" so it never deletes only some of the files,
// and fresh: an async function that returns the component's current complete
// asset list, used instead of assets when deleting (it may throw an error
// with blocked set).
export function componentView(r, c, assets, opts) {
  const total = assets.reduce((n, a) => n + (Number(a.size) || 0), 0);
  const updated = c.lastUpdated || assets.reduce((m, a) => (a.lastModified && a.lastModified > m ? a.lastModified : m), "");
  const del = !canDelete(r) || !opts.onDeleted ? null
    : r.format !== "maven" ? versionDelete(r, c, opts.onDeleted)
      : deleteButton(t("del.component"), async () => {
        const list = opts.fresh ? await opts.fresh() : assets;
        return {
          text: t("del.componentText", { coord: coord(c), n: list.length }),
          del: () => deletePaths(r, list.map((a) => a.path)),
        };
      }, opts.onDeleted, opts.incomplete);
  const main = mainAsset(assets);
  // Downloads (plan 11 Decision 1): undefined when the listing did not carry them.
  const known = assets.filter((a) => a.downloads !== undefined);
  const downloads = known.reduce((n, a) => n + (Number(a.downloads) || 0), 0);
  const lastDl = known.reduce((m, a) => (a.lastDownloaded && a.lastDownloaded > m ? a.lastDownloaded : m), "");
  const assetRows = assets.map((a) => h("tr", null,
    h("td", { class: "name" }, h("span", { class: "row-ico" }, icon("file")), assetLink(a.path, opts)),
    h("td", { class: "num" }, fmtBytes(a.size)),
    h("td", { class: "num", title: a.lastDownloaded ? t("comp.lastDownloaded") + ": " + fmtDate(a.lastDownloaded) : null }, a.downloads === undefined ? "—" : fmtNum(a.downloads)),
    h("td", { class: "act" }, h("a", { class: "btn small", href: sameOrigin(r, a.path), download: "" }, t("asset.download")))));
  const mainCard = h("section", { class: "card" },
    hero(r.format, c.name, blue(c.version), [c.group || r.name, FORMAT_NAMES[r.format] || r.format, fmtBytes(total)].join(" · ")),
    h("div", { class: "card-body" }, tabs([
      [t("comp.overview"), () => [
        kv(t("comp.group"), c.group), kv(t("comp.name"), c.name), kv(t("col.version"), c.version),
        kv(t("col.repo"), repoLink(r.name)), kv(t("col.format"), fmtBadge(r.format)),
        kv(t("col.updated"), updated ? fmtDate(updated) : ""),
        known.length ? kv(t("comp.downloads"), fmtNum(downloads)) : null,
        known.length ? kv(t("comp.lastDownloaded"), lastDl ? fmtDate(lastDl) : t("comp.neverDownloaded")) : null,
        usageBlock(r.format, c)]],
      [t("comp.assets", { n: assets.length }), () => tbl([t("col.file"), { t: t("col.bytes"), cls: "num" }, { t: t("comp.downloads"), cls: "num" }, { t: t("col.actions"), cls: "act" }], assetRows, t("comp.noAssets"))],
      [t("comp.versions"), once(() => versionsBox(r, c, opts))],
    ])));
  return h("div", { class: "two detail" }, mainCard,
    h("aside", { class: "stack" },
      card(t("comp.status"), t("comp.statusSub"), [
        kv(t("comp.stored"), h("span", { class: "badge green" }, r.type === "proxy" ? t("comp.cached") : t("comp.storedYes"))),
        kv(t("col.repo"), repoLink(r.name)), kv(t("col.format"), FORMAT_NAMES[r.format] || r.format),
        kv("Asset", t("comp.assetSum", { n: assets.length, size: fmtBytes(total) }))]),
      card(t("comp.actions"), t("comp.actionsSub"), h("div", { class: "stack" },
        main ? full(copyButton(absURL(r, main.path), null, t("comp.copyUrlOf", { name: baseName(main.path) }), t("comp.copyUrl"))) : null,
        del))));
}

// assetView loads an asset and renders its summary, usage, checksums and actions.
export async function assetView(r, path, opts) {
  const a = await api(apiRepo(r.name, "/asset?path=" + enc(path)));
  const c = deriveComp(r.format, a.path);
  // A cargo path holds the crate name lowercased; cargo add needs the
  // published spelling, which the asset API reads from the package rows or,
  // for a proxy, its cached index (QA L-20).
  if (c && a.crate) c.name = a.crate;
  const proxy = r.type === "proxy";
  let target = a.downloadUrl;
  try { target = new URL(a.downloadUrl, location.href).pathname; } catch (e) { /* use as is */ }
  // A file of another format is deleted with its whole version.
  const del = !canDelete(r) || !opts.onDeleted ? null
    : r.format !== "maven" ? (c ? versionDelete(r, c, opts.onDeleted) : null)
      : deleteButton(t("del.asset"), async () => ({ text: t("del.assetText", { path: a.path }), del: () => deletePaths(r, [a.path]) }), opts.onDeleted);
  const hash = (v) => (v ? h("code", { class: "hash" }, v) : "");
  return h("div", { class: "two detail" },
    h("section", { class: "card" },
      hero(r.format, baseName(a.path), null, [r.name, fmtBytes(a.size)].join(" · ")),
      h("div", { class: "card-body" },
        kv(t("col.repo"), repoLink(r.name)),
        kv(t("col.format"), fmtBadge(r.format)),
        c && c.group ? kv(t("comp.group"), c.group) : null,
        c ? kv(t("asset.component"), c.name + " " + c.version) : null,
        kv(t("asset.path"), copyLine(a.path, t("asset.copyPath"))),
        kv(t("asset.type"), a.contentType),
        kv(t("col.bytes"), fmtBytes(a.size) + " (" + t("asset.bytes", { n: fmtNum(a.size) }) + ")"),
        kv(t("asset.created"), fmtDate(a.created)),
        kv(t("asset.modified"), fmtDate(a.lastModified)),
        kv(t("comp.downloads"), fmtNum(a.downloads)),
        kv(t("comp.lastDownloaded"), a.lastDownloaded ? fmtDate(a.lastDownloaded) : t("comp.neverDownloaded")),
        proxy ? kv(t("asset.checked"), fmtDate(a.checked)) : null,
        usageBlock(r.format, c))),
    h("aside", { class: "stack" },
      card(t("comp.actions"), null, h("div", { class: "stack" },
        a.canDownload === false ? null : h("a", { class: "btn primary", href: target, download: "" }, t("asset.download")),
        full(copyButton(absURL(r, a.path), null, t("comp.copyUrlOf", { name: baseName(a.path) }), t("comp.copyUrl"))),
        del)),
      card(t("asset.checksums"), null, [kv("SHA-1", hash(a.sha1)), kv("SHA-256", hash(a.sha256)), kv("SHA-512", hash(a.sha512)), kv("MD5", hash(a.md5))])));
}

// ---- repository setup (client configuration, from docs/guide.md) -------------

function setupSnippets(r) {
  const url = r.url;
  let host = "";
  try { host = new URL(url).hostname; } catch (e) { /* keep empty */ }
  const local = ["localhost", "127.0.0.1", "::1", "[::1]"].includes(host);
  const hosted = r.type === "hosted";
  const cred = "<server><id>commandcenter</id><username>ci</username><password>" + t("setup.password") + "</password></server>";
  switch (r.format) {
    case "maven": {
      // The mirror alone leaves Maven's "central" with snapshots off, so no
      // SNAPSHOT dependency is ever requested; the profile turns them on for
      // "central", which the mirror then sends here (QA M-17).
      const central = (tag) => "<" + tag + "><id>central</id><url>http://central</url>\n          <releases><enabled>true</enabled></releases>\n          <snapshots><enabled>true</enabled></snapshots></" + tag + ">";
      return hosted
        ? [["pom.xml — distributionManagement", "<distributionManagement>\n  <repository>\n    <id>commandcenter</id>\n    <url>" + xml(url) + "</url>\n  </repository>\n</distributionManagement>"],
          ["~/.m2/settings.xml", "<settings>\n  <servers>\n    " + cred + "\n  </servers>\n</settings>"]]
        : [["~/.m2/settings.xml — mirror", "<settings>\n  <servers>\n    " + cred + "\n  </servers>\n  <mirrors>\n    <mirror><id>commandcenter</id><mirrorOf>*</mirrorOf>\n      <url>" + xml(url) + "</url></mirror>\n  </mirrors>\n" +
          "  <profiles>\n    <profile>\n      <id>commandcenter</id>\n      <repositories>\n        " + central("repository") + "\n      </repositories>\n" +
          "      <pluginRepositories>\n        " + central("pluginRepository") + "\n      </pluginRepositories>\n    </profile>\n  </profiles>\n" +
          "  <activeProfiles>\n    <activeProfile>commandcenter</activeProfile>\n  </activeProfiles>\n</settings>"]];
    }
    case "npm":
      return [["~/.npmrc", "registry=" + url], ["npm login", "npm login --registry " + url]];
    case "pypi": {
      const out = [["pip.conf", "[global]\nindex-url = " + url + "simple/" + (local ? "" : "\ntrusted-host = " + host)]];
      if (hosted || r.writeMember) out.push(["twine", "twine upload --repository-url " + url + " -u ci dist/*"]);
      return out;
    }
    case "go":
      return [["GOPROXY", "go env -w GOPROXY=" + url.replace(/\/$/, "") + "\ngo env -w GONOSUMDB=example.com"]];
    case "cargo":
      return [["~/.cargo/config.toml", "[registries.commandcenter]\nindex = \"sparse+" + url + "\"" + (hosted ? "" : "\n\n[source.crates-io]\nreplace-with = \"commandcenter\"")]];
  }
  return [];
}

function setupCard(r) {
  const list = setupSnippets(r);
  if (!list.length) return null;
  return card(t("setup.title"), t("setup.sub"), list.map(([title, text]) => {
    const pre = h("pre", { class: "code" }, text);
    return h("div", { class: "snippet" }, h("div", { class: "codebar" }, h("span", null, title), copyButton(text, pre, t("setup.copy", { name: title }))), pre);
  }));
}

// ---- tree browser ------------------------------------------------------------------

// viewTree: #browse/browse:<repo>[:<path>] — a lazy tree (left) and the
// detail of the selected node (right).
export async function viewTree(view, name, path) {
  const r = await api(apiRepo(name));
  let alive = true;
  state.leaving.push(() => { alive = false; });
  const fmt = r.format;
  // Search skips group repositories, so a group's link searches everything.
  const head = pageHead(r.name, h("p", { class: "badges" }, fmtBadge(fmt), typeBadge(r.type)),
    h("div", { class: "actions" },
      h("a", { class: "btn", href: browseLink() }, "← " + t("tree.back")),
      full(copyButton(r.url, null, t("repos.copyUrlFor", { name: r.name }), t("repos.copyUrl"))),
      h("a", { class: "btn", href: searchLink("generic", r.type === "group" ? null : { repo: name }) }, icon("search"), t("tree.search")),
      state.me.admin ? h("a", { class: "btn", href: adminRepoLink(r.name) }, t("tree.settings")) : null,
      canUpload(r) ? h("a", { class: "btn primary", href: uploadLink(r.name) }, icon("upload"), t("tree.upload")) : null),
    { eyebrow: t("tree.eyebrow") });
  if (r.type === "group") {
    const members = r.members || [];
    put(view, head, h("div", { class: "two" },
      card(t("tree.members"), t("tree.groupNote"), members.length
        ? members.map((m) => h("div", { class: "listrow" }, h("a", { href: browseLink(m) }, h("strong", null, m)), h("a", { class: "btn small", href: browseLink(m) }, t("repos.browse"))))
        : h("div", { class: "empty" }, t("tree.noMembers"))),
      card(t("repo.info"), null, [kv(t("col.format"), fmtBadge(fmt)), kv(t("col.type"), typeBadge(r.type)), kv("URL", copyLine(r.url, t("repos.copyUrlFor", { name: r.name })))])),
    setupCard(r));
    return;
  }

  const treeEl = h("ul", { class: "tree", role: "tree", "aria-label": t("tree.aria", { name: r.name }) });
  const emptyEl = h("div", { class: "empty", hidden: true }, t("tree.empty"));
  const detail = h("div", { class: "detail-col" }, loading());
  const byLi = new WeakMap();
  const root = { kind: "dir", path: "", level: 0, children: null, expanded: true, group: treeEl, parent: null };
  let selected = null, focused = null, dseq = 0;
  let settled = false, restoring = false; // settled: initial selection done; restoring: handling Back/Forward

  const expandable = (n) => n.kind === "dir" || n.kind === "comp";

  function mk(kind, p, label, parent, extra) {
    const n = Object.assign({ kind, path: p, label, parent, level: parent.level + 1, children: null, expanded: false }, extra);
    const ico = kind === "dir" ? "folder" : kind === "comp" ? "box" : kind === "asset" ? "file" : "info";
    n.icoBox = h("span", { class: "tree-ico" }, icon(ico));
    n.row = h("div", { class: "tree-row" },
      h("span", { class: "twisty", "aria-hidden": "true" }, expandable(n) ? icon("chevron") : null),
      n.icoBox, h("span", { class: "tree-label" }, label));
    n.li = h("li", { role: "treeitem", "aria-level": String(n.level), "aria-selected": "false", "aria-label": label, tabindex: "-1", class: kind === "more" ? "tree-more" : null }, n.row);
    if (expandable(n)) {
      n.group = h("ul", { role: "group", hidden: true });
      n.li.setAttribute("aria-expanded", "false");
      n.li.appendChild(n.group);
    }
    byLi.set(n.li, n);
    return n;
  }

  function setKind(n, kind, c) {
    n.kind = kind;
    n.comp = c;
    put(n.icoBox, kind === "comp" ? icon("box") : icon("folder"));
  }

  // build turns a browse listing into child nodes: Maven version folders,
  // npm "-" folders and Go "@v" folders give component nodes (Decision 3).
  function build(node, entries, truncated) {
    const base = node.path ? node.path + "/" : "";
    const kids = [];
    const files = entries.filter((e) => !e.dir);
    const hasMeta = fmt === "maven" && files.some((e) => e.name === "maven-metadata.xml");
    for (const e of entries.filter((x) => x.dir)) {
      const p = base + e.name;
      const segs = p.split("/");
      if (hasMeta && segs.length >= 3) {
        kids.push(mk("comp", p, e.name, node, { comp: { group: segs.slice(0, -2).join("."), name: segs[segs.length - 2], version: e.name } }));
      } else {
        kids.push(mk("dir", p, e.name, node));
      }
    }
    // A Maven folder is a version folder when one of its files derives to a
    // component; the metadata guess above is corrected once it is listed.
    if (fmt === "maven" && node !== root && !node.synthetic) {
      const c = files.map((e) => deriveComp(fmt, base + e.name)).find(Boolean);
      if (c) setKind(node, "comp", c);
      else if (node.kind === "comp") setKind(node, "dir", null);
    }
    const grouping = (fmt === "npm" && base.endsWith("/-/")) || (fmt === "go" && base.endsWith("/@v/"));
    const comps = new Map();
    for (const e of files) {
      const p = base + e.name;
      const c = grouping ? deriveComp(fmt, p) : null;
      if (!c) {
        kids.push(mk("asset", p, e.name, node, { size: e.size, mod: e.lastModified, dl: e.downloads, ldl: e.lastDownloaded }));
        continue;
      }
      let cn = comps.get(c.version);
      if (!cn) {
        cn = mk("comp", base + c.version, c.version, node, { comp: c, synthetic: true });
        cn.children = [];
        comps.set(c.version, cn);
        kids.push(cn);
      }
      const an = mk("asset", p, e.name, cn, { size: e.size, mod: e.lastModified, dl: e.downloads, ldl: e.lastDownloaded });
      cn.children.push(an);
      cn.group.appendChild(an.li);
    }
    if (truncated) kids.push(mk("more", base + "\u0000more", t("tree.moreLabel"), node));
    node.children = kids;
    node.truncated = !!truncated;
    put(node.group, kids.map((k) => k.li));
    if (node === root) emptyEl.hidden = kids.length > 0;
  }

  async function load(node) {
    if (node.children) return;
    if (!node.pending) {
      node.pending = api(apiRepo(name, "/browse?path=" + enc(node.path ? node.path + "/" : "")))
        .then((d) => { if (alive) build(node, d.entries || [], d.truncated); })
        .finally(() => { node.pending = null; });
    }
    await node.pending;
  }

  async function expand(node) {
    if (node === root || !expandable(node)) return;
    await load(node);
    node.expanded = true;
    node.li.setAttribute("aria-expanded", "true");
    node.group.hidden = false;
  }

  function collapse(node) {
    if (!expandable(node) || node === root) return;
    node.expanded = false;
    node.li.setAttribute("aria-expanded", "false");
    node.group.hidden = true;
  }

  function focusItem(node, move) {
    if (!node || node === root) return;
    if (focused && focused !== node && focused.li) focused.li.setAttribute("tabindex", "-1");
    focused = node;
    node.li.setAttribute("tabindex", "0");
    if (move) node.li.focus();
  }

  function select(node, o) {
    if (!alive) return;
    o = o || {};
    if (selected && selected.li) selected.li.setAttribute("aria-selected", "false");
    selected = node;
    rootBtn.classList.toggle("active", node === root);
    if (node !== root) {
      node.li.setAttribute("aria-selected", "true");
      focusItem(node, o.focus);
    }
    if (!o.keepHash && !restoring && node.kind !== "more") {
      // Each user selection is a history entry, so Back returns to the previous node;
      // the initial (deep-link) selection only rewrites the current one.
      const to = browseLink(name, node === root ? "" : node.path);
      if (to !== location.hash) history[settled ? "pushState" : "replaceState"](null, "", to);
    }
    if (!o.noDetail) renderDetail(node);
  }

  const run = (fn) => Promise.resolve().then(fn).catch((e) => { if (alive) handleError(e); });

  // afterDelete reloads the nearest ancestor that still has content and
  // selects it.
  async function afterDelete(start) {
    let p = start || root;
    for (;;) {
      p.children = null;
      await load(p);
      if (!alive) return;
      if (p === root || (p.children && p.children.length)) break;
      p = p.parent || root;
    }
    Object.assign(r, await api(apiRepo(name))); // file counts changed
    if (!alive) return;
    if (p !== root) await expand(p);
    if (!alive) return;
    select(p, { focus: p !== root });
    if (p === root && root.children.length) focusItem(root.children[0], true);
  }

  const optsFor = (node) => ({
    assetHref: (p) => browseLink(name, p),
    onAsset: (p) => run(() => reveal(p, true)),
    onPath: (p) => run(() => reveal(p, true)),
    onDeleted: () => afterDelete(node.parent),
    // A truncated listing may hide some of the component's files.
    incomplete: node.truncated || (node.synthetic && node.parent.truncated) ? t("del.truncated") : null,
  });

  function rootDetail() {
    const n = root.children ? root.children.filter((k) => k.kind !== "more").length : 0;
    return h("div", { class: "stack" },
      card(t("repo.info"), r.name, [
        kv(t("col.format"), fmtBadge(fmt)), kv(t("col.type"), typeBadge(r.type)), kv(t("col.status"), onlineBadge(r.online)),
        kv("URL", copyLine(r.url, t("repos.copyUrlFor", { name: r.name }))),
        r.remote ? kv(t("repo.remote"), h("code", null, r.remote)) : null,
        kv(t("col.files"), fmtNum(r.assets)), kv(t("col.size"), fmtBytes(r.bytes)),
        kv(t("tree.children"), root.truncated ? n + "+" : String(n))]),
      setupCard(r));
  }

  function folderDetail(node) {
    const n = node.children.filter((k) => k.kind !== "more").length;
    return h("div", { class: "stack" },
      card(node.label, t("tree.folder"), [
        kv(t("col.repo"), repoLink(r.name)), kv(t("asset.path"), copyLine(node.path + "/", t("asset.copyPath"))),
        kv(t("tree.children"), node.truncated ? n + "+" : String(n))]),
      node.truncated ? truncNote(t("tree.moreText")) : null);
  }

  async function renderDetail(node) {
    const n = ++dseq;
    put(detail, loading());
    try {
      let out;
      if (node === root) {
        await load(root);
        out = rootDetail();
      } else if (node.kind === "dir" || node.kind === "comp") {
        await load(node);
        // Listing may turn a guessed Maven version folder back into a folder.
        if (node.kind === "comp" && node.comp) {
          const assets = node.children.filter((k) => k.kind === "asset" && (node.synthetic || deriveComp(fmt, k.path)))
            .map((k) => ({ path: k.path, size: k.size, lastModified: k.mod, downloads: k.dl, lastDownloaded: k.ldl }));
          out = componentView(r, node.comp, assets, optsFor(node));
        } else {
          out = folderDetail(node);
        }
      } else if (node.kind === "asset") {
        out = await assetView(r, node.path, optsFor(node));
      } else {
        out = card(t("tree.moreLabel"), null, [truncNote(t("tree.moreText")),
          h("a", { href: searchLink("generic", { repo: name }) }, t("tree.search"))]);
      }
      if (alive && n === dseq) put(detail, out);
    } catch (e) {
      if (alive && n === dseq) {
        put(detail, h("div", { class: "empty" }, t("detail.loadFailed")));
        handleError(e);
      }
    }
  }

  // reveal expands the ancestors of target one level at a time and selects
  // it. A path outside the listed entries still shows its asset details.
  async function reveal(target, focus) {
    target = target.replace(/\/+$/, "");
    let node = root;
    for (;;) {
      await load(node);
      if (!alive) return;
      await expand(node);
      if (!alive) return;
      const kids = node.children;
      let next = kids.find((k) => k.path === target && k.kind !== "more");
      if (next) {
        await expand(next);
        if (!alive) return;
        select(next, { focus });
        if (focus === undefined) next.li.scrollIntoView({ block: "nearest" });
        return;
      }
      next = kids.find((k) => expandable(k) && !k.synthetic && target.startsWith(k.path + "/")) ||
        kids.find((k) => k.synthetic && k.children.some((a) => a.path === target));
      if (!next) break;
      node = next;
    }
    select(node, { noDetail: true, keepHash: true });
    const n = ++dseq;
    let out;
    try {
      out = [truncNote(t("tree.outside")), await assetView(r, target, { assetHref: optsFor(node).assetHref, onAsset: optsFor(node).onAsset })];
    } catch (e) {
      if (!alive) return;
      if (e.status !== 404) throw e;
      out = h("div", { class: "empty" }, t("tree.notFound", { path: target }));
    }
    if (alive && n === dseq) put(detail, out);
  }

  // Mouse: a click selects (and opens) a node; the twisty only toggles.
  treeEl.addEventListener("click", (ev) => {
    const li = ev.target.closest("[role=treeitem]");
    const node = li && byLi.get(li);
    if (!node || !node.row.contains(ev.target)) return;
    if (ev.target.closest(".twisty")) {
      focusItem(node, true);
      run(() => (node.expanded ? collapse(node) : expand(node)));
      return;
    }
    select(node, { focus: true });
    if (expandable(node) && !node.expanded) run(() => expand(node));
  });

  // Keyboard (WAI-ARIA tree): arrows move, Right opens, Left closes or goes
  // to the parent, Enter selects.
  const visible = () => [...treeEl.querySelectorAll("[role=treeitem]")].filter((li) => !li.parentElement.closest("[role=group][hidden]"));
  treeEl.addEventListener("keydown", (ev) => {
    const li = ev.target.closest("[role=treeitem]");
    const node = li && byLi.get(li);
    if (!node) return;
    const vis = visible();
    const i = vis.indexOf(li);
    const go = (el) => { if (el) focusItem(byLi.get(el), true); };
    switch (ev.key) {
      case "ArrowDown": go(vis[i + 1]); break;
      case "ArrowUp": go(vis[i - 1]); break;
      case "Home": go(vis[0]); break;
      case "End": go(vis[vis.length - 1]); break;
      case "ArrowRight":
        if (expandable(node)) {
          if (!node.expanded) run(() => expand(node));
          else if (node.children && node.children.length) focusItem(node.children[0], true);
        }
        break;
      case "ArrowLeft":
        if (expandable(node) && node.expanded) collapse(node);
        else if (node.parent !== root) focusItem(node.parent, true);
        break;
      case "Enter":
      case " ":
        select(node, { focus: true });
        break;
      default:
        return;
    }
    ev.preventDefault();
  });

  // The repository line above the tree selects the root (repository details).
  const rootBtn = h("button", { type: "button", class: "tree-root" }, icon("repository"), h("span", null, r.name));
  rootBtn.addEventListener("click", () => select(root));
  put(view, head,
    h("div", { class: "browser" },
      h("section", { class: "card tree-card" }, rootBtn, treeEl, emptyEl, h("p", { class: "notice" }, t("tree.hint"))),
      detail));

  if (path) await reveal(path);
  else {
    await load(root);
    select(root, { keepHash: true });
  }
  if (!focused && root.children && root.children.length) focusItem(root.children[0]);
  settled = true;

  // Back/Forward between selections: restore the node in place instead of
  // re-rendering the whole tree (app.js skips its hashchange handler then).
  if (alive) {
    state.onPop = (rt) => {
      if (!alive || rt.key !== "browse/browse" || rt.params[0] !== name) return false;
      run(async () => {
        restoring = true;
        try {
          if (rt.params[1]) await reveal(rt.params[1]);
          else select(root, { keepHash: true });
        } finally {
          restoring = false;
        }
      });
      return true;
    };
    state.leaving.push(() => { state.onPop = null; });
  }
}
