// The Command Center console shell (plan 10 Decisions 3, 4, 6 and 7): the
// sidebar, the topbar, menus and dialogs, the theme and the language.
import { state, ANON, h, put, api, icon, loadRepos, hideBanner, showError, canUpload, isBusy, fmtNum } from "./util.js";
import { t, getLang, setLang, onLangChange, applyServerDefault } from "./i18n.js";

const hooks = { rerender: () => {} };
const els = {};
let lastRoute = null; // the route the chrome shows, redrawn on a language change
let toastTimer = 0;

// toast shows a short notice at the bottom of the screen.
function toast(text) {
  if (!els.toast) {
    els.toast = h("div", { class: "toast", role: "status" });
    document.body.appendChild(els.toast);
  }
  els.toast.textContent = text;
  els.toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => els.toast.classList.remove("show"), 5000);
}

// ---- storage (best effort: private windows may throw) -----------------------

function getPref(k) {
  try { return localStorage.getItem("commandcenter." + k); } catch (e) { return null; }
}
function setPref(k, v) {
  try {
    if (v === null) localStorage.removeItem("commandcenter." + k);
    else localStorage.setItem("commandcenter." + k, v);
  } catch (e) { /* ignore */ }
}

// ---- theme (Decision 4): light → dark → system ------------------------------

const THEMES = ["light", "dark", "system"];
const THEME_ICON = { light: "sun", dark: "moon", system: "monitor" };
const themeLabel = (m) => (m === "light" ? t("theme.light") : m === "dark" ? t("theme.dark") : t("theme.system"));

function themePref() {
  const v = getPref("theme");
  return v === "light" || v === "dark" ? v : "system";
}

// applyTheme sets <html data-theme>; without it the CSS follows the system.
function applyTheme() {
  const m = themePref();
  if (m === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", m);
  if (els.theme) {
    put(els.theme, icon(THEME_ICON[m]));
    const label = t("theme.title", { mode: themeLabel(m) });
    els.theme.title = label;
    els.theme.setAttribute("aria-label", label);
  }
}
applyTheme(); // as early as possible, before the first paint of the shell

function cycleTheme() {
  const next = THEMES[(THEMES.indexOf(themePref()) + 1) % THEMES.length];
  setPref("theme", next === "system" ? null : next);
  applyTheme();
}

// ---- dialogs ------------------------------------------------------------------

const FOCUSABLE = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

// modal opens a modal dialog in the concept's style. Esc, the overlay and
// the close button close it; Tab is kept inside; focus returns to the opener.
export function modal(title, body, footer, opts) {
  opts = opts || {};
  const opener = opts.returnFocus || document.activeElement;
  const titleId = "dlg-" + Math.random().toString(36).slice(2, 8);
  const closeBtn = h("button", { type: "button", class: "icon-btn", "aria-label": t("dialog.close"), title: t("dialog.close") }, icon("close"));
  const inner = h("div", { class: "modal-inner" },
    h("div", { class: "modalhead" }, h("h2", { id: titleId }, title), closeBtn),
    h("div", { class: "modalbody" }, body),
    footer ? h("div", { class: "modalfoot" }, footer) : null);
  const dlg = h("dialog", { class: "modal" + (opts.cls ? " " + opts.cls : ""), "aria-labelledby": titleId }, inner);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    if (dlg.open) dlg.close();
    dlg.remove();
    if (opts.onClose) opts.onClose();
    if (opener && opener.isConnected && typeof opener.focus === "function") opener.focus();
  };
  closeBtn.addEventListener("click", close);
  dlg.addEventListener("cancel", (ev) => { ev.preventDefault(); close(); });
  // A click on the dialog element itself (not its content) is the overlay.
  // Both the press and the release must be on the overlay, so a drag that
  // starts in a field and ends outside does not close the dialog.
  let downOnBackdrop = false;
  dlg.addEventListener("mousedown", (ev) => { downOnBackdrop = ev.target === dlg; });
  dlg.addEventListener("click", (ev) => {
    const ok = downOnBackdrop && ev.target === dlg;
    downOnBackdrop = false;
    if (ok) close();
  });
  dlg.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") { ev.preventDefault(); close(); return; }
    if (ev.key !== "Tab") return;
    const f = [...dlg.querySelectorAll(FOCUSABLE)].filter((x) => x.offsetParent !== null || x === document.activeElement);
    if (!f.length) { ev.preventDefault(); return; }
    const first = f[0], last = f[f.length - 1];
    if (ev.shiftKey && (document.activeElement === first || !dlg.contains(document.activeElement))) { ev.preventDefault(); last.focus(); }
    else if (!ev.shiftKey && (document.activeElement === last || !dlg.contains(document.activeElement))) { ev.preventDefault(); first.focus(); }
  });
  document.body.appendChild(dlg);
  dlg.showModal();
  const auto = dlg.querySelector("[autofocus]") || dlg.querySelector(FOCUSABLE);
  if (auto) auto.focus();
  return { dlg, close };
}

let loginOpen = null;

export function openLogin() {
  if (loginOpen) return;
  const user = h("input", { id: "login-user", type: "text", name: "username", autocomplete: "username", required: true, autofocus: true });
  const pass = h("input", { id: "login-pass", type: "password", name: "password", autocomplete: "current-password", required: true });
  const msg = h("div", { class: "form-msg", role: "alert" });
  const btn = h("button", { type: "submit", class: "btn primary", form: "login-form" }, t("login.submit"));
  const cancel = h("button", { type: "button", class: "btn" }, t("login.cancel"));
  const form = h("form", { id: "login-form", class: "login-form", novalidate: true },
    h("div", { class: "login-brand" }, h("div", { class: "brandmark" }, brandMark()),
      h("div", null, h("strong", null, t("brand.name")), h("p", { class: "muted" }, t("login.lead")))),
    h("div", { class: "field" }, h("label", { for: "login-user" }, t("login.user")), user),
    h("div", { class: "field" }, h("label", { for: "login-pass" }, t("login.pass")), pass),
    msg,
    h("p", { class: "tiny muted" }, t("login.tokensNote")));
  const m = modal(t("login.title"), form, [cancel, btn], {
    cls: "modal-login",
    onClose: () => {
      loginOpen = null;
      if (location.hash === "#login") history.replaceState(null, "", "#browse/welcome");
    },
  });
  loginOpen = m;
  cancel.addEventListener("click", m.close);
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    msg.textContent = "";
    if (!user.value || !pass.value) {
      msg.textContent = t("login.required");
      (user.value ? pass : user).focus();
      return;
    }
    btn.disabled = true;
    try {
      await api("login", { method: "POST", body: { user: user.value.trim(), password: pass.value } }); // the server refuses names with outer spaces
      await refreshMe();
      hideBanner();
      m.close();
      hooks.rerender();
    } catch (e) {
      msg.textContent = e.status === 401 ? t("login.bad") : e.status === 429 ? t("login.wait", { s: (e.data && e.data.retryAfter) || 60 }) : e.message;
      pass.value = "";
      pass.focus();
    } finally {
      btn.disabled = false;
    }
  });
}

// ---- menus ----------------------------------------------------------------

let openMenu = null;
function closeMenu() {
  if (!openMenu) return;
  openMenu.btn.setAttribute("aria-expanded", "false");
  openMenu.list.hidden = true;
  openMenu = null;
}

// dropdown wires btn to a role=menu list built by build() on every open.
function dropdown(btn, build) {
  const list = h("div", { class: "menu", role: "menu", hidden: true });
  const wrap = h("div", { class: "menu-wrap" }, btn, list);
  btn.setAttribute("aria-haspopup", "menu");
  btn.setAttribute("aria-expanded", "false");
  btn.addEventListener("click", (ev) => {
    ev.stopPropagation();
    if (openMenu && openMenu.list === list) { closeMenu(); return; }
    closeMenu();
    put(list, build());
    list.hidden = false;
    btn.setAttribute("aria-expanded", "true");
    openMenu = { btn, list };
    const first = list.querySelector("[role=menuitem]");
    if (first) first.focus();
  });
  list.addEventListener("click", (ev) => { if (ev.target.closest("[role=menuitem]")) closeMenu(); });
  list.addEventListener("keydown", (ev) => {
    const items = [...list.querySelectorAll("[role=menuitem]")];
    const i = items.indexOf(document.activeElement);
    if (ev.key === "ArrowDown") { ev.preventDefault(); items[(i + 1) % items.length].focus(); }
    else if (ev.key === "ArrowUp") { ev.preventDefault(); items[(i - 1 + items.length) % items.length].focus(); }
    else if (ev.key === "Tab") closeMenu();
  });
  // Esc closes the menu whether focus is on an item or on the button.
  wrap.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && openMenu && openMenu.list === list) { ev.preventDefault(); ev.stopPropagation(); closeMenu(); btn.focus(); }
  });
  return wrap;
}

const menuLink = (href, label, ico) => h("a", { href, role: "menuitem", class: "menu-item" }, icon(ico), h("span", null, label));
function menuButton(label, ico, onclick) {
  return h("button", { type: "button", role: "menuitem", class: "menu-item", onclick }, icon(ico), h("span", null, label));
}

// ---- topbar -----------------------------------------------------------------

const SEARCH_HASH = "#browse/search/generic";

function buildTopbar() {
  const hdr = document.getElementById("hdr");
  els.crumb = h("nav", { class: "crumb" });
  els.resources = h("div", { class: "resource-strip" });

  const q = h("input", { id: "hdr-search", type: "search", autocomplete: "off" });
  els.kbd = h("kbd", { "aria-hidden": "true" }, "/");
  const form = h("form", { class: "global-search", role: "search" }, icon("search"), q, els.kbd);
  form.addEventListener("click", (ev) => { if (ev.target !== q) q.focus(); });
  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    const v = q.value.trim();
    const to = SEARCH_HASH + (v ? "=" + encodeURIComponent("keyword=" + encodeURIComponent(v)) : "");
    if (location.hash === to) hooks.rerender(); // same query: no hashchange, so re-run it
    else location.hash = to;
  });
  els.search = q;
  els.searchForm = form;

  els.theme = h("button", { type: "button", class: "icon-btn" });
  els.theme.addEventListener("click", cycleTheme);
  els.lang = h("button", { type: "button", class: "icon-btn lang-btn" });
  els.lang.addEventListener("click", () => setLang(getLang() === "ko" ? "en" : "ko"));
  els.role = h("span", { class: "top-role" });
  els.who = h("div", { class: "who" });

  put(hdr, h("div", { class: "top-console" }, els.crumb, els.resources),
    h("div", { class: "top-actions" }, form, els.theme, els.lang, els.role, els.who));
  applyTheme();
}

// Only summarize repositories already visible to this session. Groups repeat
// their members' assets, so count files in hosted/proxy repositories once.
function renderResources() {
  const repos = state.repos;
  const files = repos ? repos.filter((r) => r.type !== "group")
    .reduce((n, r) => n + (Number(r.assets) || 0), 0) : null;
  const offline = repos ? repos.filter((r) => r.online === false).length : null;
  const resource = (label, ico, value, warning) => h("div", {
    class: "resource" + (warning ? " warning" : ""), title: label,
  }, icon(ico), h("span", { class: "sr-only" }, label + ": "),
  h("strong", null, value === null ? "—" : fmtNum(value)));
  put(els.resources,
    resource(t("dash.repos"), "repos", repos ? repos.length : null),
    resource(t("col.files"), "file", files),
    resource(t("repo.down"), "status", offline, offline > 0));
}

// applyTexts refreshes the language-dependent text of the static chrome.
function applyTexts() {
  const skip = document.querySelector(".skip");
  if (skip) skip.textContent = t("skip");
  els.crumb.setAttribute("aria-label", t("crumb.aria"));
  els.search.placeholder = t("top.search");
  els.search.setAttribute("aria-label", t("top.search"));
  els.searchForm.title = t("top.searchHint");
  els.lang.textContent = getLang().toUpperCase();
  els.lang.title = t("lang.title");
  els.lang.setAttribute("aria-label", t("lang.title"));
  applyTheme();
}

// renderWho draws the login button or the avatar menu. It keeps the existing
// button while the user and language are unchanged, so focus is not lost on
// every route.
function renderWho() {
  const me = state.me;
  const key = getLang() + (me.anonymous ? ":-" : ":u:" + me.user + ":" + me.admin);
  if (els.whoKey === key) return;
  els.whoKey = key;
  if (me.anonymous) {
    els.role.textContent = "";
    els.role.hidden = true;
    put(els.who, h("button", { type: "button", class: "btn small primary login-btn", onclick: openLogin }, icon("user"), h("span", null, t("nav.login"))));
    return;
  }
  const role = me.admin ? t("user.admin") : t("user.user");
  els.role.textContent = role;
  els.role.hidden = false;
  const initials = (me.user.replace(/[^\p{L}\p{N}]/gu, "").slice(0, 2) || "?").toUpperCase();
  const btn = h("button", { type: "button", class: "avatar", title: me.user, "aria-label": t("user.menu", { user: me.user }) }, initials);
  put(els.who, dropdown(btn, () => [
    h("div", { class: "menu-head", role: "presentation" }, h("strong", null, me.user), h("span", { class: "sub" }, role)),
    menuLink("#user/account", t("nav.account"), "user"),
    menuLink(me.admin ? "#admin/security/usertoken" : "#user/usertoken", t("nav.tokens"), "token"),
    h("div", { class: "menu-sep", role: "separator" }),
    menuButton(t("user.signout"), "signout", signOut),
  ]));
}

// signOut ends the session. A 401 means it was already gone; any other
// failure keeps the signed-in state and reports the error.
export async function signOut() {
  const userBtn = els.who.querySelector("button");
  try {
    await api("logout", { method: "POST" });
  } catch (e) {
    if (e.status !== 401) {
      showError(t("user.signoutFailed", { msg: e.message }));
      if (userBtn && userBtn.isConnected) userBtn.focus();
      return;
    }
  }
  state.me = ANON();
  await loadRepos(true);
  renderWho();
  const loginBtn = els.who.querySelector("button");
  if (loginBtn) loginBtn.focus(); // the Log In button replaces the avatar
  const hsh = location.hash;
  if (hsh.startsWith("#admin") || hsh.startsWith("#user")) location.hash = "#browse/welcome";
  else hooks.rerender();
}

// ---- breadcrumb (Decision 7): 커맨드센터 / <page> / <id> --------------------

const FORMAT_LABELS = { maven: "Maven", npm: "npm", pypi: "PyPI", golang: "Go", cargo: "Cargo", docker: "Docker" };

function pageTitle(key) {
  if (key.startsWith("browse/search/")) return t("nav.search");
  switch (key) {
    case "browse/welcome": return t("nav.dashboard");
    case "browse/component": return t("comp.component");
    case "browse/browse": return t("nav.browse");
    case "browse/upload": return t("nav.upload");
    case "admin/repository/repositories": return t("nav.repositories");
    case "admin/system/tasks": return t("nav.tasks");
    case "admin/system/backup": return t("nav.backup");
    case "admin/system/nexusimport": return t("nav.importNexus");
    case "admin/repository/cleanuppolicies": return t("nav.cleanup");
    case "admin/repository/blobstores": return t("nav.blobstores");
    case "admin/system/general": return t("nav.general");
    case "admin/security/users": return t("nav.users");
    case "admin/security/roles": return t("nav.roles");
    case "admin/security/realms": return t("nav.realms");
    case "admin/security/usertoken": case "user/usertoken": return t("nav.tokens");
    case "admin/support/status": return t("nav.status");
    case "admin/support/systeminformation": return t("nav.sysinfo");
    case "user/account": return t("nav.account");
    case "login": return t("nav.login");
  }
  return t("notFound.title");
}

function renderCrumb(route) {
  const key = route.key || "";
  let id = route.params && route.params.length ? route.params[0] : null;
  if (key.startsWith("browse/search/")) {
    const kind = key.slice("browse/search/".length);
    id = kind === "custom" ? t("nav.searchCustom") : FORMAT_LABELS[kind] || null;
  } else if (key === "admin/repository/repositories" && id === "create") {
    id = t("rf.createTitle");
  } else if (key === "admin/repository/cleanuppolicies" && id === "create") {
    id = t("pol.createTitle");
  } else if (key === "admin/repository/blobstores" && id === "create") {
    id = t("blob.createTitle");
  } else if (key === "admin/system/tasks" && id === "create") {
    id = t("task.createTitle");
  } else if ((key === "admin/security/users" || key === "admin/security/roles") && id === "create") {
    id = t("access.createCrumb");
  } else if (key === "admin/system/tasks" && id === "compact-blobstore") {
    id = t("task.compact");
  }
  const sep = () => h("span", { class: "sep", "aria-hidden": "true" }, "/");
  put(els.crumb,
    h("a", { href: "#browse/welcome" }, t("brand.local")), sep(),
    h("strong", { "aria-current": id ? null : "page" }, pageTitle(key)),
    id ? [sep(), h("span", { class: "crumb-id", "aria-current": "page" }, id)] : null);
}

// ---- instance pill and health dot (admins only) -----------------------------

let healthTimer = 0;
let health = null; // null: unknown, -1: unavailable, n: unhealthy check count

function showHealth() {
  const pill = els.instance;
  pill.classList.toggle("good", state.me.admin && health === 0);
  pill.classList.toggle("bad", state.me.admin && health !== null && health !== 0);
  const host = els.instanceHost.textContent;
  let label = t("instance.title", { host });
  if (state.me.admin && health !== null) {
    label += " · " + (health < 0 ? t("instance.unavailable") : health ? t("instance.unhealthy", { n: health }) : t("instance.healthy"));
  }
  els.instanceState.textContent = !state.me.admin || health === null ? t("instance.unavailable")
    : health < 0 ? t("instance.unavailable") : health ? t("instance.unhealthy", { n: health }) : t("instance.healthy");
  pill.title = label;
  pill.setAttribute("aria-label", label);
}

async function pollHealth() {
  if (!state.me.admin) return;
  try {
    const d = await api("status/checks");
    health = (d.checks || []).filter((c) => !c.healthy).length;
  } catch (e) {
    health = -1;
  }
  showHealth();
}

function syncHealth() {
  if (state.me.admin && !healthTimer) {
    pollHealth();
    healthTimer = setInterval(() => { if (document.visibilityState === "visible") pollHealth(); }, 60000);
  } else if (!state.me.admin && healthTimer) {
    clearInterval(healthTimer);
    healthTimer = 0;
    health = null;
  }
  showHealth();
}

// instanceHost is the host of the configured base URL, as the repository
// URLs carry it; before any repository is visible, the browser's host.
function instanceHost() {
  for (const r of state.repos || []) {
    try { return new URL(r.url, location.href).host; } catch (e) { /* next */ }
  }
  return location.host;
}

// ---- sidebar (Decision 6: real features only) ---------------------------------

// Share the favicon's package-and-hub mark across both brand areas.
function brandMark() {
  return h("img", { src: "/ui/icon.svg", alt: "", "aria-hidden": "true", width: 24, height: 24 });
}

const SEARCH_SUB = [["cargo", "cargo"], ["docker", "docker"], ["go", "golang"], ["maven", "maven"], ["npm", "npm"], ["pypi", "pypi"]];

// navSections lists [title, items]; an item is [href, label, icon, active(key)].
function navSections() {
  const me = state.me;
  const repos = state.repos || [];
  const is = (...keys) => (k) => keys.includes(k);
  const out = [[t("nav.sec.workspace"), [
    ["#browse/welcome", t("nav.dashboard"), "dashboard", is("browse/welcome", "login")],
    ["#browse/browse", t("nav.browse"), "repos", is("browse/browse", "browse/component")],
    ["#browse/search/generic", t("nav.search"), "search", (k) => k.startsWith("browse/search/")],
    repos.some(canUpload) ? ["#browse/upload", t("nav.upload"), "upload", is("browse/upload")] : null,
  ]]];
  if (me.admin) {
    out.push([t("nav.sec.operations"), [
      ["#admin/repository/repositories", t("nav.repositories"), "settings", is("admin/repository/repositories")],
      ["#admin/repository/blobstores", t("nav.blobstores"), "blob", is("admin/repository/blobstores")],
      ["#admin/repository/cleanuppolicies", t("nav.cleanup"), "cleanup", is("admin/repository/cleanuppolicies")],
      ["#admin/system/tasks", t("nav.tasks"), "tasks", is("admin/system/tasks")],
    ]]);
  }
  if (!me.anonymous) {
    out.push([t("nav.sec.access"), [
      me.admin ? ["#admin/security/users", t("nav.users"), "users", is("admin/security/users")] : null,
      me.admin ? ["#admin/security/roles", t("nav.roles"), "roles", is("admin/security/roles")] : null,
      me.admin ? ["#admin/security/realms", t("nav.realms"), "realms", is("admin/security/realms")] : null,
      [me.admin ? "#admin/security/usertoken" : "#user/usertoken", t("nav.tokens"), "token", is("admin/security/usertoken", "user/usertoken")],
    ]]);
  }
  if (me.admin) {
    out.push([t("nav.sec.system"), [
      ["#admin/system/general", t("nav.general"), "settings", is("admin/system/general")],
      ["#admin/system/backup", t("nav.backup"), "archive", is("admin/system/backup")],
      ["#admin/system/nexusimport", t("nav.importNexus"), "inbox", is("admin/system/nexusimport")],
      ["#admin/support/status", t("nav.status"), "health", is("admin/support/status")],
      ["#admin/support/systeminformation", t("nav.sysinfo"), "info", is("admin/support/systeminformation")],
    ]]);
  }
  return out;
}

function navLink(href, label, ico, active, cls) {
  return h("a", { href, class: "nav-link" + (cls ? " " + cls : "") + (active ? " active" : ""), title: label, "aria-current": active ? "page" : null },
    h("span", { class: "ico-wrap" }, icon(ico)), h("span", { class: "navtext" }, label));
}

// searchChildren: the per-format searches, shown under Search while a
// search screen is open.
function searchChildren(key) {
  if (!key.startsWith("browse/search/")) return null;
  const formats = new Set((state.repos || []).map((r) => r.format));
  const items = [["#browse/search/custom", t("nav.searchCustom"), key === "browse/search/custom"]]
    .concat(SEARCH_SUB.filter(([f]) => formats.has(f)).map(([, kind]) =>
      ["#browse/search/" + kind, FORMAT_LABELS[kind], key === "browse/search/" + kind]));
  return h("div", { class: "nav-sub" }, items.map(([href, label, active]) =>
    h("a", { href, class: "nav-sublink" + (active ? " active" : ""), "aria-current": active ? "page" : null }, label)));
}

// The version comes from /-/api/me, which tells it to signed-in users only.
function renderFooter() {
  if (!els.footer) return;
  const v = state.me.anonymous ? "" : String(state.me.version || "");
  put(els.footer,
    h("b", null, t("brand.name")), v ? " · " + (/^[0-9]/.test(v) ? "v" : "") + v : null,
    h("br"), t("footer.openSource"));
}

function renderSidebar(route) {
  const side = document.getElementById("side");
  const key = route.key || "";
  const words = t("brand.name").split(" ");
  // The sidebar is rebuilt on every route: keep focus on the link it was on.
  const focused = side.contains(document.activeElement) ? document.activeElement.getAttribute("href") : null;
  els.instanceHost.textContent = instanceHost();
  const nav = h("nav", { class: "nav-scroll", "aria-label": t("nav.aria") },
    navSections().map(([title, items]) => [
      h("div", { class: "nav-label" }, title),
      h("div", { class: "nav-commands" }, items.filter(Boolean).map(([href, label, ico, active]) => [
        navLink(href, label, ico, active(key)),
        href === SEARCH_HASH ? searchChildren(key) : null,
      ])),
    ]));
  els.footer = h("div", { class: "nav-footer" });
  renderFooter();
  put(side,
    h("a", { class: "brand", href: "#browse/welcome", "aria-label": t("brand.name") },
      h("div", { class: "brandmark" }, brandMark()),
      h("div", { class: "brandtext" }, h("strong", null, words.map((w, i) => [i ? h("br") : null, w])), h("small", null, t("brand.sub")))),
    els.instance, nav, els.footer);
  if (focused) {
    const again = [...side.querySelectorAll("a[href]")].find((a) => a.getAttribute("href") === focused);
    if (again) again.focus();
  }
}

// ---- the "/" shortcut (Decision 7) --------------------------------------------

function typing(el) {
  return !!el && (el.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName));
}

// focusSearch focuses the topbar search, or, where it is hidden (narrow
// screens), opens the search screen and focuses its keyword field.
function focusSearch() {
  if (els.search.offsetParent !== null) {
    els.search.focus();
    els.search.select();
    return;
  }
  const field = () => document.getElementById("sc-keyword");
  if (location.hash.startsWith(SEARCH_HASH) && field()) { field().focus(); return; }
  if (!location.hash.startsWith(SEARCH_HASH)) location.hash = SEARCH_HASH;
  let tries = 0;
  const wait = () => {
    const f = field();
    if (f) f.focus();
    else if (++tries < 60) setTimeout(wait, 50);
  };
  setTimeout(wait, 0);
}

// ---- public -------------------------------------------------------------------

export async function refreshMe() {
  try {
    state.me = await api("me");
  } catch (e) {
    state.me = ANON();
  }
  await loadRepos(true);
  // The server's defaultLanguage applies when this browser has no stored
  // choice; a switch re-renders through the onLangChange listener.
  applyServerDefault(state.me.defaultLanguage);
}

// rerender re-runs the current route (leave callbacks, chrome, view), for a
// screen that must reload itself without a hash change.
export function rerender() { hooks.rerender(); }

// initShell builds the topbar once; rerender re-runs the current route.
export function initShell(rerender) {
  hooks.rerender = rerender;
  els.instanceHost = h("b", null, location.host);
  els.instanceState = h("span", { class: "instance-state" });
  els.instance = h("div", { class: "instance" }, h("i", { class: "hdot", "aria-hidden": "true" }),
    h("span", { class: "instance-copy" }, els.instanceHost, els.instanceState));
  buildTopbar();
  applyTexts();
  state.idle = () => {
    state.pendingLang = false;
    hooks.rerender();
  };
  onLangChange(() => {
    applyTexts();
    if (lastRoute) renderChrome(lastRoute); // the sidebar, breadcrumb and menus change at once
    if (isBusy()) {
      // In-progress work (an upload, a form, a secret) must survive the toggle:
      // the screen follows on the next navigation or when the work is done.
      state.pendingLang = true;
      toast(t("lang.deferred"));
    } else {
      hooks.rerender();
    }
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && healthTimer) pollHealth();
  });
  document.addEventListener("click", (ev) => { if (openMenu && !openMenu.list.parentNode.contains(ev.target)) closeMenu(); });
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "/" || ev.ctrlKey || ev.metaKey || ev.altKey || ev.defaultPrevented) return;
    if (typing(document.activeElement) || document.querySelector("dialog[open]")) return;
    ev.preventDefault();
    closeMenu();
    focusSearch();
  });
}

// renderChrome updates the topbar and sidebar for route.
export function renderChrome(route) {
  lastRoute = route;
  closeMenu();
  renderWho();
  renderResources();
  renderSidebar(route);
  syncHealth();
  renderCrumb(route);
  if (route.key === "browse/search/generic" && route.criteria) els.search.value = route.criteria.get("keyword") || "";
}
