// Shared helpers for the Command Center web UI.
// Every value from the API is attacker-controlled (paths, package names), so
// the DOM is built only with h(): text goes through text nodes, attributes
// through setAttribute, and href values through safeURL. Icons are built
// with createElementNS from the static path table below, never from data.

import { t, locale } from "./i18n.js";

export const state = {
  me: { user: "", admin: false, anonymous: true },
  repos: null, // last /-/api/repositories list, or null when not loaded
  leaving: [], // cleanup callbacks of the current route
  pendingLang: false, // a language change waits for the busy screen to finish
  idle: null, // set by the shell: re-renders the view once nothing is busy
  onPop: null, // set by the tree browser: (route) => true when it restored the selection in place
};

export const ANON = () => ({ user: "", admin: false, anonymous: true });

// ---- DOM helpers ---------------------------------------------------------

// safeURL allows only same-origin targets: a "#fragment", or any URL that
// resolves to location.origin. Anything else, such as javascript:, data:,
// "//other.site" or another site, becomes "#".
export function safeURL(u) {
  u = String(u);
  if (u.startsWith("#")) return u;
  try {
    // Resolve like the browser will (it strips tabs/newlines, so "/\t/x"
    // becomes "//x") and compare origins.
    return new URL(u, location.href).origin === location.origin ? u : "#";
  } catch (e) {
    return "#";
  }
}

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    // An on* key is only ever a listener: a string value would become an
    // inline handler (blocked by the CSP, but never set it), so skip it.
    if (/^on/i.test(k)) {
      if (typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
      continue;
    }
    if (k === "href") el.setAttribute("href", safeURL(v));
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  append(el, children);
  return el;
}

export function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

// put replaces el's children. Unlike replaceChildren it skips null and
// false and flattens arrays, so optional parts can be passed inline.
export function put(el, ...children) {
  el.replaceChildren();
  append(el, children);
}

const SVGNS = "http://www.w3.org/2000/svg";
export function s(tag, attrs) {
  const el = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
  return el;
}

// ---- icons ---------------------------------------------------------------

// 24x24 stroke icons: each entry is a list of path "d" strings. The
// navigation set (dashboard, repos, search, upload, settings, blob, cleanup,
// tasks, users, roles, realms, health, guide) follows the project's own design
// concept (its rects, circles and ellipses are
// written as paths).
const ICONS = {
  menu: ["M4 6h16", "M4 12h16", "M4 18h16"],
  search: ["M4 10a6 6 0 1 0 12 0a6 6 0 1 0-12 0z", "M15 15l6 6"],
  dashboard: ["M4 3h5a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z", "M15 3h5a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1h-5a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z",
    "M4 14h5a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-5a1 1 0 0 1 1-1z", "M15 14h5a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1h-5a1 1 0 0 1-1-1v-5a1 1 0 0 1 1-1z"],
  repos: ["M4 5a8 3 0 1 0 16 0a8 3 0 1 0-16 0z", "M4 5v14c0 4 16 4 16 0V5", "M4 12c0 4 16 4 16 0"],
  browse: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  settings: ["M4 6h16", "M4 12h16", "M4 18h16", "M6 6a2 2 0 1 0 4 0a2 2 0 1 0-4 0z", "M14 12a2 2 0 1 0 4 0a2 2 0 1 0-4 0z", "M8 18a2 2 0 1 0 4 0a2 2 0 1 0-4 0z"],
  blob: ["M4 4h16v16H4z", "M4 10h16", "M8 7h.1", "M8 15h.1"],
  cleanup: ["M3 7h18", "M8 7V4h8v3", "M6 7v14h12V7", "M10 11v6", "M14 11v6"],
  repository: ["M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3s-3.6 3-8 3s-8-1.3-8-3z", "M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6", "M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"],
  users: ["M5 7a4 4 0 1 0 8 0a4 4 0 1 0-8 0z", "M2 21v-3a7 7 0 0 1 14 0v3", "M18 3a4 4 0 0 1 0 8", "M20 21v-3a7 7 0 0 0-3-6"],
  roles: ["M12 3l8 4v6c0 4-8 8-8 8s-8-4-8-8V7l8-4z", "M8 12l3 3l5-6"],
  realms: ["M10 13a5 5 0 0 1 0-7l3-3a5 5 0 0 1 7 7l-3 3", "M14 11a5 5 0 0 1 0 7l-3 3a5 5 0 0 1-7-7l3-3"],
  health: ["M2 12h5l3-8l4 16l3-8h5"],
  guide: ["M3 4h8l1 2l1-2h8v16h-8l-1 1l-1-1H3V4z", "M12 6v15"],
  user: ["M12 4a4 4 0 1 0 0 8a4 4 0 1 0 0-8z", "M4 21c.8-4 4-6.5 8-6.5s7.2 2.5 8 6.5"],
  token: ["M8 10a4 4 0 1 0 0 4", "M12 12h9", "M18 12v3", "M21 12v2"],
  status: ["M2 12h5l3-8l4 16l3-8h5"],
  info: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z", "M12 11v6", "M12 7.5v.5"],
  tasks: ["M6 4h12a2 2 0 0 1 2 2v13a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z", "M8 2v4", "M16 2v4", "M4 9h16", "M8 13h4", "M8 17h8"],
  refresh: ["M20 11a8 8 0 1 0-2.3 5.7", "M20 4v7h-7"],
  help: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z", "M9.5 9.5a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .9-1 1.6v.6", "M12 17v.5"],
  back: ["M15 5l-7 7l7 7"],
  chevron: ["M9 6l6 6l-6 6"],
  caret: ["M6 9l6 6l6-6"],
  close: ["M6 6l12 12", "M18 6L6 18"],
  folder: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  file: ["M6 3h8l4 4v14H6z", "M14 3v4h4"],
  signout: ["M10 4H5v16h5", "M14 8l4 4l-4 4", "M18 12H9"],
  custom: ["M4 6h10", "M18 6h2", "M4 12h4", "M12 12h8", "M4 18h12", "M20 18h0", "M16 4v4", "M10 10v4", "M18 16v4"],
  box: ["M12 3l8 4.5v9L12 21l-8-4.5v-9z", "M4 7.5l8 4.5l8-4.5", "M12 12v9"],
  ok: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z", "M8 12.5l2.7 2.7L16 9.8"],
  upload: ["M12 16V3", "M7 8l5-5l5 5", "M4 15v6h16v-6"],
  bad: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z", "M9 9l6 6", "M15 9l-6 6"],
  sun: ["M8 12a4 4 0 1 0 8 0a4 4 0 1 0-8 0z", "M12 2v2", "M12 20v2", "M2 12h2", "M20 12h2", "M4.9 4.9l1.4 1.4", "M17.7 17.7l1.4 1.4", "M4.9 19.1l1.4-1.4", "M17.7 6.3l1.4-1.4"],
  moon: ["M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"],
  monitor: ["M3 5h18v11H3z", "M8 20h8", "M12 16v4"],
  cert: ["M4 4h16v12H4z", "M8 8h8", "M8 11h5", "M15 16v5l2-1.5l2 1.5v-5"],
  lifebuoy: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z", "M12 8a4 4 0 1 0 0 8a4 4 0 1 0 0-8z", "M5.6 5.6l3.6 3.6", "M14.8 14.8l3.6 3.6", "M18.4 5.6l-3.6 3.6", "M9.2 14.8l-3.6 3.6"],
};

// icon returns an SVG element for one of the names above; unknown names
// give an empty icon. The name is always a literal at the call site.
export function icon(name, cls) {
  const svg = s("svg", { viewBox: "0 0 24 24", class: "ico" + (cls ? " " + cls : ""), "aria-hidden": "true", focusable: "false" });
  const paths = Object.prototype.hasOwnProperty.call(ICONS, name) ? ICONS[name] : [];
  for (const d of paths) svg.appendChild(s("path", { d }));
  return svg;
}

// ---- formatting ----------------------------------------------------------

export function fmtBytes(n) {
  n = Number(n) || 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? String(n) : n.toFixed(n < 10 ? 1 : 0)) + " " + units[i];
}
export const fmtNum = (n) => (Number(n) || 0).toLocaleString(locale());
export function fmtDate(v) {
  if (!v) return "-";
  const d = new Date(v);
  return isNaN(d) ? String(v) : d.toLocaleString(locale(), { hour12: false });
}
export function fmtUptime(sec) {
  sec = Math.floor(Number(sec) || 0);
  const d = Math.floor(sec / 86400), hh = Math.floor(sec % 86400 / 3600), mm = Math.floor(sec % 3600 / 60);
  if (d) return t("time.dh", { d, h: hh });
  if (hh) return t("time.hm", { h: hh, m: mm });
  return t("time.ms", { m: mm, s: sec % 60 });
}

export const FORMATS = ["maven", "npm", "pypi", "go", "cargo", "docker", "raw", "helm", "nuget", "apt", "yum", "rubygems", "conda", "composer", "conan", "r", "gitlfs", "p2", "swift", "cocoapods", "huggingface", "terraform"];
const TYPES = { hosted: "hosted", proxy: "proxy", group: "group" };
export const fmtBadge = (f) => h("span", { class: "badge" + (FORMATS.includes(f) ? " f-" + f : "") }, f);
export const typeBadge = (ty) => h("span", { class: "badge" + (TYPES[ty] ? " t-" + ty : "") }, ty);
export const FORMAT_NAMES = { maven: "Maven", npm: "npm", pypi: "PyPI", go: "Go", cargo: "Cargo", docker: "Docker", raw: "Raw", helm: "Helm", nuget: "NuGet", apt: "APT", yum: "Yum", rubygems: "RubyGems", conda: "Conda", composer: "Composer", conan: "Conan", r: "R", gitlfs: "Git LFS", p2: "p2", swift: "Swift", cocoapods: "CocoaPods", huggingface: "Hugging Face", terraform: "Terraform" };
const TILES = { maven: "M", npm: "npm", pypi: "py", go: "go", cargo: "rs", docker: "dk", raw: "raw", helm: "hm", nuget: "nu", apt: "deb", yum: "rpm", rubygems: "gem", conda: "cn", composer: "php", conan: "co", r: "R", gitlfs: "lfs", p2: "p2", swift: "sw", cocoapods: "pod", huggingface: "hf", terraform: "tf" };
// fmtTile is the concept's coloured format tile.
export const fmtTile = (f) => h("span", { class: "format" + (FORMATS.includes(f) ? " " + f : ""), "aria-hidden": "true" },
  FORMATS.includes(f) ? TILES[f] : String(f || "?").slice(0, 1));
// nameCell is a tile, a main line and an optional small second line.
export const nameCell = (f, main, sub) => h("div", { class: "namecell" }, fmtTile(f), h("span", null, main, sub ? h("small", null, sub) : null));
export const onlineBadge = (online) => (online === false ? h("span", { class: "badge red" }, h("span", { class: "dot", "aria-hidden": "true" }), t("repo.down")) : h("span", { class: "badge green" }, h("span", { class: "dot", "aria-hidden": "true" }), t("repo.up")));

// ---- concept building blocks ------------------------------------------------

// card is a concept card: a head with title, optional subtitle and actions,
// then a padded body; body null leaves the body out (for a table).
export function card(title, sub, body, act) {
  return h("section", { class: "card" },
    h("header", { class: "card-head" }, h("div", null, h("h2", null, title), sub ? h("p", null, sub) : null), act || null),
    body === null ? null : h("div", { class: "card-body" }, body));
}
export const kv = (k, v) => h("div", { class: "kv" }, h("span", null, k), h("span", null, v === "" || v === null || v === undefined ? "—" : v));

// bar is the concept's thin meter; the width is set through CSSOM, which
// the CSP allows (no style attribute).
export function bar(pct, color) {
  const i = h("i");
  i.style.width = Math.max(0.5, Math.min(100, Number(pct) || 0)) + "%";
  return h("div", { class: "bar" + (color ? " " + color : "") }, i);
}

// stat is a dashboard tile; pct (0-100) draws the bar, omit it for none.
export function stat(o) {
  return h("div", { class: "stat" },
    h("div", { class: "stat-label" }, o.label),
    h("div", { class: "stat-value" }, o.value, o.unit ? h("small", null, o.unit) : null),
    o.pct === undefined ? null : bar(o.pct, o.color),
    h("div", { class: "stat-foot" }, h("span", null, o.foot || ""), h("span", null, o.right || "")));
}

// tbl is the concept table (.tablewrap), for cards without padding.
export function tbl(headers, rows, emptyText) {
  return h("div", { class: "tablewrap" }, rows.length ? h("table", null,
    h("thead", null, h("tr", null, headers.map((x) => typeof x === "string" ? h("th", { scope: "col" }, x) : h("th", { scope: "col", class: x.cls }, x.t)))),
    h("tbody", null, rows)) : h("div", { class: "empty" }, emptyText || t("common.noItems")));
}

// ---- paths ---------------------------------------------------------------

export const enc = encodeURIComponent;
// "." and ".." would let a crafted hash climb out of the API path, so they
// are rejected here and the router answers "not found" for them.
export const badSeg = (x) => x === "." || x === "..";
export function encSeg(x) {
  if (badSeg(x)) throw new APIError(404, "Not found");
  return enc(x);
}
export const encPath = (p) => p.split("/").map(encSeg).join("/");
export const apiRepo = (name, rest) => "repositories/" + encSeg(name) + (rest || "");
export function parentDir(path) {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

// Hash routes for browse and admin screens.
export const browseLink = (repo, path) => "#browse/browse" + (repo ? ":" + enc(repo) + (path ? ":" + enc(path) : "") : "");
export const componentLink = (c, from) => {
  const q = new URLSearchParams({ group: c.group || "", name: c.name, version: c.version });
  if (from) q.set("from", from);
  return "#browse/component:" + enc(c.repo) + "?" + q;
};
// Upload (plan 9 Decision 8): hosted maven, npm and pypi repositories the user can write.
export const canUpload = (r) => r.type === "hosted" && ["maven", "npm", "pypi", "raw", "helm"].includes(r.format) && !!r.canWrite;
export const uploadLink = (repo) => "#browse/upload" + (repo ? "/" + enc(repo) : "");
// repoBase is the same-origin path of a repository: baseURL may name another
// host than the browser uses, and the session cookie needs the same origin.
export function repoBase(r) {
  try { return new URL(r.url, location.href).pathname; } catch (e) { return "/repository/" + enc(r.name) + "/"; }
}
export const adminRepoLink = (repo) => "#admin/repository/repositories" + (repo ? ":" + enc(repo) : "");
export const searchLink = (kind, params) => {
  const q = params ? new URLSearchParams(params).toString() : "";
  return "#browse/search/" + kind + (q ? "=" + enc(q) : "");
};

// ---- API -----------------------------------------------------------------

export class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

export async function api(path, opts) {
  opts = opts || {};
  const init = {
    method: opts.method || "GET",
    credentials: "same-origin",
    headers: { "X-Command-Center-UI": "1", Accept: "application/json" },
  };
  if (opts.body !== undefined) {
    init.body = JSON.stringify(opts.body);
    init.headers["Content-Type"] = "application/json";
  }
  let res;
  try {
    res = await fetch("/-/api/" + path, init);
  } catch (e) {
    throw new APIError(0, t("err.connect"));
  }
  if (res.status === 204) return null;
  let data = null;
  try { data = await res.json(); } catch (e) { /* not JSON */ }
  if (!res.ok) {
    const err = new APIError(res.status, (data && data.error) || res.status + " " + res.statusText);
    if (data && Array.isArray(data.errors)) err.errors = data.errors.map(String); // validation list (400)
    err.data = data; // the whole reply, e.g. the repositories a 409 names
    throw err;
  }
  return data;
}

// loadRepos caches the visible repository list for the sidebar and screens.
export async function loadRepos(force) {
  if (state.repos && !force) return state.repos;
  try {
    const d = await api("repositories");
    state.repos = d.repositories || [];
  } catch (e) {
    state.repos = [];
  }
  return state.repos;
}

// ---- banner --------------------------------------------------------------

const hooks = { onAuthLost: () => {} };
export function setAuthLostHook(fn) { hooks.onAuthLost = fn; }

function bannerEl() { return document.getElementById("banner"); }

export function hideBanner() {
  const b = bannerEl();
  if (!b) return;
  b.hidden = true;
  b.replaceChildren();
}

export function showError(msg, withLogin) {
  const b = bannerEl();
  if (!b) return;
  put(b, h("div", { class: "banner-box" },
    h("div", { class: "msg" }, msg, withLogin ? h("a", { href: "#login" }, t("nav.login")) : null),
    h("button", { type: "button", class: "btn small", "aria-label": t("banner.dismiss"), onclick: hideBanner }, t("common.close"))));
  b.hidden = false;
}

export function handleError(err) {
  if (err && err.status === 401) {
    if (!state.me.anonymous) { state.me = ANON(); hooks.onAuthLost(); }
    showError(t("err.loginNeeded"), true);
    return;
  }
  if (err && err.status === 404 && state.me.anonymous) {
    showError(t("err.maybeHidden", { msg: err.message }), true);
    return;
  }
  if (err && err.status === 403) { showError(t("err.denied", { msg: err.message })); return; }
  showError((err && err.message) || String(err));
}

// ---- shared widgets ------------------------------------------------------

// copyButton copies text; without the clipboard API (plain http on a LAN
// address) it selects target's text so the user can press Ctrl+C.
export function copyButton(text, target, label, shown) {
  shown = shown || t("common.copy");
  const btn = h("button", { type: "button", class: "btn small", "aria-label": label || shown }, shown);
  let timer = 0;
  const done = (msg, ok) => {
    btn.textContent = msg;
    btn.classList.toggle("copied", ok);
    clearTimeout(timer);
    timer = setTimeout(() => { btn.textContent = shown; btn.classList.remove("copied"); }, 1500);
  };
  const fallback = () => {
    if (target) {
      const sel = window.getSelection();
      sel.removeAllRanges();
      const r = document.createRange();
      r.selectNodeContents(target);
      sel.addRange(r);
      let ok = false;
      try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
      done(ok ? t("common.copied") : "Ctrl+C", ok);
    } else {
      // Nothing to select: copy from a temporary off-screen textarea.
      const ta = h("textarea", { class: "sr-only", readonly: true, "aria-hidden": "true" });
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
      ta.remove();
      done(ok ? t("common.copied") : t("common.copyFailed"), ok);
    }
  };
  btn.addEventListener("click", () => {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(() => done(t("common.copied"), true), fallback);
    } else {
      fallback();
    }
  });
  return btn;
}

export function copyLine(text, label) {
  const code = h("code", null, text);
  return h("div", { class: "copyline" }, code, copyButton(text, code, label));
}


// pageHead contains the title, an optional one-line description and actions.
export function pageHead(title, sub, right) {
  return h("div", { class: "page-head" },
    h("div", { class: "page-title" }, h("h1", null, title), sub || null), right || null);
}

export const loading = () => h("div", { class: "loading" }, t("common.loading"));
export const truncNote = (text) => h("div", { class: "notice", role: "status" }, text);


// alive registers a route-leave flag: the returned function reports whether
// the screen is still shown, so late replies never touch a newer page.
export function alive() {
  let on = true;
  state.leaving.push(() => { on = false; });
  return () => on;
}

// markBusy marks the current screen as holding in-progress work (an upload,
// a dirty form, a show-once secret): a language change must not rebuild it.
// It returns the function that clears the mark; leaving the route clears it too.
const busy = new Set();
export const isBusy = () => busy.size > 0;
export function markBusy(reason) {
  const k = { reason };
  let done = false;
  busy.add(k);
  const unmark = () => {
    if (done) return;
    done = true;
    busy.delete(k);
    if (!busy.size && state.pendingLang && state.idle) state.idle();
  };
  state.leaving.push(unmark);
  return unmark;
}

// taskName is the task's display name (the built-in compaction is translated).
export const taskName = (x) => (x.id === "compact-blobstore" ? t("task.compact") : x.name);

export function debounce(fn, ms) {
  let timer = 0;
  const d = (...a) => { clearTimeout(timer); timer = setTimeout(() => fn(...a), ms); };
  d.cancel = () => clearTimeout(timer);
  return d;
}

export function needLogin(view, title, iconName) {
  put(view, pageHead(title, null, null, iconName), h("section", { class: "card" },
    h("p", null, t("need.login")),
    h("a", { class: "btn primary", href: "#login" }, t("nav.login"))));
}

// needAdmin: anonymous callers get a login prompt, signed-in non-admins the
// access notice.
export function needAdmin(view, title, iconName) {
  if (state.me.anonymous) return needLogin(view, title, iconName);
  put(view, pageHead(title, null, null, iconName), h("section", { class: "card" },
    h("p", null, t("need.admin")),
    h("p", { class: "sub" }, t("need.adminSub")),
    h("a", { href: "#browse/welcome" }, t("notFound.home"))));
}

// ---- filtered tables (plan 12: shared by the users and token screens) -----------

let flt = 0;
// filterBar is a concept toolbar over table rows whose dataset.key holds
// the text to match: a search box, a live count and a no-match line.
export function filterBar(rows, placeholder, countKey) {
  const id = "flt-" + (++flt);
  const q = h("input", { id, type: "search", placeholder, autocomplete: "off" });
  const count = h("span", { class: "end", role: "status" });
  const none = h("div", { class: "empty", hidden: true }, t("common.noMatch"));
  const apply = () => {
    const v = q.value.trim().toLowerCase();
    let shown = 0;
    for (const tr of rows) {
      tr.hidden = !!v && !tr.dataset.key.includes(v);
      if (!tr.hidden) shown++;
    }
    count.textContent = t(countKey, { n: fmtNum(shown) });
    none.hidden = shown > 0 || !rows.length;
  };
  q.addEventListener("input", apply);
  apply();
  return { bar: h("div", { class: "toolbar" }, h("label", { for: id, class: "sr-only" }, placeholder), q, count), none };
}

// row builds a table row; key is the filter text.
export function row(cells, key) {
  const tr = h("tr", null, cells.map(([v, cls]) => h("td", { class: cls || null }, v)));
  tr.dataset.key = (key || "").toLowerCase();
  return tr;
}
