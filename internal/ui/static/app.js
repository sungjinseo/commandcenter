// Command Center web UI: a dependency-free single-page app in the Command
// Center design (plan 10), with "#browse/..." and "#admin/..." hash routes.
import {
  state, h, put, badSeg, browseLink, searchLink, loading, pageHead, needAdmin, needLogin, hideBanner, handleError, setAuthLostHook, SEARCH_FORMATS,
  mayOpen, readOnlyFor, readOnly,
} from "./util.js";
import { initShell, renderChrome, openLogin, refreshMe, screenTitle } from "./shell.js";
import { t } from "./i18n.js";
import { viewTokens, showSettings } from "./admin.js";
import { viewAccount, viewUserTokens } from "./access.js";
import { viewWelcome } from "./dash.js";
import { viewRepos, viewTree } from "./browse.js";
import { viewSearch, viewComponent } from "./search.js";
import { dropFlash } from "./repoform.js";
import { viewTags } from "./ops.js";
import { viewUploadList, viewUpload } from "./upload.js";

// ADMIN_SCREENS: value is whether the screen takes a ":<id>" parameter.
const ADMIN_SCREENS = {
  "admin/repository/repositories": true,
  "admin/repository/dashboard": true,
  "admin/repository/blobstores": true,
  "admin/repository/selectors": true,
  "admin/repository/cleanuppolicies": true,
  "admin/repository/routingrules": true,
  "admin/repository/staging": false,
  "admin/security/privileges": false,
  "admin/security/roles": true,
  "admin/security/users": true,
  "admin/security/anonymous": false,
  "admin/security/ldap": false,
  "admin/security/realms": false,
  "admin/security/sslcertificates": false,
  "admin/security/oidc": false,
  "admin/security/atlassiancrowd": false,
  "admin/security/saml": false,
  "admin/security/rutauth": false,
  "admin/security/usertoken": false,
  "admin/security/apitokens": false,
  "admin/firewall/settings": false,
  "admin/firewall/quarantine": false,
  "admin/support/logs": false,
  "admin/support/activity": false,
  "admin/support/logging": false,
  "admin/support/metrics": false,
  "admin/support/usage": false,
  "admin/support/supportzip": false,
  "admin/support/systeminformation": false,
  "admin/support/status": true,
  "admin/system/api": false,
  "admin/system/configuration": false,
  "admin/system/emailserver": false,
  "admin/system/http": false,
  "admin/system/nodes": false,
  "admin/system/recovery": true,
  "admin/system/tasks": true,
  "admin/system/webhooks": true,
  "admin/system/general": false,
};

// MOVED maps the route heads of the earlier layout to their homes now; the
// rest of the hash (":<id>", "=<criteria>", "?<query>") is kept.
const MOVED = {
  "admin/security/contentselectors": "admin/repository/selectors",
  "admin/security/certificates": "admin/security/sslcertificates",
  "admin/system/vulnerability": "admin/firewall/settings",
  "admin/repository/quarantine": "admin/firewall/quarantine",
  "admin/repository/tags": "browse/tags",
  "admin/system/support": "admin/support/supportzip",
  "user/user-token": "user/usertoken",
  "admin/security/user-tokens": "admin/security/usertoken",
  "browse/search/generic": "browse/search",
  "browse/search/golang": "browse/search/go",
};

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
  if (a === "tokens") return "#user/apitokens";
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
  if (raw === "user") return { redirect: "#user/account" };
  let m = /^([a-z/-]+)([:=?].*)?$/.exec(raw);
  if (m && Object.prototype.hasOwnProperty.call(MOVED, m[1])) return { redirect: "#" + MOVED[m[1]] + (m[2] || "") };
  // The logs moved off the status screen with their filter links.
  if (raw.startsWith("admin/support/status?")) return { redirect: "#admin/support/logs" + raw.slice("admin/support/status".length) };

  // Search: browse/search[/<custom|format>][=<urlencoded querystring>]
  m = /^browse\/search(?:\/([a-z0-9]+))?(?:=(.*))?$/.exec(raw);
  if (m) {
    if (m[1] !== undefined && m[1] !== "custom" && !Object.prototype.hasOwnProperty.call(SEARCH_FORMATS, m[1])) return { notFound: true };
    const q = m[2] === undefined ? "" : dec(m[2]);
    if (q === null) return { notFound: true };
    return { key: "browse/search" + (m[1] ? "/" + m[1] : ""), criteria: new URLSearchParams(q) };
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

  // Repository operations links carry log filters in the hash query.
  m = /^admin\/support\/logs\?(.*)$/.exec(raw);
  if (m) return { key: "admin/support/logs", params: [], criteria: new URLSearchParams(m[1]) };

  // Accept slash-form repository edit deep links alongside existing colon URLs.
  m = /^admin\/repository\/repositories\/([^/:]+)$/.exec(raw);
  if (m) {
    const name = dec(m[1]);
    if (!name || badSeg(name) || name.includes("/")) return { notFound: true };
    return { key: "admin/repository/repositories", params: [name] };
  }

  // Everything else: <head>[:<param>[:<param>...]], decoded per segment.
  const parts = raw.split(":");
  const head = parts.shift();
  if (head.split("/").some(badSeg)) return { notFound: true };
  const params = parts.map(dec);
  if (params.some((x) => x === null || x === "" || badSeg(x))) return { notFound: true };

  if (head === "browse/welcome" && !params.length) return { key: head, params };
  if (head === "browse/tags" && params.length < 2) return { key: head, params };
  if (head === "browse/browse") {
    if (params.length > 2) params.splice(1, params.length, params.slice(1).join(":"));
    // A path may hold "/" but no "." or ".." segment.
    if (params[1] !== undefined && params[1].split("/").some(badSeg)) return { notFound: true };
    return { key: head, params };
  }
  if (Object.prototype.hasOwnProperty.call(ADMIN_SCREENS, head)) {
    // "repositories:create[:<recipe>]" is the create flow (recipe picker, form).
    const max = head === "admin/repository/repositories" && params[0] === "create" ? 2 : (ADMIN_SCREENS[head] ? 1 : 0);
    if (params.length > max) return { notFound: true };
    return { key: head, params };
  }
  if ((head === "user/account" || head === "user/usertoken" || head === "user/apitokens") && !params.length) return { key: head, params };
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
    case "browse/tags":
      if (state.me.anonymous) return needLogin(view, t("nav.tags"));
      if (p0 === "create" && !state.me.admin) return needAdmin(view, t("nav.tags"));
      return viewTags(view, p0);
    case "browse/search": return viewSearch(view, "generic", r.criteria);
    case "user/account": return viewAccount(view);
    case "user/usertoken": return viewUserTokens(view, false);
    case "user/apitokens": return viewTokens(view, false);
  }
  if (r.key.startsWith("browse/search/")) return viewSearch(view, r.key.slice("browse/search/".length), r.criteria);
  // Delegated privileges open some screens to non-admins (the server decides).
  if (!mayOpen(r.key)) return needAdmin(view, screenTitle(r.key));
  const out = showSettings(view, r.key, p0, p1, r.criteria);
  if (out) {
    if (!readOnlyFor(r.key)) return out;
    await out;
    if (view.isConnected) readOnly(view);
    return;
  }
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
    history.replaceState(null, "", r.redirect); // no history entry and no hashchange: route the new hash now
    return route();
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
