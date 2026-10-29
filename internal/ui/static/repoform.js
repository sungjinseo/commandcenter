// Repository create/edit/delete (plan 9 Decision 7) in the Command Center
// layout (plan 10 Decision 8): typecards for Hosted/Proxy/Group and a format
// select, the savebar, a card per section and the help aside.
//   #admin/repository/repositories:create[:<recipe>]  new repository form
//   #admin/repository/repositories:<name>             edit form
// Settings screens are admin-only; the form checks again so a non-admin
// never sees a create or edit control.
import {
  state, h, put, api, apiRepo, adminRepoLink, browseLink, typeBadge, fmtBytes, fmtNum, FORMAT_NAMES,
  copyLine, pageHead, needAdmin, loadRepos, handleError, card, kv, alive, icon, markBusy,
} from "./util.js";
import { modal, rerender } from "./shell.js";
import { t } from "./i18n.js";

// tk looks up "<base>.<k>" for k from a fixed list (TYPES).
const tk = (base, k) => t(base + "." + k);

const createLink = (recipe) => adminRepoLink("create") + (recipe ? ":" + encodeURIComponent(recipe) : "");

// Recipe names: "maven2" is our "maven" format.
const RECIPE_FORMATS = [["maven2", "maven"], ["npm", "npm"], ["pypi", "pypi"], ["go", "go"], ["cargo", "cargo"], ["docker", "docker"], ["raw", "raw"], ["helm", "helm"], ["nuget", "nuget"], ["apt", "apt"], ["yum", "yum"], ["rubygems", "rubygems"], ["conda", "conda"], ["composer", "composer"], ["conan", "conan"], ["r", "r"], ["gitlfs", "gitlfs"]];
const TYPES = ["hosted", "proxy", "group"];
// Formats the server stores only as hosted (a proxy or group is refused with 400).
const HOSTED_ONLY = ["gitlfs"];
const typesOf = (format) => (HOSTED_ONLY.includes(format) ? ["hosted"] : TYPES);
const RECIPES = RECIPE_FORMATS.flatMap(([label, format]) => typesOf(format).map((type) => ({ id: label + "-" + type, format, type })));
const recipeOf = (format, type) => RECIPES.find((r) => r.format === format && r.type === type);
export const DEFAULT_RECIPE = "maven2-hosted";

// The repositories route reserves "create" for the create form, so a
// repository with that name could never be opened for editing.
const RESERVED = "create";

const DEPLOY = [["allow", "rf.deployAllow"], ["allow_once", "rf.deployDisable"], ["deny", "rf.deployRO"]];
const VERSION = [["release", "Release"], ["snapshot", "Snapshot"], ["mixed", "Mixed"]];
const REMOTE_EXAMPLE = {
  maven: "https://repo1.maven.org/maven2/", npm: "https://registry.npmjs.org/", pypi: "https://pypi.org/",
  go: "https://proxy.golang.org/", cargo: "https://index.crates.io/", docker: "https://registry-1.docker.io", helm: "https://charts.helm.sh/stable/",
  nuget: "https://api.nuget.org/v3/index.json", apt: "https://deb.debian.org/debian/", yum: "https://dl.rockylinux.org/pub/",
  rubygems: "https://rubygems.org/", conda: "https://conda.anaconda.org/conda-forge/", composer: "https://repo.packagist.org/",
  conan: "https://center.conan.io/", r: "https://cloud.r-project.org/",
};
const ANON = [["", "rf.anonInherit"], ["true", "rf.anonAllow"], ["false", "rf.anonDeny"]];

let uidN = 0;
const uid = (p) => "rf-" + p + "-" + (++uidN);
// flash is a one-shot success message {to, text} for the form at hash "to".
let flash = null;
// carry keeps the typed name (and the control to refocus) while the create
// form switches its type or format, which re-runs the route.
let carry = null;

// dropFlash is called by the router on every route change: a message whose
// target is not the new route is never shown later.
export function dropFlash() {
  if (flash && flash.to !== location.hash) flash = null;
}

// field is one concept form field: label, control, optional help. With
// o.group the control is not labelable (a widget), so it is named by role.
function field(label, control, help, o) {
  o = o || {};
  const hid = help ? uid("h") : null;
  let lab = null;
  if (o.group) {
    const lid = uid("l");
    lab = h("span", { class: "flabel", id: lid }, label);
    control.setAttribute("role", "group");
    control.setAttribute("aria-labelledby", lid);
  } else if (label) {
    if (!control.id) control.id = uid("c");
    lab = h("label", { for: control.id }, label, o.req ? h("span", { class: "required", "aria-hidden": "true" }, " *") : null);
  }
  if (hid) control.setAttribute("aria-describedby", hid);
  return h("div", { class: "field" + (o.full ? " full" : "") }, lab, control, help ? h("small", { id: hid }, help) : null);
}

const fields = (...list) => h("div", { class: "fields" }, list.filter(Boolean));

function textInput(value, attrs) {
  return h("input", Object.assign({ id: uid("t"), type: "text", value: value || "", autocomplete: "off", spellcheck: "false" }, attrs || {}));
}

function select(options, value, attrs) {
  const el = h("select", Object.assign({ id: uid("s") }, attrs || {}),
    options.map(([v, label]) => h("option", { value: v }, label)));
  el.value = value;
  return el;
}

function checkbox(label, checked, attrs) {
  const box = h("input", Object.assign({ type: "checkbox", checked: !!checked }, attrs || {}));
  return { box, el: h("label", { class: "check" }, box, label) };
}

// strong renders t(key) with its "{name}" placeholder in bold.
function strong(key, name) {
  const [a, b] = t(key).split("{name}");
  return [a, h("strong", null, name), b];
}

// ---- group members --------------------------------------------------------------

// memberPicker is a two-list member editor. candidates are the
// repositories that may join (same format, not the group itself); members
// keeps its order. onChange runs after every change.
function memberPicker(candidates, initial, onChange) {
  let members = initial.slice();
  const availId = uid("avail"), memId = uid("mem");
  const avail = h("select", { id: availId, multiple: true, size: 8, class: "rf-list" });
  const mem = h("select", { id: memId, multiple: true, size: 8, class: "rf-list" });
  const typeOf = (n) => { const c = candidates.find((x) => x.name === n); return c ? c.type : "?"; };
  const opt = (n, sel) => { const o = h("option", { value: n }, n + " (" + typeOf(n) + ")"); o.selected = sel.has(n); return o; };
  const picked = (el) => [...el.selectedOptions].map((o) => o.value);

  function render(selAvail, selMem) {
    put(avail, candidates.filter((c) => !members.includes(c.name)).map((c) => opt(c.name, selAvail || new Set())));
    put(mem, members.map((n) => opt(n, selMem || new Set())));
    sync();
  }
  function addSel() {
    const p = picked(avail);
    if (!p.length) return;
    members = members.concat(p);
    render(new Set(), new Set(p));
    onChange(members);
  }
  function rmSel() {
    const p = picked(mem);
    if (!p.length) return;
    members = members.filter((n) => !p.includes(n));
    render(new Set(p), new Set());
    onChange(members);
  }
  // move shifts the selected members one place up (-1) or down (+1).
  function move(d) {
    const sel = new Set(picked(mem));
    if (!sel.size) return;
    const m = members.slice();
    const idx = d < 0 ? m.map((_, i) => i) : m.map((_, i) => m.length - 1 - i);
    for (const i of idx) {
      const j = i + d;
      if (sel.has(m[i]) && j >= 0 && j < m.length && !sel.has(m[j])) [m[i], m[j]] = [m[j], m[i]];
    }
    members = m;
    render(new Set(), sel);
    mem.focus();
    onChange(members);
  }
  const btn = (label, aria, fn) => {
    const b = h("button", { type: "button", class: "btn small", "aria-label": aria }, label);
    b.addEventListener("click", fn);
    return b;
  };
  const add = btn(t("rf.add"), t("rf.addAria"), addSel);
  const rm = btn(t("rf.remove"), t("rf.removeAria"), rmSel);
  const up = btn(t("rf.up"), t("rf.upAria"), () => move(-1));
  const down = btn(t("rf.down"), t("rf.downAria"), () => move(1));
  function sync() {
    add.disabled = !avail.selectedOptions.length;
    up.disabled = down.disabled = rm.disabled = !mem.selectedOptions.length;
  }
  avail.addEventListener("change", sync);
  mem.addEventListener("change", sync);
  avail.addEventListener("dblclick", addSel);
  mem.addEventListener("dblclick", rmSel);
  avail.addEventListener("keydown", (ev) => { if (ev.key === "Enter") { ev.preventDefault(); addSel(); } });
  mem.addEventListener("keydown", (ev) => {
    if (ev.altKey && (ev.key === "ArrowUp" || ev.key === "ArrowDown")) { ev.preventDefault(); move(ev.key === "ArrowUp" ? -1 : 1); }
    else if (ev.key === "Enter" || ev.key === "Delete" || ev.key === "Backspace") { ev.preventDefault(); rmSel(); }
  });
  render();
  const el = h("div", { class: "rf-members" },
    h("div", { class: "rf-col" }, h("label", { for: availId }, t("rf.available")), avail),
    h("div", { class: "rf-btns" }, add, rm),
    h("div", { class: "rf-col" }, h("label", { for: memId }, t("rf.members")), mem),
    h("div", { class: "rf-btns" }, up, down));
  return { el, get: () => members.slice() };
}

// ---- delete ---------------------------------------------------------------------

export function deleteDialog(name, isLive) {
  let busy = false; // a request is in flight: typing must not re-enable Delete
  const id = uid("del");
  const input = h("input", { id, type: "text", autocomplete: "off", spellcheck: "false", autofocus: true });
  const msg = h("div", { class: "form-msg", role: "alert" });
  const cancel = h("button", { type: "button", class: "btn" }, t("common.cancel"));
  const del = h("button", { type: "submit", class: "btn danger", disabled: true, form: id + "-f" }, t("rf.delete"));
  const roles = h("div", { class: "notice", hidden: true });
  const form = h("form", { id: id + "-f", novalidate: true },
    h("div", { class: "notice amber" }, strong("rf.delText", name)),
    roles,
    h("div", { class: "field" }, h("label", { for: id }, t("rf.delType")), input),
    msg);
  const m = modal(t("rf.delTitle"), form, [cancel, del], { cls: "modal-small" });
  // The delete also removes the roles' privileges on the repository (QA N-25).
  api("roles").then((d) => {
    const ids = (d.roles || []).filter((r) => (r.privileges || []).some((p) => p.target === "repo:" + name)).map((r) => r.id);
    if (ids.length) { put(roles, t("rf.delRoles", { roles: ids.join(", ") })); roles.hidden = false; }
  }).catch(() => {}); // the delete works without the list
  cancel.addEventListener("click", m.close);
  input.addEventListener("input", () => { del.disabled = busy || input.value !== name; msg.textContent = ""; });
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (busy || input.value !== name) return;
    busy = true;
    del.disabled = true;
    try {
      await api(apiRepo(name), { method: "DELETE" });
    } catch (e) {
      busy = false;
      if (e.status === 401) { m.close(); handleError(e); return; }
      msg.textContent = e.message; // 409 names the groups that use it
      del.disabled = input.value !== name;
      return;
    }
    m.close();
    await loadRepos(true);
    if (isLive()) {
      if (location.hash === adminRepoLink()) rerender();
      else location.hash = adminRepoLink();
    }
  });
}

// ---- form -----------------------------------------------------------------------

// viewRepoForm shows the create form (o.recipe) or the edit form (o.name).
export async function viewRepoForm(view, o) {
  const title = t("nav.repositories");
  if (!state.me.admin) return needAdmin(view, title);
  const isLive = alive();
  const editing = o.name !== undefined;
  const eyebrow = { eyebrow: t("rf.eyebrow") };
  const back = h("a", { class: "btn", href: adminRepoLink() }, t("rf.back"));
  let cfg, info = null;
  const listP = api("repositories");
  listP.catch(() => {}); // awaited below; this only stops an unhandled rejection if an earlier await throws first
  const polP = api("cleanup/policies").catch(() => null); // null: no select, and the stored policy is kept
  const bsP = editing ? null : api("blobstores").catch(() => null); // null: the select offers only "default"
  if (editing) {
    [cfg, info] = await Promise.all([api(apiRepo(o.name, "/config")), api(apiRepo(o.name))]);
  } else {
    const r = RECIPES.find((x) => x.id === o.recipe);
    if (!r) {
      put(view, pageHead(t("rf.createTitle"), null, h("div", { class: "actions" }, back), eyebrow),
        h("section", { class: "card" }, h("div", { class: "empty" }, t("rf.unknownRecipe", { recipe: o.recipe }),
          h("p", null, h("a", { href: createLink() }, t("rf.create"))))));
      return;
    }
    cfg = {
      name: "", format: r.format, type: r.type, remote: r.format === "docker" && r.type === "proxy" ? REMOTE_EXAMPLE.docker : "", remoteAuth: null,
      metadataMaxAge: "24h", contentMaxAge: "-1", negativeCacheTTL: "10m",
      members: [], writeMember: "", versionPolicy: r.format === "maven" && r.type === "hosted" ? "release" : "",
      writePolicy: r.type === "hosted" ? "allow_once" : "", anonymousRead: null,
      distribution: r.format === "apt" && r.type === "hosted" ? "stable" : "", component: r.format === "apt" && r.type === "hosted" ? "main" : "", depth: 0,
    };
  }
  // Routing rule (proxy only): the rule names; null when the list fails, so the stored name is kept.
  const rrP = cfg.type === "proxy" ? api("routing-rules").catch(() => null) : null;
  const all = ((await listP).repositories || []);
  const pols = await polP;
  const rules = rrP ? await rrP : null;
  const stores = bsP ? await bsP : null;
  if (!isLive()) return;
  const isProxy = cfg.type === "proxy", isGroup = cfg.type === "group", isHosted = cfg.type === "hosted";
  const isDocker = cfg.format === "docker";
  const kept = !editing && carry ? carry : null;
  carry = null;

  const errBox = h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
  const okText = flash && flash.to === location.hash ? flash.text : "";
  flash = null;
  const okBox = h("div", { class: "notice ok", role: "status", hidden: !okText }, okText);

  // Basic settings
  const name = textInput(editing ? cfg.name : kept ? kept.name : "", { disabled: editing, required: true, maxlength: 64 });
  const online = checkbox(t("rf.upText"), editing ? cfg.online !== false : true);
  // The blob store is chosen on create (hosted and proxy) and immutable after.
  let bstore = null;
  if (!isGroup && !editing) {
    const names = ((stores && stores.blobstores) || []).map((s) => s.name);
    if (!names.includes("default")) names.unshift("default");
    bstore = select(names.map((n) => [n, n]), kept && names.includes(kept.blobStore) ? kept.blobStore : "default");
  }
  const fmtNames = RECIPE_FORMATS.map(([, f]) => [f, FORMAT_NAMES[f]]);
  const format = editing ? textInput(FORMAT_NAMES[cfg.format] || cfg.format, { readonly: true }) : select(fmtNames, cfg.format);
  // switchTo re-runs the create route for another recipe, keeping the name.
  const switchTo = (fmt, type, focus) => {
    const r = recipeOf(fmt, type) || recipeOf(fmt, "hosted");
    if (!r) return;
    carry = { name: name.value, focus, blobStore: bstore ? bstore.value : "" };
    location.replace(createLink(r.id));
  };
  if (!editing) format.addEventListener("change", () => switchTo(format.value, cfg.type, "format"));

  // Maven
  const versionOpts = isHosted ? VERSION : [["", t("rf.notSet")]].concat(VERSION);
  const version = cfg.format === "maven" && !isGroup ? select(versionOpts, cfg.versionPolicy || (isHosted ? "release" : "")) : null; // groups have none
  const layout = cfg.format === "maven" ? textInput("Strict", { readonly: true }) : null;

  // Hosted
  const deploy = isHosted ? select(isDocker
    ? [["allow", t("docker.allowOverwrite")], ["allow_once", t("docker.preventOverwrite")]]
    : DEPLOY.map(([v, k]) => [v, t(k)]), cfg.writePolicy || "allow_once") : null;

  // APT hosted: distribution, component and the (write-only) signing key; Yum hosted: repodata depth
  const isApt = cfg.format === "apt" && isHosted, isYum = cfg.format === "yum" && isHosted;
  const distribution = isApt ? textInput(cfg.distribution || "stable", { placeholder: "stable", maxlength: 100 }) : null;
  const component = isApt ? textInput(cfg.component || "main", { placeholder: "main", maxlength: 100 }) : null;
  const signingKey = isApt ? h("textarea", { id: uid("sk"), class: "mono", rows: 5, spellcheck: "false", autocomplete: "off",
    placeholder: cfg.signingKeySet ? t("rf.unchanged") : "-----BEGIN PGP PRIVATE KEY BLOCK-----" }) : null;
  const clrKey = isApt && cfg.signingKeySet ? checkbox(t("rf.clearSigningKey"), false) : null;
  if (clrKey) clrKey.box.addEventListener("change", () => { signingKey.disabled = clrKey.box.checked; if (clrKey.box.checked) signingKey.value = ""; });
  const depth = isYum ? select([["0", t("rf.depth0")], ["1", t("rf.depth1")]], String(cfg.depth || 0)) : null;

  // Proxy
  let remote, mdAge, ctAge, negTTL, hidden, user, pass, bearer, clrPass, clrBearer;
  const auth = cfg.remoteAuth || {};
  if (isProxy) {
    remote = textInput(cfg.remote, { type: "url", placeholder: REMOTE_EXAMPLE[cfg.format] || "https://", required: true });
    mdAge = textInput(cfg.metadataMaxAge, { placeholder: "24h" });
    ctAge = textInput(cfg.contentMaxAge, { placeholder: "-1" });
    negTTL = textInput(cfg.negativeCacheTTL, { placeholder: "10m" });
    hidden = cfg.remoteHasHiddenParts ? checkbox(t("rf.hiddenClear"), false) : null;
    user = textInput(auth.username, { autocomplete: "off" });
    pass = h("input", { id: uid("pw"), type: "password", autocomplete: "new-password", placeholder: auth.passwordSet ? t("rf.unchanged") : "" });
    bearer = h("input", { id: uid("bt"), type: "password", autocomplete: "off", placeholder: auth.bearerTokenSet ? t("rf.unchanged") : "" });
    clrPass = auth.passwordSet ? checkbox(t("rf.clearPass"), false) : null;
    clrBearer = auth.bearerTokenSet ? checkbox(t("rf.clearBearer"), false) : null;
    for (const [c, inp] of [[clrPass, pass], [clrBearer, bearer]]) {
      if (c) c.box.addEventListener("change", () => { inp.disabled = c.box.checked; if (c.box.checked) inp.value = ""; });
    }
  }
  const storedRule = cfg.routingRule || "";
  let rule = null, ruleHint = null;
  if (isProxy && rules) {
    const names = (rules.routingRules || []).map((r) => r.name);
    if (storedRule && !names.includes(storedRule)) names.unshift(storedRule);
    if (names.length) rule = select([["", t("rf.none")]].concat(names.map((n) => [n, n])), storedRule);
    else ruleHint = h("div", { class: "field full" }, h("small", null, t("rf.ruleNone"), " ", h("a", { href: "#admin/repository/routingrules" }, t("rf.ruleLink"))));
  }

  // Group
  let picker = null, writable = null;
  if (isGroup) {
    const candidates = all.filter((r) => r.format === cfg.format && r.name !== cfg.name)
      .map((r) => ({ name: r.name, type: r.type })).sort((a, b) => a.name.localeCompare(b.name));
    writable = h("select", { id: uid("wm") });
    const fillWritable = (members, keep) => {
      const hosted = members.filter((m) => { const c = candidates.find((x) => x.name === m); return c && c.type === "hosted"; });
      put(writable, h("option", { value: "" }, t("rf.none")), hosted.map((m) => h("option", { value: m }, m)));
      writable.value = hosted.includes(keep) ? keep : "";
    };
    picker = memberPicker(candidates, cfg.members || [], (m) => fillWritable(m, writable.value));
    fillWritable(cfg.members || [], cfg.writeMember);
  }

  // Cleanup policy (plan 11 Decision 5): hosted and proxy repositories, never
  // Cargo hosted; only the policies for all formats or this one.
  const storedPolicy = cfg.cleanupPolicy || "";
  const cargoHosted = isHosted && cfg.format === "cargo";
  let policy = null;
  if ((isHosted || isProxy) && !cargoHosted && pols) {
    const fit = (pols.policies || []).filter((p) => p.format === "all" || p.format === cfg.format)
      .map((p) => [p.name, p.name + " (" + (p.format === "all" ? t("pol.allFormats") : FORMAT_NAMES[p.format] || p.format) + ")"]);
    if (storedPolicy && !fit.some(([n]) => n === storedPolicy)) fit.unshift([storedPolicy, storedPolicy]);
    policy = select([["", t("rf.noPolicy")]].concat(fit), storedPolicy);
  }
  const cleanupCard = isGroup ? null : card(t("rf.cleanup"), t("rf.cleanupSub"), cargoHosted
    ? h("div", { class: "notice" }, t("pol.cargoNote"))
    : policy ? fields(field(t("rf.policy"), policy, [t("rf.policyHelp"), " ", h("a", { href: "#admin/repository/cleanuppolicies" }, t("rf.policyLink"))], { full: true }))
      : h("div", { class: "notice amber" }, t("rf.policyFailed")));

  const anon = select(ANON.map(([v, k]) => [v, t(k)]), cfg.anonymousRead === true ? "true" : cfg.anonymousRead === false ? "false" : "");

  const setHelp = (isSet, key) => (isSet ? [h("span", { class: "rf-set" }, t(key)), " — ", t("rf.keepEmpty")] : null);
  const authCard = isProxy ? card(t("rf.authTitle"), t("rf.authSub"), fields(
    field(t("rf.username"), user, null, { full: true }),
    field(t("rf.password"), pass, setHelp(auth.passwordSet, "rf.passSet")),
    field(t("rf.bearer"), bearer, setHelp(auth.bearerTokenSet, "rf.bearerSet")),
    clrPass ? h("div", { class: "field" }, clrPass.el) : null,
    clrBearer ? h("div", { class: "field" }, clrBearer.el) : null)) : null;
  const authErr = h("div", { class: "form-errors", hidden: true }, t("rf.hostChanged"));
  if (authCard) authCard.querySelector(".card-body").prepend(authErr);

  const save = h("button", { type: "submit", class: "btn primary" }, editing ? t("rf.save") : t("rf.create"));
  const cancel = h("a", { class: "btn", href: editing ? adminRepoLink(cfg.name) : adminRepoLink() }, editing ? t("rf.discard") : t("common.cancel"));
  if (editing) cancel.addEventListener("click", (ev) => { ev.preventDefault(); rerender(); }); // reload the stored values

  const help = h("aside", { class: "form-help" },
    h("h3", null, t("rf.helpTitle")),
    typesOf(cfg.format).map((ty) => h("p", { class: ty === cfg.type ? "on" : null }, h("b", null, tk("rf.type", ty)), h("br"), tk("rf.typeHelp", ty))),
    h("p", null, t("rf.helpLive")));

  const form = h("form", { class: "rf", novalidate: true },
    h("div", { class: "savebar" }, h("span", null, editing ? t("rf.saveNoteEdit") : t("rf.saveNoteCreate")), h("div", { class: "actions" }, cancel, save)),
    errBox,
    h("div", { class: "form-layout" },
      h("div", { class: "stack" },
        card(t("rf.basic"), t("rf.basicSub"), fields(
          field(t("rf.name"), name, editing ? null : t("rf.nameHelp"), { req: !editing }),
          field(t("col.format"), format),
          field(t("col.type"), textInput(tk("rf.type", cfg.type), { readonly: true })),
          h("div", { class: "field" }, h("span", { class: "flabel" }, t("col.status")), online.el, h("small", null, t("rf.statusHelp"))),
          bstore ? field(t("rf.blobStore"), bstore, t("rf.blobStoreHelp")) : null,
          editing && !isGroup ? field(t("rf.blobStore"), textInput(cfg.blobStore || "default", { readonly: true }), t("rf.blobStoreFixed")) : null,
          info ? field(t("rf.url"), copyLine(info.url, t("rf.copyUrl")), null, { group: true, full: true }) : null)),
        cfg.format === "maven" ? card(t("rf.maven"), t("rf.mavenSub"), fields(
          version ? field(t("rf.version"), version, t("rf.versionHelp")) : null,
          field(t("rf.layout"), layout))) : null,
        isHosted ? card(t("rf.hosted"), t("rf.hostedSub"), fields(
          field(isDocker ? t("docker.writePolicy") : t("rf.deploy"), deploy, isDocker ? t("docker.writePolicyHelp") : t("rf.deployHelp"), { full: true }),
          distribution ? field(t("rf.distribution"), distribution, t("rf.distributionHelp")) : null,
          component ? field(t("rf.component"), component, t("rf.componentHelp")) : null,
          signingKey ? field(t("rf.signingKey"), signingKey, cfg.signingKeySet ? [h("span", { class: "rf-set" }, t("rf.signingKeySet")), " — ", t("rf.keepEmpty")] : t("rf.signingKeyHelp"), { full: true }) : null,
          clrKey ? h("div", { class: "field full" }, clrKey.el) : null,
          depth ? field(t("rf.depth"), depth, t("rf.depthHelp")) : null)) : null,
        isProxy ? card(t("rf.proxy"), t("rf.proxySub"), fields(
          field(t("rf.remote"), remote, t("rf.remoteHelp"), { req: true, full: true }),
          hidden ? h("div", { class: "field full" }, hidden.el, h("small", null, t("rf.hiddenHelp"))) : null,
          field(t("rf.ctAge"), ctAge, t("rf.ageHelp")),
          field(t("rf.mdAge"), mdAge, t("rf.ageHelp")),
          field(t("rf.negTTL"), negTTL, t("rf.negHelp")),
          rule ? field(t("rf.rule"), rule, t("rf.ruleHelp"), { full: true }) : ruleHint)) : null,
        authCard,
        isGroup ? card(t("rf.group"), t("rf.groupSub"), fields(
          field(t("rf.memberRepos"), picker.el, t("rf.membersHelp"), { group: true, full: true }),
          isDocker ? null : field(t("rf.writable"), writable, t("rf.writableHelp"), { full: true }))) : null,
        cleanupCard,
        card(t("rf.access"), t("rf.accessSub"), fields(field(t("rf.anon"), anon, t("rf.anonHelp"), { full: true }))),
        info ? card(t("rf.storage"), t("rf.storageSub"), [
          kv(t("col.files"), fmtNum(info.assets)),
          kv(t("col.size"), fmtBytes(info.bytes) + " (" + t("asset.bytes", { n: fmtNum(info.bytes) }) + ")"),
        ]) : null),
      help));

  function showErrors(list, hostChange) {
    put(errBox, h("strong", null, list.length > 1 ? t("rf.errMany") : t("rf.errOne")),
      h("ul", null, list.map((e) => h("li", null, e))));
    errBox.hidden = false;
    okBox.hidden = true;
    if (authCard) {
      authCard.classList.toggle("invalid", !!hostChange);
      authErr.hidden = !hostChange;
      for (const inp of [user, pass, bearer]) { if (hostChange) inp.setAttribute("aria-invalid", "true"); else inp.removeAttribute("aria-invalid"); }
    }
    errBox.focus();
  }
  function clearErrors() {
    errBox.hidden = true;
    if (authCard) {
      authCard.classList.remove("invalid");
      authErr.hidden = true;
      for (const inp of [user, pass, bearer]) inp.removeAttribute("aria-invalid");
    }
  }

  function body() {
    const b = {
      name: editing ? cfg.name : name.value.trim(), format: cfg.format, type: cfg.type,
      remote: cfg.remote || "", metadataMaxAge: cfg.metadataMaxAge || "", contentMaxAge: cfg.contentMaxAge || "",
      negativeCacheTTL: cfg.negativeCacheTTL || "", members: cfg.members || [], writeMember: cfg.writeMember || "",
      versionPolicy: cfg.versionPolicy || "", writePolicy: cfg.writePolicy || "",
      anonymousRead: anon.value === "" ? null : anon.value === "true",
      // Always sent, so an edit keeps the stored policy when no select is shown.
      cleanupPolicy: policy ? policy.value : storedPolicy,
      online: online.box.checked,
    };
    if (bstore) b.blobStore = bstore.value;
    if (version) b.versionPolicy = version.value;
    if (deploy) b.writePolicy = deploy.value;
    if (isProxy) {
      b.remote = remote.value.trim();
      b.metadataMaxAge = mdAge.value.trim();
      b.contentMaxAge = ctAge.value.trim();
      b.negativeCacheTTL = negTTL.value.trim();
      if (hidden && hidden.box.checked) b.clearRemoteHiddenParts = true;
      b.routingRule = rule ? rule.value : storedRule;
      const a = { username: user.value.trim(), password: pass.value, bearerToken: bearer.value };
      if (clrPass && clrPass.box.checked) a.clearPassword = true;
      if (clrBearer && clrBearer.box.checked) a.clearBearerToken = true;
      if (editing || a.username || a.password || a.bearerToken) b.remoteAuth = a;
    }
    if (isGroup) {
      b.members = picker.get();
      b.writeMember = writable.value;
    }
    if (distribution) {
      b.distribution = distribution.value.trim();
      b.component = component.value.trim();
      b.signingKey = signingKey.value;
      signingKey.value = ""; // the secret never stays in the page
      if (clrKey && clrKey.box.checked) b.clearSigningKey = true;
    }
    if (depth) b.depth = Number(depth.value);
    return b;
  }

  // check catches what the server would reject anyway, before a round trip.
  function check(b) {
    const errs = [];
    if (!b.name) errs.push(t("rf.e.name"));
    else if (/[/\\\s]/.test(b.name)) errs.push(t("rf.e.chars"));
    else if (!editing && b.name === RESERVED) errs.push(t("rf.e.reserved", { name: RESERVED }));
    if (isProxy && !b.remote) errs.push(t("rf.e.remote"));
    if (isGroup && !b.members.length) errs.push(t("rf.e.members"));
    return errs;
  }

  // A form with edits is in-progress work: a language change must not rebuild it.
  let unBusy = null;
  const dirty = () => { if (!unBusy) unBusy = markBusy("repoform"); };
  form.addEventListener("input", dirty);
  form.addEventListener("change", dirty);

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    clearErrors();
    const b = body();
    // Secrets never stay in the page once captured: success re-renders, and
    // a failure (e.g. a host change) asks for them again anyway.
    if (pass) pass.value = "";
    if (bearer) bearer.value = "";
    const errs = check(b);
    if (errs.length) { showErrors(errs); return; }
    save.disabled = true;
    let out;
    try {
      out = editing
        ? await api(apiRepo(cfg.name), { method: "PUT", body: b })
        : await api("repositories", { method: "POST", body: b });
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      const list = (e.errors && e.errors.length) ? e.errors : [e.message];
      showErrors(list, list.some((x) => /remote host changed/i.test(x)));
      return;
    }
    await loadRepos(true);
    if (!isLive()) return;
    const to = adminRepoLink(out.name);
    flash = { to, text: editing ? t("rf.updated") : t("rf.created") };
    if (location.hash === to) rerender(); // same hash: no hashchange, so re-run the route
    else location.hash = to;
  });

  let typecards = null;
  if (!editing) {
    typecards = h("div", { class: "typecards", role: "group", "aria-label": t("col.type") }, typesOf(cfg.format).map((ty) => {
      const b = h("button", { type: "button", class: "typecard" + (ty === cfg.type ? " selected" : ""), "aria-pressed": String(ty === cfg.type), "data-type": ty },
        h("strong", null, tk("rf.type", ty)), h("small", null, tk("rf.typeCard", ty)));
      b.addEventListener("click", () => { if (ty !== cfg.type) switchTo(cfg.format, ty, "type"); });
      return b;
    }));
  }
  let acts = [back];
  if (editing) {
    const del = h("button", { type: "button", class: "btn danger" }, t("rf.delete"));
    del.addEventListener("click", () => deleteDialog(cfg.name, isLive));
    acts = [back, h("a", { class: "btn", href: browseLink(cfg.name) }, icon("browse"), t("repos.browse")), del];
  }

  put(view,
    pageHead(editing ? t("rf.editTitle", { name: cfg.name }) : t("rf.createTitle"),
      h("p", null, editing ? [typeBadge(cfg.type), " ", t("rf.editSub")] : t("rf.createSub")),
      h("div", { class: "actions" }, acts), eyebrow),
    okBox, typecards, !editing && HOSTED_ONLY.includes(cfg.format) ? h("div", { class: "notice" }, t("rf.hostedOnly", { format: FORMAT_NAMES[cfg.format] || cfg.format })) : null, form);
  if (kept && kept.focus === "type") typecards.querySelector(".selected").focus();
  else if (kept && kept.focus === "format") format.focus();
  else if (!editing) name.focus();
}
