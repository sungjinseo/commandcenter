// Command Center web UI: a dependency-free single-page app in the Command
// Center design (plan 10), with "#browse/..." and "#admin/..." hash routes.
import {
  state, h, put, badSeg, browseLink, searchLink, loading, pageHead, needAdmin, hideBanner, handleError, setAuthLostHook,
} from "./util.js";
import { initShell, renderChrome, openLogin, refreshMe } from "./shell.js";
import { t } from "./i18n.js";
import { viewTokens, showSettings } from "./admin.js";
import { viewAccount } from "./access.js";
import { viewWelcome } from "./dash.js";
import { viewRepos, viewTree } from "./browse.js";
import { viewSearch, viewComponent } from "./search.js";
import { dropFlash } from "./repoform.js";
import { viewUploadList, viewUpload } from "./upload.js";

const SEARCH_KINDS = ["generic", "custom", "cargo", "golang", "maven", "npm", "pypi", "docker"];

const ADMIN_SCREENS = {
  "admin/repository/repositories": true,
  "admin/security/users": true,
  "admin/security/roles": true,
  "admin/security/realms": false,
  "admin/security/usertoken": false,
  "admin/support/status": true,
  "admin/support/systeminformation": false,
  "admin/system/tasks": true,
  "admin/system/backup": false,
  "admin/repository/cleanuppolicies": true,
  "admin/repository/blobstores": true,
  "admin/system/general": false,
}; // value: whether the screen takes a ":<id>" parameter

function dec(x) {
  try { return decodeURIComponent(x); } catch (e) { return null; }
}

// legacy maps a plan 7 route ("#/...") to its current equivalent (Decision 1).
function legacy(raw) {
  let query = "";
  const i = raw.indexOf("?");
  if (i >= 0) { query = raw.slice(i + 1); raw = raw.slice(0, i); }
  const segs = raw.split("/").filter((x) => x !== "").map(dec);
  if (segs.some((x) => x === null || badSeg(x))) return null;
  const [a, name, kind, ...rest] = segs;
  if (!a) return "#browse/welcome";
  if (a === "r" && name) {
    if (!kind || kind === "pkg") return browseLink(name);
    if (kind === "browse" || kind === "asset") return browseLink(name, rest.join("/"));
    return null;
  }
  if (segs.length !== 1) return null;
  if (a === "search") {
    const p = new URLSearchParams(query);
    const c = {};
    if (p.get("q")) c.keyword = p.get("q");
    if (p.get("repo")) c.repo = p.get("repo");
    return searchLink("generic", Object.keys(c).length ? c : null);
  }
  if (a === "tokens") return "#user/usertoken";
  if (a === "admin") return "#admin/support/status";
  if (a === "login") return "#login";
  return null;
}

// parse turns location.hash into { key, params, criteria } or
// { redirect } or { notFound }.
export function parse(hash) {
  const raw = hash.replace(/^#/, "");
  if (raw === "" || raw.startsWith("/")) {
    const to = legacy(raw);
    return to ? { redirect: to } : { notFound: true };
  }
  if (raw === "login") return { key: "login" };
  if (raw === "admin") return { redirect: "#admin/repository/repositories" };
  if (raw === "browse") return { redirect: "#browse/welcome" };
  if (raw === "browse/search") return { redirect: "#browse/search/generic" };

  // Search: browse/search/<kind>[=<urlencoded querystring>]
  let m = /^browse\/search\/([a-z]+)(?:=(.*))?$/.exec(raw);
  if (m) {
    if (!SEARCH_KINDS.includes(m[1])) return { notFound: true };
    const q = m[2] === undefined ? "" : dec(m[2]);
    if (q === null) return { notFound: true };
    return { key: "browse/search/" + m[1], criteria: new URLSearchParams(q) };
  }

  // Upload: browse/upload[/<repo>].
  m = /^browse\/upload(?:\/([^/:]+))?$/.exec(raw);
  if (m) {
    const repo = m[1] === undefined ? undefined : dec(m[1]);
    if (repo === null || repo === "" || (repo && badSeg(repo))) return { notFound: true };
    return { key: "browse/upload", params: repo === undefined ? [] : [repo] };
  }

  // Component detail: browse/component:<repo>?group=&name=&version=.
  m = /^browse\/component:([^?]+)\?(.*)$/.exec(raw);
  if (m) {
    const repo = dec(m[1]);
    const criteria = new URLSearchParams(m[2]);
    if (!repo || badSeg(repo) || repo.includes("/") || dec(m[2]) === null ||
        !criteria.get("name") || !criteria.get("version")) return { notFound: true };
    return { key: "browse/component", params: [repo], criteria };
  }

  // Everything else: <head>[:<param>[:<param>...]], decoded per segment.
  const parts = raw.split(":");
  const head = parts.shift();
  if (head.split("/").some(badSeg)) return { notFound: true };
  const params = parts.map(dec);
  if (params.some((x) => x === null || x === "" || badSeg(x))) return { notFound: true };

  if (head === "browse/welcome" && !params.length) return { key: head, params };
  if (head === "browse/browse") {
    if (params.length > 2) params.splice(1, params.length, params.slice(1).join(":"));
    // A path may hold "/" but no "." or ".." segment.
    if (params[1] !== undefined && params[1].split("/").some(badSeg)) return { notFound: true };
    return { key: head, params };
  }
  if (Object.prototype.hasOwnProperty.call(ADMIN_SCREENS, head)) {
    // "repositories:create[:<recipe>]" is the create flow (recipe picker, form).
    // "tasks:create[:<type>]" preselects the task type (the backup page links it).
    const max = (head === "admin/repository/repositories" || head === "admin/system/tasks") && params[0] === "create" ? 2 : (ADMIN_SCREENS[head] ? 1 : 0);
    if (params.length > max) return { notFound: true };
    return { key: head, params };
  }
  if ((head === "user/account" || head === "user/usertoken") && !params.length) return { key: head, params };
  return { notFound: true };
}

// ---- screens ----------------------------------------------------------------

function viewNotFound(view) {
  put(view, pageHead(t("notFound.title")),
    h("section", { class: "card" }, h("p", null, t("notFound.text")),
      h("a", { href: "#browse/welcome" }, t("notFound.home"))));
}

async function show(view, r) {
  const [p0, p1] = r.params || [];
  switch (r.key) {
    case "browse/welcome": return viewWelcome(view);
    case "login":
      await viewWelcome(view);
      if (state.me.anonymous) openLogin();
      else history.replaceState(null, "", "#browse/welcome");
      return;
    case "browse/browse":
      if (p0 === undefined) return viewRepos(view);
      return viewTree(view, p0, p1);
    case "browse/component": return viewComponent(view, p0, r.criteria);
    case "browse/upload": return p0 === undefined ? viewUploadList(view) : viewUpload(view, p0);
    case "user/account": return viewAccount(view);
    case "user/usertoken": return viewTokens(view, false);
  }
  if (r.key.startsWith("browse/search/")) return viewSearch(view, r.key.slice("browse/search/".length), r.criteria);
  if (!state.me.admin) {
    const titles = { "admin/repository/repositories": t("nav.repositories"), "admin/security/users": t("nav.users"), "admin/security/roles": t("nav.roles"), "admin/security/realms": t("nav.realms"), "admin/security/usertoken": t("nav.tokens"),
      "admin/support/status": t("nav.status"), "admin/support/systeminformation": t("nav.sysinfo"), "admin/system/tasks": t("nav.tasks"), "admin/system/backup": t("nav.backup"),
      "admin/repository/cleanuppolicies": t("nav.cleanup"), "admin/repository/blobstores": t("nav.blobstores"), "admin/system/general": t("nav.general") };
    return needAdmin(view, titles[r.key] || t("need.admin"));
  }
  const out = showSettings(view, r.key, p0, p1);
  if (out) return out;
  return viewNotFound(view);
}

// ---- router -------------------------------------------------------------------

let seq = 0;

// chromeOf is what the sidebar and breadcrumb need of a route.
const chromeOf = (r) => r.key ? { key: r.key, params: r.params, criteria: r.criteria } : { key: "" };

async function route() {
  state.pendingLang = false; // this render shows the current language
  for (const fn of state.leaving) { try { fn(); } catch (e) { /* ignore */ } }
  state.leaving = [];
  dropFlash();
  const r = parse(location.hash);
  if (r.redirect) {
    location.replace(r.redirect);
    return;
  }
  hideBanner();
  renderChrome(chromeOf(r));
  const n = ++seq;
  const view = h("div", { class: "view" }, loading());
  put(document.getElementById("view"), view);
  try {
    if (r.notFound) viewNotFound(view);
    else await show(view, r);
  } catch (e) {
    if (view.isConnected && n === seq) {
      put(view, h("div", { class: "empty" }, e && e.status === 404 ? t("route.notFound") : t("route.loadFailed")));
      handleError(e);
    }
  }
}

setAuthLostHook(() => renderChrome(chromeOf(parse(location.hash))));
initShell(route);
// Back/Forward fire popstate and then hashchange. The tree browser restores its
// selection in place on popstate; the hashchange that follows is then skipped.
// popHandled holds the hash the tree already restored; the hashchange for that
// hash is skipped (compared by value, not by timer ordering).
let popHandled = null;
window.addEventListener("popstate", () => {
  popHandled = state.onPop && state.onPop(parse(location.hash)) ? location.hash : null;
});
window.addEventListener("hashchange", () => {
  const skip = popHandled !== null && popHandled === location.hash;
  popHandled = null;
  if (!skip) route();
});
document.querySelector(".skip").addEventListener("click", (ev) => {
  ev.preventDefault();
  document.getElementById("main").focus();
});
refreshMe().then(route);
