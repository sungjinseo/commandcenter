// Access control screens (plan 12 Task 7): users, roles, realms with the
// LDAP settings, and the account page with the password change. The server
// enforces every rule; these screens only collect input and show its answers.
// Passwords are cleared from their inputs as soon as they are read.
//   #admin/security/users[:create|:<id>|:ldap%3A<id>]
//   #admin/security/roles[:create|:<id>]
//   #admin/security/realms
//   #user/account
import {
  state, h, put, api, enc, encSeg, alive, filterBar, row, pageHead, loading, card, kv, tbl, handleError, needLogin, icon,
  loadRepos, FORMATS, FORMAT_NAMES, fmtDate, fmtNum, copyLine, copyButton, markBusy,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete } from "./browse.js";
import { modal } from "./shell.js";
import { field, fields, sel, textIn, button, formState, errList } from "./ops.js";

let uidN = 0;
const uid = (p) => "ac-" + p + "-" + (++uidN);
const dash = "—";
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const acts = (...b) => h("div", { class: "actions" }, b);
const sub = (text) => h("p", null, text);
const notFound = (text) => h("section", { class: "card" }, h("div", { class: "empty" }, text));
const eyebrow = () => ({ eyebrow: t("users.eyebrow") });
const notice = (text, cls) => h("div", { class: "notice" + (cls ? " " + cls : "") }, text);
const errBox = () => h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
const pwIn = () => h("input", { type: "password", autocomplete: "new-password" });
const runes = (s) => [...s].length;
const LOCKOUT = /no admin who can log in/; // pinned to the server text by TestAccessMessagesPinned
const IN_USE = /in use/;
const WRONG_PW = /the current password is incorrect/;
const REENTER = /re-enter the bind password/;
const REENTER_OIDC = /re-enter the client secret/;
const REENTER_CROWD = /re-enter the application password/;
const ACTIONS = ["browse", "read", "add", "edit", "delete"];

const userLink = (id) => "#admin/security/users" + (id ? ":" + enc(id) : "");
const roleLink = (id) => "#admin/security/roles" + (id ? ":" + enc(id) : "");
// dynamic keys go through tables, so every key stays a literal for the i18n test
const REALM_KEYS = { local: ["realms.name.local", "realms.desc.local"], token: ["realms.name.token", "realms.desc.token"], ldap: ["realms.name.ldap", "realms.desc.ldap"], oidc: ["realms.name.oidc", "realms.desc.oidc"],
  crowd: ["realms.name.crowd", "realms.desc.crowd"], rutauth: ["realms.name.rutauth", "realms.desc.rutauth"], saml: ["realms.name.saml", "realms.desc.saml"] };
// isDir: users of these sources live in a directory or identity provider; their
// stored rows are overrides and their owner key is "<source>:<id>".
const isDir = (src) => ["ldap", "oidc", "crowd", "rutauth", "saml"].includes(src);
const realmName = (id) => (REALM_KEYS[id] ? t(REALM_KEYS[id][0]) : id);
const realmDesc = (id) => (REALM_KEYS[id] ? t(REALM_KEYS[id][1]) : "");
const roleBadges = (roles) => (roles && roles.length ? roles.map((r) => badge(r, "blue")) : dash);
const userState = (u) => ((u.status || "active") === "active" ? badge(t("users.active"), "green") : badge(t("users.disabled")));
const adminBadge = (yes) => (yes ? badge(t("users.isAdmin"), "amber") : t("users.no"));
const actHead = () => ({ t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" });

// failWith shows an API failure in the page's error box: a lockout gets its
// own text, sign-in and permission problems the generic banner.
function failWith(fs, e, head) {
  if (e.status === 401 || e.status === 403) { handleError(e); return; }
  fs.show(e.status === 409 && LOCKOUT.test(e.message) ? [t("access.lockout")] : errList(e), head);
}

// checkGroup is a row of checkboxes over [value, label] items.
function checkGroup(items, selected) {
  const boxes = items.map(([v, label]) => {
    const box = h("input", { type: "checkbox", value: v, checked: selected.includes(v) });
    return { v, box, el: h("label", { class: "check" }, box, label) };
  });
  return { el: h("div", { class: "checks" }, boxes.map((b) => b.el)), values: () => boxes.filter((b) => b.box.checked).map((b) => b.v) };
}
// groupField is a labelled non-input control (a check group).
function groupField(label, control, help) {
  const id = uid("g");
  return h("div", { class: "field full", role: "group", "aria-labelledby": id },
    h("span", { class: "flabel", id }, label), control, help ? h("small", null, help) : null);
}
const roleItems = (roles) => roles.map((r) => [r.id, r.id]);

// ---- users -----------------------------------------------------------------------

// mergeUsers joins the stored rows and the directory rows of /-/api/users:
// a directory user with an override row (stored, source "ldap") keeps that
// record and the directory's roles as the effective ones. A local user with
// the same id is another principal, so the directory user keeps its own row,
// keyed (in its link) by its owner key "ldap:<id>".
function mergeUsers(list) {
  const stored = new Map(), dir = new Map();
  for (const u of list) (u.directory ? dir : stored).set(u.id, u);
  const over = (id) => stored.has(id) && isDir(stored.get(id).source);
  const out = [...stored.values()].map((u) => Object.assign({}, u, { key: u.id, eff: over(u.id) && dir.has(u.id) ? dir.get(u.id).roles : u.roles }));
  for (const [id, u] of dir) if (!over(id)) out.push(Object.assign({}, u, { key: stored.has(id) ? (u.source || "ldap") + ":" + id : id, eff: u.roles }));
  return out;
}
const realmBadge = (u) => (isDir(u.source) ? badge(realmName(u.source), "purple") : badge(realmName("local")));

export function viewUsers(view, id) {
  if (id === "create") return userCreate(view);
  return id === undefined ? userList(view) : userDetail(view, id);
}

async function userList(view) {
  const isLive = alive();
  const users = mergeUsers((await api("users")).users || []);
  if (!isLive()) return;
  const rows = users.map((u) => row([
    [h("a", { href: userLink(u.key) }, h("strong", null, u.id)), "name"],
    [u.email || dash],
    [realmBadge(u)],
    [roleBadges(u.eff)],
    [userState(u)],
    [h("a", { class: "btn small", href: userLink(u.key) }, t("common.details")), "act"],
  ], [u.id, u.name, u.email].join(" ")));
  const f = filterBar(rows, t("users.filter"), "users.count");
  put(view,
    pageHead(t("nav.users"), sub(t("users.sub")), acts(h("a", { class: "btn primary", href: userLink("create") }, t("users.create"))), eyebrow()),
    h("section", { class: "card" }, f.bar,
      tbl([t("users.user"), t("users.email"), t("users.realm"), t("users.roles"), t("col.status"), actHead()], rows, t("users.none")), f.none));
}

async function userCreate(view) {
  const isLive = alive();
  const roles = (await api("roles")).roles || [];
  if (!isLive()) return;
  const idIn = textIn("", { required: true, maxlength: 64 });
  const nameIn = textIn("", { maxlength: 200 });
  const emailIn = textIn("", { maxlength: 200 });
  const source = sel([["local", realmName("local")], ["ldap", realmName("ldap")], ["oidc", realmName("oidc")], ["crowd", realmName("crowd")], ["rutauth", realmName("rutauth")]], "local");
  const pw = pwIn(), pw2 = pwIn();
  const status = sel([["active", t("users.active")], ["disabled", t("users.disabled")]], "active");
  const rg = checkGroup(roleItems(roles), []);
  const pwF = field(t("users.pw"), pw, t("users.pwHelp"), { req: true });
  const pw2F = field(t("users.pwConfirm"), pw2, null, { req: true });
  const ldapNote = notice(t("users.ldapNote"), "");
  ldapNote.hidden = true;
  const syncSource = () => {
    const ldap = isDir(source.value);
    pwF.hidden = pw2F.hidden = ldap;
    ldapNote.hidden = !ldap;
  };
  source.addEventListener("change", syncSource);
  const save = h("button", { type: "submit", class: "btn primary" }, t("users.createBtn"));
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  put(form, fs.err, fields(
    field(t("users.id"), idIn, t("users.idHelp"), { req: true }),
    field(t("users.name"), nameIn),
    field(t("users.email"), emailIn),
    field(t("users.source"), source),
    pwF, pw2F,
    field(t("col.status"), status),
    groupField(t("users.roles"), rg.el)), ldapNote, h("div", { class: "savebar" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const ldap = isDir(source.value);
    const a = pw.value, b = pw2.value;
    pw.value = pw2.value = ""; // the secret never stays in the page
    const id = idIn.value.trim();
    const errs = [];
    if (!id) errs.push(t("users.e.id"));
    if (!ldap) {
      if (runes(a) < 8) errs.push(t("access.e.pwShort"));
      else if (a !== b) errs.push(t("access.e.pwMatch"));
    }
    if (errs.length) { fs.show(errs, t("users.errOne")); return; }
    save.disabled = true;
    try {
      await api("users", { method: "POST", body: { id, name: nameIn.value.trim(), email: emailIn.value.trim(),
        password: ldap ? "" : a, source: source.value, status: status.value, roles: rg.values() } });
      if (!isLive()) return;
      fs.done(userLink(id), t("users.created"));
    } catch (e) {
      save.disabled = false;
      if (isLive()) failWith(fs, e, t("users.errOne"));
    }
  });
  put(view, pageHead(t("users.createTitle"), sub(t("users.createSub")), acts(h("a", { class: "btn", href: userLink() }, t("users.back"))), eyebrow()),
    fs.ok, h("div", { class: "two" }, card(t("users.info"), t("users.infoSub"), form),
      card(t("users.access"), t("users.accessSub"), notice(t("users.tokenNote")))));
  idIn.focus();
}

// passwordModal asks for a new password twice and sends it to onSave.
function passwordModal(title, onSave) {
  const pw = pwIn(), pw2 = pwIn();
  const err = errBox();
  const form = h("form", { novalidate: true }, err, fields(field(t("users.newPw"), pw, t("users.pwHelp"), { full: true }), field(t("users.pwConfirm"), pw2, null, { full: true })));
  const save = button(t("users.resetBtn"), "primary");
  const cancel = button(t("common.cancel"));
  const m = modal(title, form, [cancel, save]);
  cancel.addEventListener("click", m.close);
  const submit = async () => {
    const a = pw.value, b = pw2.value;
    pw.value = pw2.value = "";
    err.hidden = true;
    const msg = runes(a) < 8 ? t("access.e.pwShort") : a !== b ? t("access.e.pwMatch") : "";
    if (msg) { put(err, msg); err.hidden = false; return; }
    save.disabled = true;
    try {
      await onSave(a);
      m.close();
    } catch (e) {
      save.disabled = false;
      put(err, errList(e).join(" "));
      err.hidden = false;
    }
  };
  form.addEventListener("submit", (ev) => { ev.preventDefault(); submit(); });
  save.addEventListener("click", submit);
  pw.focus();
}

async function userDetail(view, id) {
  const isLive = alive();
  const [ul, rl] = await Promise.all([api("users"), api("roles")]);
  if (!isLive()) return;
  const u = mergeUsers(ul.users || []).find((x) => x.key === id);
  const back = h("a", { class: "btn", href: userLink() }, t("users.back"));
  if (!u) {
    put(view, pageHead(t("nav.users"), null, acts(back), eyebrow()), notFound(t("users.notFound", { id })));
    return;
  }
  const stored = !u.directory, ldap = isDir(u.source);
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const effective = (u.eff || []);
  const access = card(t("users.access"), t("users.accessSub"), [
    effective.length ? effective.map((r) => h("div", { class: "listrow" }, h("a", { href: roleLink(r) }, h("strong", null, r)), r === "admin" ? badge(t("users.isAdmin"), "amber") : null))
      : h("div", { class: "empty" }, t("users.noRoles")),
    notice(t("users.tokenNote"))]);
  // revoke ends every API token of the user (local and LDAP); a password
  // reset leaves them valid.
  const revoke = button(t("users.revoke"), "danger");
  revoke.addEventListener("click", async () => {
    if (!(await confirmDelete(t("users.revokeText", { id: u.id }), t("users.revokeTitle"), t("users.revoke")))) return;
    fs.clear();
    revoke.disabled = true;
    try {
      const out = await api("users/" + enc(u.id) + "/tokens", { method: "DELETE" });
      if (!isLive()) return;
      put(fs.ok, t("users.revoked", { n: out.revoked }));
      fs.ok.hidden = false;
    } catch (e) {
      if (isLive()) failWith(fs, e, t("users.revokeErr"));
    } finally {
      revoke.disabled = false;
    }
  });
  let head;
  if (!stored) {
    put(form, kv(t("users.id"), u.id), kv(t("users.name"), u.name), kv(t("users.email"), u.email), kv(t("users.realm"), realmBadge(u)),
      kv(t("users.roles"), h("span", null, roleBadges(u.eff))), notice(t("users.dirNote")));
    head = acts(back, revoke);
  } else {
    const nameIn = textIn(u.name, { disabled: ldap, maxlength: 200 });
    const emailIn = textIn(u.email, { disabled: ldap, maxlength: 200 });
    const status = sel([["active", t("users.active")], ["disabled", t("users.disabled")]], u.status || "active");
    const rg = checkGroup(roleItems(rl.roles || []), u.roles || []);
    const save = h("button", { type: "submit", class: "btn primary" }, t("users.save"));
    const idIn = textIn(u.id, { disabled: true });
    const srcIn = textIn(realmName(ldap ? u.source : "local"), { disabled: true });
    put(form, fs.err, fields(
      field(t("users.id"), idIn), field(t("users.source"), srcIn),
      field(t("users.name"), nameIn), field(t("users.email"), emailIn),
      field(t("col.status"), status), groupField(t("users.roles"), rg.el, ldap ? t("users.overrideHelp") : null)),
    ldap ? notice(t("users.ldapNote")) : null, h("div", { class: "savebar" }, save));
    const send = (patch) => api("users/" + enc(id), { method: "PUT", body: Object.assign({ name: u.name, email: u.email, roles: u.roles || [], status: u.status || "active" }, patch) });
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      fs.clear();
      save.disabled = true;
      try {
        await send({ name: nameIn.value.trim(), email: emailIn.value.trim(), roles: rg.values(), status: status.value });
        if (isLive()) fs.done(userLink(id), t("users.saved"));
      } catch (e) {
        save.disabled = false;
        if (isLive()) failWith(fs, e, t("users.errOne"));
      }
    });
    const disabled = u.status === "disabled";
    const toggle = button(disabled ? t("users.enable") : t("users.disable"));
    toggle.addEventListener("click", async () => {
      fs.clear();
      toggle.disabled = true;
      try {
        await send({ status: disabled ? "active" : "disabled" });
        if (isLive()) fs.done(userLink(id), disabled ? t("users.enabledMsg") : t("users.disabledMsg"));
      } catch (e) {
        toggle.disabled = false;
        if (isLive()) failWith(fs, e, t("users.errOne"));
      }
    });
    const reset = ldap ? null : button(t("users.reset"));
    if (reset) {
      reset.addEventListener("click", () => passwordModal(t("users.resetTitle", { id }), async (pw) => {
        await api("users/" + enc(id) + "/password", { method: "PUT", body: { password: pw } });
        if (!isLive()) return;
        put(fs.ok, t("users.resetDone"));
        fs.ok.hidden = false;
        fs.err.hidden = true;
      }));
    }
    const del = button(t("common.delete"), "danger");
    del.addEventListener("click", async () => {
      if (!(await confirmDelete(t("users.delText", { id }), t("users.delTitle")))) return;
      fs.clear();
      del.disabled = true;
      try {
        await api("users/" + enc(id), { method: "DELETE" });
      } catch (e) {
        del.disabled = false;
        if (isLive()) failWith(fs, e, t("users.errOne"));
        return;
      }
      if (isLive()) location.hash = userLink();
    });
    head = acts(back, reset, revoke, toggle, del);
  }
  put(view, pageHead(u.id, sub(t("users.realmSub", { realm: realmName(ldap ? u.source : "local") })), head, eyebrow()),
    fs.ok, h("div", { class: "two" }, card(t("users.info"), t("users.infoSub"), form), access));
}

// ---- roles -----------------------------------------------------------------------

const targetKind = (tg) => (tg === "*" ? "all" : tg.startsWith("format:") ? "format" : tg.startsWith("selector:") ? "selector" : "repo");
const targetValue = (tg) => (tg === "*" ? "" : tg.slice(tg.indexOf(":") + 1));
const scopeText = (tg) => {
  const k = targetKind(tg), v = targetValue(tg);
  return k === "all" ? t("roles.scopeAll") : k === "format" ? t("roles.scopeFormat", { v: FORMAT_NAMES[v] || v }) : k === "selector" ? t("roles.scopeSelector", { v }) : t("roles.scopeRepo", { v });
};

export async function viewRoles(view, id) {
  const isLive = alive();
  // The selectors list only fills the selector target select; without it a stored name is still offered.
  const [rd, repos, sd] = await Promise.all([api("roles"), loadRepos(true), api("content-selectors").catch(() => null)]);
  if (!isLive()) return;
  const roles = rd.roles || [];
  const selNames = ((sd && sd.contentSelectors) || []).map((x) => x.name);
  const creating = id === "create";
  let cur = creating ? { id: "", description: "", privileges: [] } : roles.find((r) => r.id === id) || (id === undefined ? roles[0] : null);
  const add = h("a", { class: "btn primary", href: roleLink("create") }, t("roles.create"));
  const eb = eyebrow();
  if (id === undefined) {
    const panel = card(t("nav.roles"), null, null);
    panel.appendChild(tbl([t("roles.id"), t("roles.desc"), t("roles.privs")], roles.map((r) => row([
      [h("a", { href: roleLink(r.id) }, h("strong", null, r.id))],
      [r.description || dash], [(r.privileges || []).length, "num"],
    ])), t("roles.none")));
    put(view, pageHead(t("nav.roles"), sub(t("roles.sub")), acts(add), eb), panel);
    return;
  }
  if (!cur && !creating) {
    put(view, pageHead(t("nav.roles"), sub(t("roles.sub")), acts(add), eb), notFound(id === undefined ? t("roles.none") : t("roles.notFound", { id })));
    return;
  }
  const aside = h("nav", { class: "role-list card", "aria-label": t("nav.roles") }, roles.map((r) => {
    const on = !creating && r.id === cur.id;
    return h("a", { class: "role-item" + (on ? " active" : ""), href: roleLink(r.id), "aria-current": on ? "page" : null },
      h("strong", null, r.id, r.id === "admin" ? badge(t("roles.builtin"), "amber") : null), h("small", null, r.description || dash));
  }));
  const main = h("form", { class: "stack", novalidate: true });
  const fs = formState(main);
  const repoNames = repos.map((r) => r.name);
  let editing = creating;

  // privRow builds one editable privilege: scope, target and actions.
  const rowsBox = h("div");
  const privs = [];
  const addPriv = (p) => {
    const kind = sel([["all", t("roles.scopeAll")], ["format", t("roles.kFormat")], ["repo", t("roles.kRepo")], ["selector", t("roles.kSelector")]], targetKind(p.target));
    const val = h("select", { "aria-label": t("roles.target") });
    // withKept lists names plus a stored one the list no longer has (the "kept" rule).
    const withKept = (names, keep) => (names.includes(keep) || !keep ? names : names.concat(keep)).map((n) => [n, n]);
    // A selector privilege also names the repositories it applies to ("*" = any).
    const stored = p.repos || [];
    const reposSel = h("select", { multiple: true, size: 4, "aria-label": t("roles.repos") },
      [["*", t("roles.anyRepo")]].concat(withKept(repoNames.filter((n) => n !== "*"), stored.find((n) => n !== "*" && !repoNames.includes(n))))
        .map(([v, l]) => h("option", { value: v, selected: stored.includes(v) }, l)));
    const reposF = h("div", { class: "field" }, h("span", { class: "flabel" }, t("roles.repos")), reposSel);
    const fill = () => {
      const k = kind.value, keep = val.value || targetValue(p.target);
      const opts = k === "format" ? FORMATS.map((f) => [f, FORMAT_NAMES[f]]) : k === "repo" ? withKept(repoNames, keep) : k === "selector" ? withKept(selNames, keep) : [];
      put(val, opts.map(([v, l]) => h("option", { value: v }, l)));
      if (opts.some(([v]) => v === keep)) val.value = keep;
      val.hidden = k === "all";
      reposF.hidden = k !== "selector";
    };
    kind.id = uid("k");
    fill();
    kind.addEventListener("change", () => { val.value = ""; fill(); });
    const ag = checkGroup(ACTIONS.map((a) => [a, a]), p.actions || []);
    const rm = button(t("roles.removePriv"), "small danger");
    const el = h("div", { class: "edit-row" }, h("div", { class: "row" },
      h("div", { class: "field" }, h("label", { for: kind.id }, t("roles.scope")), kind), h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("roles.target")), val), rm), reposF, ag.el);
    const item = { el, target: () => (kind.value === "all" ? "*" : kind.value + ":" + val.value), actions: ag.values, kind,
      repos: () => (kind.value === "selector" ? [...reposSel.selectedOptions].map((o) => o.value) : undefined) };
    rm.addEventListener("click", () => { privs.splice(privs.indexOf(item), 1); el.remove(); fs.dirty(); });
    privs.push(item);
    rowsBox.appendChild(el);
  };

  const render = () => {
    if (!editing) {
      const admin = cur.id === "admin";
      const edit = admin ? null : button(t("roles.edit"));
      const del = admin ? null : button(t("common.delete"), "danger");
      if (edit) edit.addEventListener("click", () => { editing = true; render(); });
      if (del) {
        del.addEventListener("click", async () => {
          if (!(await confirmDelete(t("roles.delText", { id: cur.id }), t("roles.delTitle")))) return;
          fs.clear();
          del.disabled = true;
          try {
            await api("roles/" + enc(cur.id), { method: "DELETE" });
          } catch (e) {
            del.disabled = false;
            if (!isLive()) return;
            if (e.status === 409) fs.show([IN_USE.test(e.message) ? t("roles.inUse", { id: cur.id }) : e.message], t("roles.errOne"));
            else failWith(fs, e, t("roles.errOne"));
            return;
          }
          if (isLive()) location.hash = roleLink();
        });
      }
      const pc = card(t("roles.privs"), t("roles.privsSub"), null);
      pc.append(tbl([t("roles.target"), t("roles.scope"), t("roles.actions")],
        (cur.privileges || []).map((p) => h("tr", null, h("td", null, h("code", null, p.target), p.repos ? h("div", { class: "tiny muted" }, p.repos.join(", ")) : null), h("td", null, scopeText(p.target)),
          h("td", null, (p.actions || []).join(" · ")))), t("roles.noPrivs")));
      put(main, fs.err, fs.ok,
        card(cur.id, admin ? t("roles.adminSub") : t("roles.infoSub"), [
          kv(t("roles.id"), cur.id), kv(t("roles.desc"), cur.description || dash)], edit || del ? acts(edit, del) : null),
        pc,
        admin ? notice(t("roles.adminNote")) : null);
      return;
    }
    // edit or create form
    privs.length = 0;
    put(rowsBox);
    for (const p of cur.privileges || []) addPriv(p);
    const idIn = textIn(cur.id, { disabled: !creating, required: true, maxlength: 64 });
    const descIn = textIn(cur.description, { maxlength: 200 });
    const addBtn = button(t("roles.addPriv"));
    addBtn.addEventListener("click", () => { addPriv({ target: "*", actions: [] }); fs.dirty(); });
    const save = h("button", { type: "submit", class: "btn primary" }, creating ? t("roles.createBtn") : t("roles.save"));
    const cancel = button(t("common.cancel"));
    cancel.addEventListener("click", () => { fs.clean(); if (creating) location.hash = roleLink(); else { editing = false; render(); } });
    put(main, fs.err, fs.ok,
      card(creating ? t("roles.createTitle") : cur.id, t("roles.infoSub"), fields(
        field(t("roles.id"), idIn, creating ? t("roles.idHelp") : null, { req: creating }), field(t("roles.desc"), descIn))),
      card(t("roles.privs"), t("roles.privsEditSub"), [rowsBox, h("div", { class: "actions" }, addBtn), notice(t("roles.overwriteNote"))]),
      h("div", { class: "savebar" }, cancel, save));
    main.onsubmit = async (ev) => {
      ev.preventDefault();
      fs.clear();
      const rid = creating ? idIn.value.trim() : cur.id;
      const body = { description: descIn.value.trim(), privileges: privs.map((p) => {
        const o = { target: p.target(), actions: p.actions() };
        if (p.repos()) o.repos = p.repos();
        return o;
      }) };
      const errs = [];
      if (!rid) errs.push(t("roles.e.id"));
      const seen = new Set();
      body.privileges.forEach((p, i) => {
        if (/^(format|repo|selector):$/.test(p.target)) errs.push(t("roles.e.target", { n: i + 1 }));
        if (!p.actions.length) errs.push(t("roles.e.actions", { n: i + 1 }));
        if (p.repos && !p.repos.length) errs.push(t("roles.e.repos", { n: i + 1 }));
        const key = p.target + (p.repos ? "|" + p.repos.join(",") : ""); // the same selector may apply to other repositories
        if (seen.has(key)) errs.push(t("roles.e.dup", { n: i + 1 }));
        seen.add(key);
      });
      if (errs.length) { fs.show(errs, t("roles.errOne")); return; }
      save.disabled = true;
      try {
        if (creating) await api("roles", { method: "POST", body: Object.assign({ id: rid }, body) });
        else await api("roles/" + enc(rid), { method: "PUT", body });
        if (isLive()) fs.done(roleLink(rid), creating ? t("roles.created") : t("roles.saved"));
      } catch (e) {
        save.disabled = false;
        if (isLive()) failWith(fs, e, t("roles.errOne"));
      }
    };
  };
  render();
  put(view, pageHead(t("nav.roles"), sub(t("roles.sub")), acts(add), eb),
    h("div", { class: "role-grid" }, aside, main));
}

// ---- realms and LDAP -------------------------------------------------------------

const STEPS = { name: "ldap.step.name", connect: "ldap.step.connect", starttls: "ldap.step.starttls", bind: "ldap.step.bind", search: "ldap.step.search", userBind: "ldap.step.userBind", groups: "ldap.step.groups" };

export async function viewRealms(view) {
  const isLive = alive();
  const rd = await api("realms");
  if (!isLive()) return;
  const avail = rd.available && rd.available.length ? rd.available : ["local", "token", "ldap"];
  let active = (rd.realms || []).filter((r) => r.enabled).map((r) => r.id);
  const realmBox = h("div", { class: "stack" });
  const fsR = formState(realmBox); // the realm lists and the LDAP form each keep their own edits
  const activeBox = h("div"), availBox = h("div"), noLocal = notice(t("realms.noLocal"), "amber");

  const draw = () => {
    put(activeBox, active.length ? active.map((r, i) => {
      const move = (d) => () => { active.splice(i, 1); active.splice(i + d, 0, r); fsR.dirty(); draw(); };
      const up = button("↑", "small"), down = button("↓", "small"), off = button(t("realms.disable"), "small");
      up.disabled = i === 0;
      down.disabled = i === active.length - 1;
      up.setAttribute("aria-label", t("realms.up", { name: realmName(r) }));
      down.setAttribute("aria-label", t("realms.down", { name: realmName(r) }));
      up.addEventListener("click", move(-1));
      down.addEventListener("click", move(1));
      off.addEventListener("click", () => { active.splice(i, 1); fsR.dirty(); draw(); });
      return h("div", { class: "listrow" }, h("span", null, badge(String(i + 1), "blue"), " ", h("strong", null, realmName(r)), h("small", null, realmDesc(r))),
        h("span", { class: "actions" }, up, down, off));
    }) : h("div", { class: "empty" }, t("realms.noneActive")));
    const rest = avail.filter((r) => !active.includes(r));
    put(availBox, rest.length ? rest.map((r) => {
      const on = button(t("realms.enable"), "small");
      on.addEventListener("click", () => { active.push(r); fsR.dirty(); draw(); });
      return h("div", { class: "listrow" }, h("span", null, h("strong", null, realmName(r)), h("small", null, realmDesc(r))), on);
    }) : h("div", { class: "empty" }, t("realms.noneAvail")));
    noLocal.hidden = active.includes("local");
  };
  draw();

  const saveR = h("button", { type: "button", class: "btn primary" }, t("realms.save"));
  saveR.addEventListener("click", async () => {
    fsR.clear();
    fsR.ok.hidden = true;
    saveR.disabled = true;
    try {
      await api("realms", { method: "PUT", body: { realms: active.map((id) => ({ id, enabled: true })).concat(avail.filter((r) => !active.includes(r)).map((id) => ({ id, enabled: false }))) } });
      if (!isLive()) return;
      fsR.clean();
      put(fsR.ok, t("realms.saved"));
      fsR.ok.hidden = false;
      saveR.disabled = false;
    } catch (e) {
      saveR.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      failWith(fsR, e, t("realms.errOne"));
    }
  });

  put(realmBox, fsR.err, fsR.ok,
    h("div", { class: "equal" }, card(t("realms.active"), t("realms.activeSub"), activeBox), card(t("realms.available"), t("realms.availableSub"), availBox)),
    noLocal, notice(t("realms.lockNote"), "amber"), h("div", { class: "savebar" }, saveR));
  // Each type owns its API, fields and dirty state. OIDC can register its
  // editor when the server's field contract is available.
  const editors = {
    ldap: async () => {
      const [ld, rl] = await Promise.all([api("realms/ldap"), api("roles")]);
      if (!isLive()) return [];
      return ldapSettings(ld, rl, isLive);
    },
    oidc: async () => {
      const [od, rl] = await Promise.all([api("realms/oidc"), api("roles")]);
      if (!isLive()) return [];
      return oidcSettings(od, rl, isLive);
    },
    crowd: async () => {
      const [cd, rl] = await Promise.all([api("realms/crowd"), api("roles")]);
      if (!isLive()) return [];
      return crowdSettings(cd, rl, isLive);
    },
    rutauth: async () => {
      const [rd, rl] = await Promise.all([api("realms/rutauth"), api("roles")]);
      if (!isLive()) return [];
      return rutSettings(rd, rl, isLive);
    },
    saml: async () => {
      const [sd, rl] = await Promise.all([api("realms/saml"), api("roles")]);
      if (!isLive()) return [];
      return samlSettings(sd, rl, isLive);
    },
  };
  const types = avail.filter((id) => editors[id]);
  const panels = new Map();
  const config = h("div", { class: "stack" });
  const type = sel(types.map((id) => [id, realmName(id)]), types[0]);
  const showType = async () => {
    const id = type.value;
    if (!editors[id]) return;
    if (!panels.has(id)) {
      const panel = h("div", { class: "stack", "data-realm-type": id }, loading());
      panels.set(id, panel);
      config.appendChild(panel);
      editors[id]().then((nodes) => { if (isLive()) put(panel, nodes); }, (e) => {
        panels.delete(id);
        panel.remove();
        if (isLive()) handleError(e);
      });
    }
    for (const [key, panel] of panels) panel.hidden = key !== id;
  };
  type.addEventListener("change", showType);
  put(view, pageHead(t("nav.realms"), sub(t("realms.sub")), null, eyebrow()), realmBox,
    types.length ? card(t("realms.settings"), null, fields(field(t("col.type"), type))) : null, config);
  await showType();
}

function ldapSettings(ld, rl, isLive) {
  // ---- LDAP form
  const roleOpts = roleItems(rl.roles || []);
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const urlIn = textIn(ld.url, { placeholder: "ldaps://ldap.example.com:636", maxlength: 300 });
  const stls = h("input", { type: "checkbox", checked: !!ld.startTLS });
  const insecure = h("input", { type: "checkbox", checked: !!ld.insecureSkipVerify });
  const insecureWarn = notice(t("ldap.insecureWarn"), "amber");
  insecureWarn.hidden = !ld.insecureSkipVerify;
  insecure.addEventListener("change", () => { insecureWarn.hidden = !insecure.checked; });
  const bindDN = textIn(ld.bindDN, { maxlength: 300 });
  const bindPw = h("input", { type: "password", autocomplete: "new-password", placeholder: ld.bindPasswordSet ? t("rf.unchanged") : "" });
  const clearPw = ld.bindPasswordSet ? h("input", { type: "checkbox" }) : null;
  if (clearPw) clearPw.addEventListener("change", () => { bindPw.disabled = clearPw.checked; if (clearPw.checked) bindPw.value = ""; });
  const timeout = textIn(ld.timeoutSeconds || "", { type: "number", min: 1, max: 60 });
  const baseDN = textIn(ld.userBaseDN, { maxlength: 300 });
  const filter = textIn(ld.userFilter, { placeholder: "(uid={user})", class: "mono", maxlength: 300 });
  const idAttr = textIn(ld.userIdAttr, { placeholder: "uid" });
  const nameAttr = textIn(ld.nameAttr, { placeholder: "cn" });
  const emailAttr = textIn(ld.emailAttr, { placeholder: "mail" });
  const groupAttr = textIn(ld.groupAttr, { placeholder: "memberOf" });
  const defaults = checkGroup(roleOpts, ld.defaultRoles || []);
  const adminCnWarn = notice(t("ldap.adminCnWarn"), "amber");
  const adminDefWarn = notice(t("ldap.adminDefaultWarn"), "amber");
  const maps = [], mapBox = h("div");
  const syncWarn = () => {
    adminCnWarn.hidden = !maps.some((m) => { const g = m.group.value.trim(); return m.roles.values().includes("admin") && !(g.includes("=") && g.includes(",")); }); // a full DN has "=" and ","
    adminDefWarn.hidden = !defaults.values().includes("admin");
  };
  const addMap = (m) => {
    const group = textIn(m.group, { class: "mono", placeholder: "cn=admins,ou=groups,dc=example,dc=com", "aria-label": t("ldap.mapGroup") });
    const roles = checkGroup(roleOpts, m.roles || []);
    const rm = button(t("ldap.mapRemove"), "small danger");
    const el = h("div", { class: "edit-row" }, h("div", { class: "row" }, h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("ldap.mapGroup")), group), rm), roles.el);
    const item = { group, roles };
    rm.addEventListener("click", () => { maps.splice(maps.indexOf(item), 1); el.remove(); syncWarn(); fs.dirty(); saveHint.hidden = false; });
    maps.push(item);
    mapBox.appendChild(el);
  };
  (ld.groupMappings || []).forEach(addMap);
  const addMapBtn = button(t("ldap.mapAdd"));
  addMapBtn.addEventListener("click", () => { addMap({ group: "", roles: [] }); syncWarn(); fs.dirty(); });

  const saveHint = notice(t("ldap.saveFirst"), "amber");
  saveHint.hidden = true;
  form.addEventListener("input", () => { saveHint.hidden = false; syncWarn(); });
  form.addEventListener("change", () => { saveHint.hidden = false; syncWarn(); });
  const save = h("button", { type: "submit", class: "btn primary" }, t("ldap.save"));
  const bindErr = errBox();
  const setHelp = ld.bindPasswordSet ? [h("span", { class: "rf-set" }, t("rf.passSet")), " — ", t("rf.keepEmpty")] : null;
  const pwField = field(t("ldap.bindPw"), bindPw, setHelp);
  pwField.append(bindErr);
  put(form, fs.err, fs.ok,
    notice(t("ldap.recheck"), "amber"), notice(t("ldap.disabledWarn"), "amber"), notice(t("ldap.idAttrWarn"), "amber"),
    fields(
      field(t("ldap.url"), urlIn, t("ldap.urlHelp"), { full: true }),
      h("div", { class: "field" }, h("label", { class: "check" }, stls, t("ldap.startTLS"))),
      h("div", { class: "field" }, h("label", { class: "check" }, insecure, t("ldap.insecure"))),
      h("div", { class: "field full" }, h("small", null, t("ldap.trustHelp"))),
      field(t("ldap.bindDN"), bindDN),
      pwField,
      clearPw ? h("div", { class: "field" }, h("label", { class: "check" }, clearPw, t("ldap.clearPw"))) : null,
      field(t("ldap.timeout"), timeout, t("ldap.timeoutHelp")),
      field(t("ldap.baseDN"), baseDN),
      field(t("ldap.filter"), filter, t("ldap.filterHelp")),
      field(t("ldap.idAttr"), idAttr), field(t("ldap.nameAttr"), nameAttr),
      field(t("ldap.emailAttr"), emailAttr), field(t("ldap.groupAttr"), groupAttr),
      groupField(t("ldap.defaults"), defaults.el, t("ldap.defaultsHelp"))),
    insecureWarn,
    h("h3", { class: "card-title" }, t("ldap.maps")), h("p", { class: "hint" }, t("ldap.mapsHelp")), mapBox,
    h("div", { class: "actions" }, addMapBtn), adminCnWarn, adminDefWarn, saveHint, h("div", { class: "savebar" }, save));
  syncWarn();
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    bindErr.hidden = true;
    fs.ok.hidden = true;
    const bp = bindPw.value;
    bindPw.value = ""; // the secret never stays in the page
    const tm = timeout.value.trim() === "" ? 0 : Number(timeout.value);
    const errs = [];
    if (!Number.isInteger(tm) || tm < 0) errs.push(t("ldap.e.timeout"));
    maps.forEach((m, i) => { if (!m.group.value.trim()) errs.push(t("ldap.e.group", { n: i + 1 })); });
    if (errs.length) { fs.show(errs, t("ldap.errOne")); return; }
    save.disabled = true;
    try {
      const out = await api("realms/ldap", { method: "PUT", body: {
        url: urlIn.value.trim(), startTLS: stls.checked, insecureSkipVerify: insecure.checked, bindDN: bindDN.value.trim(),
        bindPassword: bp, clearBindPassword: !!(clearPw && clearPw.checked), timeoutSeconds: tm, userBaseDN: baseDN.value.trim(),
        userFilter: filter.value.trim(), userIdAttr: idAttr.value.trim(), nameAttr: nameAttr.value.trim(), emailAttr: emailAttr.value.trim(),
        groupAttr: groupAttr.value.trim(), defaultRoles: defaults.values(),
        groupMappings: maps.map((m) => ({ group: m.group.value.trim(), roles: m.roles.values() })),
      } });
      if (!isLive()) return;
      // update this card in place: the realm lists keep their own unsaved edits
      fs.clean();
      saveHint.hidden = true;
      bindPw.disabled = false;
      bindPw.placeholder = out && out.bindPasswordSet ? t("rf.unchanged") : "";
      if (clearPw) clearPw.checked = false;
      put(fs.ok, t("ldap.saved"));
      fs.ok.hidden = false;
      save.disabled = false;
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 400 && REENTER.test(e.message)) { put(bindErr, t("ldap.reenter")); bindErr.hidden = false; bindPw.focus(); } else failWith(fs, e, t("ldap.errOne"));
    }
  });

  // ---- test connection (the saved configuration)
  const tUser = textIn("", { autocomplete: "off" });
  const tPw = h("input", { type: "password", autocomplete: "new-password" });
  const run = button(t("ldap.test"), "primary");
  const result = h("div", { "aria-live": "polite" });
  run.addEventListener("click", async () => {
    const body = { user: tUser.value.trim(), password: tPw.value };
    tPw.value = ""; // never kept
    run.disabled = true;
    put(result, loading());
    try {
      const d = await api("realms/ldap/test", { method: "POST", body });
      if (!isLive()) return;
      put(result, badge(d.ok ? t("ldap.testOk") : t("ldap.testFail"), d.ok ? "green" : "red"),
        (d.steps || []).map((x) => h("div", { class: "listrow" },
          h("span", null, h("strong", null, STEPS[x.step] ? t(STEPS[x.step]) : x.step), x.detail ? h("small", null, x.detail) : null),
          badge(x.ok ? t("ldap.stepOk") : t("ldap.stepFail"), x.ok ? "green" : "red"))));
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); put(result); } else put(result, notice(e.message, "amber"));
    } finally {
      run.disabled = false;
    }
  });
  const testCard = card(t("ldap.testTitle"), t("ldap.testSub"), [
    fields(field(t("ldap.testUser"), tUser, t("ldap.testUserHelp")), field(t("ldap.testPw"), tPw)),
    h("div", { class: "actions end" }, run), result]);
  return [card(t("ldap.title"), t("ldap.sub"), form), testCard];
}

// oidcSettings is the OIDC realm form (issuer, client, claims, roles) and a
// discovery test that uses the form's values without saving them.
function oidcSettings(od, rl, isLive) {
  const roleOpts = roleItems(rl.roles || []);
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const issuer = textIn(od.issuer, { placeholder: "https://idp.example.com/realms/main", maxlength: 300, class: "mono" });
  const clientId = textIn(od.clientId, { maxlength: 300, class: "mono", autocomplete: "off" });
  const secret = h("input", { type: "password", autocomplete: "new-password", placeholder: od.clientSecretSet ? t("rf.unchanged") : "" });
  const clearSecret = od.clientSecretSet ? h("input", { type: "checkbox" }) : null;
  if (clearSecret) clearSecret.addEventListener("change", () => { secret.disabled = clearSecret.checked; if (clearSecret.checked) secret.value = ""; });
  const displayName = textIn(od.displayName, { maxlength: 60, placeholder: "SSO" });
  const scopes = textIn((od.scopes || []).join(" "), { class: "mono", placeholder: "openid profile email groups", maxlength: 300 });
  const userClaim = textIn(od.usernameClaim, { class: "mono", placeholder: "preferred_username" });
  const groupsClaim = textIn(od.groupsClaim, { class: "mono", placeholder: "groups" });
  const caCert = h("textarea", { class: "mono", rows: 5, placeholder: "-----BEGIN CERTIFICATE-----", spellcheck: "false" }, od.caCert || "");
  const insecure = h("input", { type: "checkbox", checked: !!od.insecureSkipVerify });
  const insecureWarn = notice(t("ldap.insecureWarn"), "amber");
  insecureWarn.hidden = !od.insecureSkipVerify;
  insecure.addEventListener("change", () => { insecureWarn.hidden = !insecure.checked; });
  const timeout = textIn(od.timeoutSeconds || "", { type: "number", min: 1, max: 60 });
  const defaults = checkGroup(roleOpts, od.defaultRoles || []);
  const adminDefWarn = notice(t("ldap.adminDefaultWarn"), "amber");
  const maps = [], mapBox = h("div");
  const syncWarn = () => { adminDefWarn.hidden = !defaults.values().includes("admin"); };
  const addMap = (m) => {
    const group = textIn(m.group, { class: "mono", placeholder: "platform:admins", "aria-label": t("oidc.mapGroup") });
    const roles = checkGroup(roleOpts, m.roles || []);
    const rm = button(t("ldap.mapRemove"), "small danger");
    const el = h("div", { class: "edit-row" }, h("div", { class: "row" }, h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("oidc.mapGroup")), group), rm), roles.el);
    const item = { group, roles };
    rm.addEventListener("click", () => { maps.splice(maps.indexOf(item), 1); el.remove(); fs.dirty(); });
    maps.push(item);
    mapBox.appendChild(el);
  };
  (od.groupMappings || []).forEach(addMap);
  const addMapBtn = button(t("ldap.mapAdd"));
  addMapBtn.addEventListener("click", () => { addMap({ group: "", roles: [] }); fs.dirty(); });
  form.addEventListener("change", syncWarn);
  const save = h("button", { type: "submit", class: "btn primary" }, t("oidc.save"));
  const secretErr = errBox();
  const setHelp = od.clientSecretSet ? [h("span", { class: "rf-set" }, t("oidc.secretSet")), " — ", t("rf.keepEmpty")] : null;
  const secretField = field(t("oidc.clientSecret"), secret, setHelp);
  secretField.append(secretErr);
  const body = () => ({
    issuer: issuer.value.trim(), clientId: clientId.value.trim(), clientSecret: secret.value, clearClientSecret: !!(clearSecret && clearSecret.checked),
    displayName: displayName.value.trim(), scopes: scopes.value.trim().split(/\s+/).filter(Boolean),
    usernameClaim: userClaim.value.trim(), groupsClaim: groupsClaim.value.trim(), caCert: caCert.value.trim(), insecureSkipVerify: insecure.checked,
    timeoutSeconds: timeout.value.trim() === "" ? 0 : Number(timeout.value), defaultRoles: defaults.values(),
    groupMappings: maps.map((m) => ({ group: m.group.value.trim(), roles: m.roles.values() })),
  });
  put(form, fs.err, fs.ok, notice(t("oidc.redirectNote", { url: location.origin + "/-/sso/oidc/callback" }), ""),
    fields(
      field(t("oidc.issuer"), issuer, t("oidc.issuerHelp"), { full: true }),
      field(t("oidc.clientId"), clientId), secretField,
      clearSecret ? h("div", { class: "field" }, h("label", { class: "check" }, clearSecret, t("oidc.clearSecret"))) : null,
      field(t("oidc.displayName"), displayName, t("oidc.displayNameHelp")),
      field(t("oidc.scopes"), scopes, t("oidc.scopesHelp"), { full: true }),
      field(t("oidc.usernameClaim"), userClaim, t("oidc.usernameClaimHelp")),
      field(t("oidc.groupsClaim"), groupsClaim, t("oidc.groupsClaimHelp")),
      field(t("oidc.caCert"), caCert, t("oidc.caCertHelp"), { full: true }),
      h("div", { class: "field" }, h("label", { class: "check" }, insecure, t("ldap.insecure"))),
      field(t("ldap.timeout"), timeout, t("ldap.timeoutHelp")),
      groupField(t("ldap.defaults"), defaults.el, t("oidc.defaultsHelp"))),
    insecureWarn,
    h("h3", { class: "card-title" }, t("ldap.maps")), h("p", { class: "hint" }, t("oidc.mapsHelp")), mapBox,
    h("div", { class: "actions" }, addMapBtn), adminDefWarn, h("div", { class: "savebar" }, save));
  syncWarn();
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    secretErr.hidden = true;
    fs.ok.hidden = true;
    const b = body();
    secret.value = ""; // the secret never stays in the page
    const errs = [];
    if (!Number.isInteger(b.timeoutSeconds) || b.timeoutSeconds < 0) errs.push(t("ldap.e.timeout"));
    maps.forEach((m, i) => { if (!m.group.value.trim()) errs.push(t("ldap.e.group", { n: i + 1 })); });
    if (errs.length) { fs.show(errs, t("oidc.errOne")); return; }
    save.disabled = true;
    try {
      const out = await api("realms/oidc", { method: "PUT", body: b });
      if (!isLive()) return;
      fs.clean();
      secret.disabled = false;
      secret.placeholder = out && out.clientSecretSet ? t("rf.unchanged") : "";
      if (clearSecret) clearSecret.checked = false;
      put(fs.ok, t("oidc.saved"));
      fs.ok.hidden = false;
      save.disabled = false;
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 400 && REENTER_OIDC.test(e.message)) { put(secretErr, t("oidc.reenter")); secretErr.hidden = false; secret.focus(); } else failWith(fs, e, t("oidc.errOne"));
    }
  });

  // ---- discovery test (the form's values, not saved)
  const run = button(t("oidc.test"), "primary");
  const result = h("div", { "aria-live": "polite" });
  run.addEventListener("click", async () => {
    run.disabled = true;
    put(result, loading());
    try {
      const d = await api("realms/oidc/test", { method: "POST", body: body() });
      if (!isLive()) return;
      put(result, badge(t("ldap.testOk"), "green"),
        [["oidc.t.issuer", d.issuer], ["oidc.t.auth", d.authorizationEndpoint], ["oidc.t.token", d.tokenEndpoint], ["oidc.t.jwks", d.jwksUri],
          ["oidc.t.scopes", (d.scopesSupported || []).join(" ")], ["oidc.t.algs", (d.algs || []).join(" ")]].map(([k, v]) =>
          h("div", { class: "listrow" }, h("span", null, h("strong", null, t(k)), h("small", { class: "mono" }, v || dash)))));
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); put(result); } else put(result, badge(t("ldap.testFail"), "red"), notice(e.message, "amber"));
    } finally {
      run.disabled = false;
    }
  });
  const testCard = card(t("oidc.testTitle"), t("oidc.testSub"), [h("div", { class: "actions end" }, run), result]);
  return [card(t("oidc.title"), t("oidc.sub"), form), testCard];
}

// ---- Crowd, RUT and SAML realms ----------------------------------------------------

// mappingRows edits group → roles mappings in the LDAP layout.
function mappingRows(roleOpts, list, placeholder, fs) {
  const maps = [], box = h("div");
  const add = (m) => {
    const group = textIn(m.group, { class: "mono", placeholder, "aria-label": t("oidc.mapGroup") });
    const roles = checkGroup(roleOpts, m.roles || []);
    const rm = button(t("ldap.mapRemove"), "small danger");
    const el = h("div", { class: "edit-row" }, h("div", { class: "row" }, h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("oidc.mapGroup")), group), rm), roles.el);
    const item = { group, roles };
    rm.addEventListener("click", () => { maps.splice(maps.indexOf(item), 1); el.remove(); fs.dirty(); });
    maps.push(item);
    box.appendChild(el);
  };
  (list || []).forEach(add);
  const addBtn = button(t("ldap.mapAdd"));
  addBtn.addEventListener("click", () => { add({ group: "", roles: [] }); fs.dirty(); });
  return {
    el: [h("h3", { class: "sub-title" }, t("ldap.maps")), box, h("div", { class: "actions" }, addBtn)],
    values: () => maps.map((m) => ({ group: m.group.value.trim(), roles: m.roles.values() })),
    errs: () => maps.map((m, i) => (m.group.value.trim() ? null : t("ldap.e.group", { n: i + 1 }))).filter(Boolean),
  };
}
const linesOf = (ta) => ta.value.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
const checkBox = (on, label, help) => {
  const box = h("input", { type: "checkbox", checked: !!on });
  return { box, el: h("div", { class: "field full" }, h("label", { class: "check" }, box, label), help ? h("small", null, help) : null) };
};

// rutSettings: the Remote User Token realm (a trusted proxy's header names the user).
function rutSettings(rd, rl, isLive) {
  const roleOpts = roleItems(rl.roles || []);
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const header = textIn(rd.header || "REMOTE_USER", { class: "mono", maxlength: 100 });
  const groupHeader = textIn(rd.groupHeader, { class: "mono", maxlength: 100, placeholder: "X-Forwarded-Groups" });
  const proxies = h("textarea", { class: "mono", rows: 3, spellcheck: "false", autocomplete: "off", placeholder: "10.0.0.0/8\n192.168.1.10/32" }, (rd.trustedProxies || []).join("\n"));
  const defaults = checkGroup(roleOpts, rd.defaultRoles || []);
  const maps = mappingRows(roleOpts, rd.groupMappings, "platform-admins", fs);
  const save = h("button", { type: "submit", class: "btn primary" }, t("rut.save"));
  put(form, fs.err, fs.ok, notice(t("rut.warn"), "amber"), fields(
    field(t("rut.header"), header, t("rut.headerHelp")),
    field(t("rut.groupHeader"), groupHeader, t("rut.groupHeaderHelp")),
    field(t("rut.proxies"), proxies, t("rut.proxiesHelp"), { full: true, req: true }),
    groupField(t("ldap.defaults"), defaults.el, t("rut.defaultsHelp"))),
    maps.el, h("p", { class: "hint" }, t("rut.mapsHelp")), h("div", { class: "savebar" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    fs.ok.hidden = true;
    const errs = maps.errs();
    if (!header.value.trim()) errs.unshift(t("rut.e.header"));
    if (errs.length) { fs.show(errs, t("rut.errOne")); return; }
    save.disabled = true;
    try {
      await api("realms/rutauth", { method: "PUT", body: { header: header.value.trim(), groupHeader: groupHeader.value.trim(), trustedProxies: linesOf(proxies),
        defaultRoles: defaults.values(), groupMappings: maps.values() } });
      if (!isLive()) return;
      fs.clean();
      put(fs.ok, t("rut.saved"));
      fs.ok.hidden = false;
    } catch (e) {
      if (isLive()) failWith(fs, e, t("rut.errOne"));
    } finally { save.disabled = false; }
  });
  return [card(t("rut.title"), t("rut.sub"), form)];
}

const CROWD_STEPS = { application: "crowd.step.application", authenticate: "crowd.step.authenticate", user: "crowd.step.user", groups: "crowd.step.groups", name: "crowd.step.name" };

// crowdSettings: Atlassian Crowd (server, application credentials, caching, roles) and a test.
function crowdSettings(cd, rl, isLive) {
  const roleOpts = roleItems(rl.roles || []);
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  let pwSet = !!cd.applicationPasswordSet;
  const url = textIn(cd.url, { type: "url", class: "mono", placeholder: "https://crowd.example.com/crowd", maxlength: 300 });
  const app = textIn(cd.applicationName, { class: "mono", maxlength: 200, autocomplete: "off" });
  const pw = h("input", { type: "password", autocomplete: "new-password", placeholder: pwSet ? t("rf.unchanged") : "" });
  const clearPw = h("input", { type: "checkbox" });
  const clearRow = h("div", { class: "field" }, h("label", { class: "check" }, clearPw, t("crowd.clearPw")));
  clearRow.hidden = !pwSet;
  clearPw.addEventListener("change", () => { pw.disabled = clearPw.checked; if (clearPw.checked) pw.value = ""; });
  const pwHelp = h("span");
  const syncSet = () => put(pwHelp, pwSet ? [h("span", { class: "rf-set" }, t("rf.passSet")), " — ", t("rf.keepEmpty")] : t("crowd.pwHelp"));
  syncSet();
  const pwErr = errBox();
  const pwField = field(t("crowd.appPw"), pw, pwHelp);
  pwField.append(pwErr);
  const timeout = textIn(cd.timeoutSeconds || "", { type: "number", min: 0, max: 60 });
  const cache = textIn(cd.cacheMinutes || "", { type: "number", min: 0, max: 1440 });
  const defaults = checkGroup(roleOpts, cd.defaultRoles || []);
  const maps = mappingRows(roleOpts, cd.groupMappings, "crowd-administrators", fs);
  const save = h("button", { type: "submit", class: "btn primary" }, t("crowd.save"));
  put(form, fs.err, fs.ok, fields(
    field(t("crowd.url"), url, t("crowd.urlHelp"), { full: true }),
    field(t("crowd.app"), app, t("crowd.appHelp")), pwField, clearRow,
    field(t("ldap.timeout"), timeout, t("crowd.timeoutHelp")),
    field(t("crowd.cache"), cache, t("crowd.cacheHelp")),
    groupField(t("ldap.defaults"), defaults.el, t("crowd.defaultsHelp"))),
    maps.el, h("div", { class: "savebar" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    pwErr.hidden = true;
    fs.ok.hidden = true;
    const secret = pw.value;
    pw.value = ""; // the secret never stays in the page
    const tm = timeout.value.trim() === "" ? 0 : Number(timeout.value), cm = cache.value.trim() === "" ? 0 : Number(cache.value);
    const errs = maps.errs();
    if (!Number.isInteger(tm) || tm < 0 || !Number.isInteger(cm) || cm < 0) errs.unshift(t("crowd.e.numbers"));
    if (errs.length) { fs.show(errs, t("crowd.errOne")); return; }
    save.disabled = true;
    try {
      const out = await api("realms/crowd", { method: "PUT", body: { url: url.value.trim(), applicationName: app.value.trim(), applicationPassword: secret,
        clearApplicationPassword: clearPw.checked, timeoutSeconds: tm, cacheMinutes: cm, groupMappings: maps.values(), defaultRoles: defaults.values() } });
      if (!isLive()) return;
      fs.clean();
      pwSet = !!(out && out.applicationPasswordSet);
      clearPw.checked = false;
      pw.disabled = false;
      pw.placeholder = pwSet ? t("rf.unchanged") : "";
      clearRow.hidden = !pwSet;
      syncSet();
      put(fs.ok, t("crowd.saved"));
      fs.ok.hidden = false;
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 400 && REENTER_CROWD.test(e.message)) { put(pwErr, t("crowd.reenter")); pwErr.hidden = false; pw.focus(); } else failWith(fs, e, t("crowd.errOne"));
    } finally { save.disabled = false; }
  });

  // test with the saved configuration; a user and password are optional
  const tUser = textIn("", { autocomplete: "off" });
  const tPw = h("input", { type: "password", autocomplete: "new-password" });
  const run = button(t("ldap.test"), "primary");
  const result = h("div", { "aria-live": "polite" });
  run.addEventListener("click", async () => {
    const body = { username: tUser.value.trim(), password: tPw.value };
    tPw.value = ""; // never kept
    run.disabled = true;
    put(result, loading());
    try {
      const d = await api("realms/crowd/test", { method: "POST", body });
      if (!isLive()) return;
      put(result, badge(d.ok ? t("ldap.testOk") : t("ldap.testFail"), d.ok ? "green" : "red"),
        (d.steps || []).map((x) => h("div", { class: "listrow" },
          h("span", null, h("strong", null, CROWD_STEPS[x.step] ? t(CROWD_STEPS[x.step]) : x.step), x.detail ? h("small", null, x.detail) : null),
          badge(x.ok ? t("ldap.stepOk") : t("ldap.stepFail"), x.ok ? "green" : "red"))));
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); put(result); } else put(result, notice(e.message, "amber"));
    } finally { run.disabled = false; }
  });
  const testCard = card(t("crowd.testTitle"), t("crowd.testSub"), [
    fields(field(t("ldap.testUser"), tUser, t("crowd.testUserHelp")), field(t("ldap.testPw"), tPw)),
    h("div", { class: "actions end" }, run), result]);
  return [card(t("crowd.title"), t("crowd.sub"), form), testCard];
}

// certRows shows SAML certificates: subject, SHA-256 fingerprint and expiry.
const certRows = (list) => list.map((c) => h("div", { class: "listrow" },
  h("span", { class: "wrap" }, h("strong", null, c.subject || dash), h("small", { class: "mono" }, "SHA-256 " + c.sha256)),
  c.expired ? badge(t("ssl.expired"), "red") : h("span", { class: "tiny muted nowrap" }, t("saml.until", { time: fmtDate(c.notAfter) }))));

// samlSettings: the SAML 2.0 service provider (what the IdP needs), the IdP
// it trusts (parsed from the metadata) and the form.
function samlSettings(sd, rl, isLive) {
  const roleOpts = roleItems(rl.roles || []);
  const spBox = h("div"), idpBox = h("div");
  const showInfo = (d) => {
    put(spBox, d.acsUrl ? [
      field(t("saml.acs"), copyLine(d.acsUrl, t("saml.acs")), null),
      field(t("saml.metadataUrl"), copyLine(d.metadataUrl, t("saml.metadataUrl")), t("saml.metadataUrlHelp")),
      kv(t("saml.entity"), h("code", null, d.effectiveEntityId || dash)),
      d.spCertificateInfo ? [h("h3", { class: "sub-title" }, t("saml.spCert")), certRows([d.spCertificateInfo])] : h("p", { class: "hint" }, t("saml.noCert")),
      h("div", { class: "actions" }, h("a", { class: "btn small", href: "/-/api/realms/saml/sp-metadata", download: "sp-metadata.xml" }, t("saml.download")))]
      : notice(t("saml.noBase"), "amber"));
    const i = d.idp;
    put(idpBox, i ? [
      kv(t("saml.idpEntity"), h("code", null, i.entityId)),
      kv(t("saml.sso"), h("span", { class: "mono" }, i.ssoUrl)),
      kv(t("saml.fetched"), i.fetchedAt ? fmtDate(i.fetchedAt) : t("saml.pasted")),
      i.wantAuthnRequestsSigned && !d.signAuthnRequests ? notice(t("saml.wantSigned"), "amber") : null,
      h("h3", { class: "sub-title" }, t("saml.idpCerts")), (i.certificates || []).length ? certRows(i.certificates) : notice(t("saml.noIdpCert"), "amber")]
      : d.idpError ? notice(t("saml.idpError", { msg: d.idpError }), "amber") : h("p", { class: "hint" }, t("saml.noIdp")));
  };
  showInfo(sd);

  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const src = sel([["xml", t("saml.srcXML")], ["url", t("saml.srcURL")]], sd.idpMetadataURL ? "url" : "xml");
  const xml = h("textarea", { class: "mono", rows: 6, spellcheck: "false", autocomplete: "off", placeholder: "<md:EntityDescriptor …>" }, sd.idpMetadataXML || "");
  const murl = textIn(sd.idpMetadataURL, { type: "url", class: "mono", placeholder: "https://idp.example.com/app/metadata", maxlength: 500 });
  const fXml = field(t("saml.xml"), xml, t("saml.xmlHelp"), { full: true }), fUrl = field(t("saml.url"), murl, t("saml.urlHelp"), { full: true });
  const syncSrc = () => { fXml.hidden = src.value !== "xml"; fUrl.hidden = src.value !== "url"; };
  src.addEventListener("change", syncSrc);
  syncSrc();
  const displayName = textIn(sd.displayName, { maxlength: 60, placeholder: "SAML" });
  const entityId = textIn(sd.entityId, { class: "mono", maxlength: 300, placeholder: sd.effectiveEntityId || "" });
  const attr = (v, ph) => textIn(v, { class: "mono", maxlength: 200, placeholder: ph });
  const aUser = attr(sd.usernameAttribute, "uid"), aFirst = attr(sd.firstNameAttribute, "givenName"), aLast = attr(sd.lastNameAttribute, "sn");
  const aEmail = attr(sd.emailAttribute, "mail"), aGroups = attr(sd.groupsAttribute, "groups");
  const defaults = checkGroup(roleOpts, sd.defaultRoles || []);
  const maps = mappingRows(roleOpts, sd.groupMappings, "platform-admins", fs);
  const vResp = checkBox(sd.validateResponseSignature !== false, t("saml.vResp"));
  const vAssert = checkBox(sd.validateAssertionSignature !== false, t("saml.vAssert"), t("saml.vHelp"));
  const sha1 = checkBox(sd.allowSHA1Signatures, t("saml.sha1"), t("saml.sha1Help"));
  const idpInit = checkBox(sd.allowIdpInitiated, t("saml.idpInit"), t("saml.idpInitHelp"));
  const signReq = checkBox(sd.signAuthnRequests, t("saml.signReq"), t("saml.signReqHelp"));
  const skew = textIn(String(sd.clockSkewSeconds ?? 120), { type: "number", min: 0, max: 600 });
  const save = h("button", { type: "submit", class: "btn primary" }, t("saml.save"));
  put(form, fs.err, fs.ok, fields(
    field(t("saml.src"), src, null, { full: true }), fXml, fUrl,
    field(t("oidc.displayName"), displayName, t("saml.displayNameHelp")),
    field(t("saml.entityId"), entityId, t("saml.entityIdHelp", { id: sd.effectiveEntityId || dash })),
    h("div", { class: "field full" }, h("h3", { class: "sub-title" }, t("saml.attrs")), h("small", null, t("saml.attrsHelp"))),
    field(t("saml.aUser"), aUser), field(t("saml.aEmail"), aEmail), field(t("saml.aFirst"), aFirst), field(t("saml.aLast"), aLast),
    field(t("saml.aGroups"), aGroups, t("saml.aGroupsHelp"), { full: true }),
    groupField(t("ldap.defaults"), defaults.el, t("saml.defaultsHelp"))),
    maps.el,
    h("h3", { class: "sub-title" }, t("saml.security")),
    fields(vResp.el, vAssert.el, sha1.el, idpInit.el, signReq.el, field(t("saml.skew"), skew, t("saml.skewHelp"))),
    h("div", { class: "savebar" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    fs.ok.hidden = true;
    const sk = skew.value.trim() === "" ? 120 : Number(skew.value);
    const errs = maps.errs();
    if (!Number.isInteger(sk) || sk < 0) errs.unshift(t("saml.e.skew"));
    if (src.value === "url" && murl.value.trim() && !/^https:\/\//.test(murl.value.trim())) errs.unshift(t("saml.e.url"));
    if (errs.length) { fs.show(errs, t("saml.errOne")); return; }
    save.disabled = true;
    try {
      const out = await api("realms/saml", { method: "PUT", body: {
        displayName: displayName.value.trim(), idpMetadataXML: src.value === "xml" ? xml.value.trim() : "", idpMetadataURL: src.value === "url" ? murl.value.trim() : "",
        entityId: entityId.value.trim(), usernameAttribute: aUser.value.trim(), firstNameAttribute: aFirst.value.trim(), lastNameAttribute: aLast.value.trim(),
        emailAttribute: aEmail.value.trim(), groupsAttribute: aGroups.value.trim(), groupMappings: maps.values(), defaultRoles: defaults.values(),
        validateResponseSignature: vResp.box.checked, validateAssertionSignature: vAssert.box.checked, allowSHA1Signatures: sha1.box.checked,
        allowIdpInitiated: idpInit.box.checked, clockSkewSeconds: sk, signAuthnRequests: signReq.box.checked } });
      if (!isLive()) return;
      fs.clean();
      showInfo(out); // the first save creates the SP certificate; the IdP is parsed again
      entityId.placeholder = out.effectiveEntityId || "";
      put(fs.ok, t("saml.saved"));
      fs.ok.hidden = false;
    } catch (e) {
      if (isLive()) failWith(fs, e, t("saml.errOne"));
    } finally { save.disabled = false; }
  });
  return [h("div", { class: "equal" }, card(t("saml.spTitle"), t("saml.spSub"), spBox), card(t("saml.idpTitle"), t("saml.idpSub"), idpBox)),
    card(t("saml.title"), t("saml.sub"), form)];
}

// ---- user tokens: a name code and pass code that build tools send instead of the password
//   #user/user-token               the caller's own token
//   #admin/security/user-tokens    the same, then every user's token

const XML_ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&apos;" };
const xmlEsc = (v) => String(v).replace(/[&<>"']/g, (ch) => XML_ESC[ch]);
const RESET_WORD = "RESET";

export async function viewUserTokens(view, admin) {
  if (state.me.anonymous) return needLogin(view, t("nav.userTokens"));
  const isLive = alive();
  const me = state.me;
  const shown = h("div");
  const ok = h("div", { class: "notice ok", role: "status", hidden: true });
  let unBusy = null; // the pass code on screen is in-progress work for a language change
  const hideShown = () => { put(shown); if (unBusy) { unBusy(); unBusy = null; } };
  const codeField = (label, text) => h("div", { class: "field" }, h("span", { class: "flabel" }, label), copyLine(text, label));
  const showCodes = (x) => {
    if (!unBusy) unBusy = markBusy("usertoken");
    const hide = button(t("tok.hide"), "small");
    hide.addEventListener("click", hideShown);
    const snippet = "<server>\n  <id>commandcenter</id>\n  <username>" + xmlEsc(x.nameCode) + "</username>\n  <password>" + xmlEsc(x.passCode) + "</password>\n</server>";
    const pre = h("pre", { class: "code" }, snippet);
    put(shown, h("div", { class: "token-box", role: "status" },
      h("strong", null, t("ut.showTitle")),
      h("p", { class: "sub" }, t("ut.showText", { user: x.user })),
      fields(codeField(t("ut.nameCode"), x.nameCode), codeField(t("ut.passCode"), x.passCode)),
      h("div", { class: "snippet-head" }, h("span", null, t("ut.maven")), copyButton(snippet, pre, t("ut.mavenCopy"))), pre,
      h("p", { class: "tiny muted" }, t("ut.otherTools")),
      h("div", { class: "actions end" }, hide)));
  };

  const mine = h("div", null, loading());
  const loadMine = async () => {
    const d = await api("me/usertoken");
    if (!isLive()) return;
    const create = button(d.exists ? t("ut.reset") : t("ut.create"), d.exists ? null : "primary");
    create.disabled = !d.enabled;
    create.addEventListener("click", async () => {
      if (d.exists && !(await confirmDelete(t("ut.resetText"), t("ut.resetTitle"), t("ut.reset")))) return;
      create.disabled = true;
      try {
        const x = await api("me/usertoken", { method: "POST" });
        if (!isLive()) return;
        showCodes(x);
        await loadMine();
        if (admin) await loadAll();
      } catch (e) { create.disabled = false; handleError(e); }
    });
    let del = null;
    if (d.exists) {
      del = button(t("common.delete"), "danger");
      del.addEventListener("click", async () => {
        if (!(await confirmDelete(t("ut.delText"), t("ut.delTitle")))) return;
        del.disabled = true;
        try {
          await api("me/usertoken", { method: "DELETE" });
          if (!isLive()) return;
          hideShown();
          await loadMine();
          if (admin) await loadAll();
        } catch (e) { del.disabled = false; handleError(e); }
      });
    }
    put(mine,
      !d.enabled ? notice(t("ut.disabled"), "amber") : d.required ? notice(t("ut.requiredNote")) : null,
      d.exists ? [
        kv(t("ut.nameCode"), h("code", null, d.nameCode)),
        kv(t("tok.created"), fmtDate(d.created)),
        kv(t("ut.expires"), d.expires ? h("span", null, fmtDate(d.expires), d.expired ? [" ", badge(t("ut.expired"), "red")] : null) : t("ut.never")),
        kv(t("tok.lastUsed"), d.lastUsed ? fmtDate(d.lastUsed) : t("tok.never"))] : h("p", { class: "hint" }, d.enabled ? t("ut.noneMine") : ""),
      h("div", { class: "actions" }, create, del));
  };

  const list = admin ? h("div", null, loading()) : null;
  const loadAll = async () => {
    const d = await api("usertokens");
    if (!isLive()) return;
    const toks = d.userTokens || [];
    const rows = toks.map((x) => {
      const rm = button(t("ut.resetOne"), "small danger");
      rm.addEventListener("click", async () => {
        if (!(await confirmDelete(t("ut.resetOneText", { user: x.user }), t("ut.resetOneTitle"), t("ut.resetOne")))) return;
        rm.disabled = true;
        try {
          await api("usertokens/" + encSeg(x.user), { method: "DELETE" });
          if (!isLive()) return;
          if (x.user === me.owner) { hideShown(); await loadMine(); }
          await loadAll();
        } catch (e) { rm.disabled = false; handleError(e); }
      });
      return row([
        [[h("strong", null, x.userId), x.source !== "local" ? h("div", { class: "tiny muted mono" }, x.user) : null], "name"],
        [badge(realmName(x.source), x.source === "local" ? null : "purple")],
        [h("code", null, x.nameCode)],
        [fmtDate(x.created), "nowrap"],
        [x.expires ? h("span", null, fmtDate(x.expires), x.expired ? [" ", badge(t("ut.expired"), "red")] : null) : t("ut.never"), "nowrap"],
        [x.lastUsed ? fmtDate(x.lastUsed) : badge(t("tok.never")), "nowrap"],
        [rm, "act"],
      ], x.user + " " + x.nameCode);
    });
    const f = filterBar(rows, t("ut.filter"), "ut.count");
    const all = button(t("ut.resetAll"), "small danger");
    all.disabled = !toks.length;
    all.addEventListener("click", () => resetAllDialog(toks.length));
    const c = card(t("ut.all"), t("ut.allSub"), null, all);
    c.append(f.bar, tbl([t("users.user"), t("users.realm"), t("ut.nameCode"), t("tok.created"), t("ut.expires"), t("tok.lastUsed"), actHead()], rows, t("ut.none")), f.none);
    put(list, c);
  };

  // resetAllDialog deletes every user token after the admin types RESET.
  const resetAllDialog = (n) => {
    const input = h("input", { type: "text", autocomplete: "off", spellcheck: "false", autofocus: true });
    const msg = h("div", { class: "form-msg", role: "alert" });
    const cancel = button(t("common.cancel"));
    const go = h("button", { type: "submit", class: "btn danger", disabled: true, form: "ut-all-f" }, t("ut.resetAll"));
    const form = h("form", { id: "ut-all-f", novalidate: true }, h("div", { class: "notice amber" }, t("ut.resetAllText", { n: fmtNum(n) })),
      field(t("ut.typeWord", { word: RESET_WORD }), input), msg);
    const m = modal(t("ut.resetAllTitle"), form, [cancel, go], { cls: "modal-small" });
    cancel.addEventListener("click", m.close);
    input.addEventListener("input", () => { go.disabled = input.value !== RESET_WORD; });
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      if (input.value !== RESET_WORD) return;
      go.disabled = true;
      try {
        const out = await api("usertokens?confirm=true", { method: "DELETE" });
        m.close();
        if (!isLive()) return;
        hideShown();
        put(ok, t("ut.resetAllDone", { n: fmtNum((out && out.deleted) || 0) }));
        ok.hidden = false;
        await Promise.all([loadMine(), loadAll()]);
      } catch (e) {
        go.disabled = false;
        if (e.status === 401) { m.close(); handleError(e); return; }
        msg.textContent = e.message;
      }
    });
  };

  put(view, pageHead(t("nav.userTokens"), sub(t("ut.sub", { user: me.user })),
    admin ? acts(h("a", { class: "btn", href: "#admin/system/general" }, t("ut.settings"))) : null, eyebrow()),
    ok, shown,
    h("div", { class: "two" }, card(t("ut.mine"), t("ut.mineSub"), mine), h("div", { class: "notice" }, icon("info", "hint-ico"), t("ut.about"))),
    list);
  await Promise.all([loadMine(), admin ? loadAll() : null]);
}

// ---- account ---------------------------------------------------------------------

export async function viewAccount(view) {
  if (state.me.anonymous) return needLogin(view, t("nav.account"));
  const me = state.me;
  const tokens = me.admin ? "#admin/security/usertoken" : "#user/usertoken";
  const dirSource = { LDAP: "ldap", OIDC: "oidc", Crowd: "crowd", RUT: "rutauth", SAML: "saml" }[me.realm] || "";
  const ldap = dirSource !== "";
  let pwCard;
  if (ldap) {
    pwCard = card(t("acct.pwTitle"), null, notice(dirSource === "oidc" ? t("acct.oidc") : dirSource === "ldap" ? t("acct.ldap") : t("acct.external", { realm: realmName(dirSource) })));
  } else {
    const cur = pwIn(), pw = pwIn(), pw2 = pwIn();
    cur.autocomplete = "current-password";
    const form = h("form", { novalidate: true });
    const fs = formState(form);
    const save = h("button", { type: "submit", class: "btn primary" }, t("acct.pwSave"));
    put(form, fs.err, fs.ok, fields(field(t("acct.current"), cur, null, { full: true }), field(t("users.newPw"), pw, t("users.pwHelp")), field(t("users.pwConfirm"), pw2)),
      h("div", { class: "savebar" }, save));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      fs.clear();
      fs.ok.hidden = true;
      const c = cur.value, a = pw.value, b = pw2.value;
      cur.value = pw.value = pw2.value = ""; // the secrets never stay in the page
      const msg = !c ? t("acct.e.current") : runes(a) < 8 ? t("access.e.pwShort") : a !== b ? t("access.e.pwMatch") : a === c ? t("acct.e.same") : "";
      if (msg) { fs.show([msg], t("acct.errOne")); return; }
      save.disabled = true;
      try {
        await api("account/password", { method: "PUT", body: { currentPassword: c, newPassword: a } });
        fs.clean();
        put(fs.ok, t("acct.pwDone"));
        fs.ok.hidden = false;
      } catch (e) {
        if (e.status === 401 || (e.status === 403 && !WRONG_PW.test(e.message))) handleError(e);
        else fs.show(e.status === 403 ? [t("acct.wrong")] : e.status === 429 ? [t("login.wait", { s: (e.data && e.data.retryAfter) || 60 })] : errList(e), t("acct.errOne"));
      } finally {
        save.disabled = false;
      }
    });
    pwCard = card(t("acct.pwTitle"), t("acct.pwSub"), form);
  }
  put(view, pageHead(t("nav.account"), sub(me.user), acts(h("a", { class: "btn", href: tokens }, icon("token"), t("nav.tokens")),
    h("a", { class: "btn", href: me.admin ? "#admin/security/user-tokens" : "#user/user-token" }, icon("key"), t("nav.userTokens"))), eyebrow()),
    h("div", { class: "two" },
      card(t("users.info"), t("users.infoSub"), [
        kv(t("users.id"), me.user),
        kv(t("users.realm"), badge(realmName(ldap ? dirSource : "local"), ldap ? "purple" : null)),
        ldap ? kv(t("acct.owner"), h("code", null, me.owner)) : null,
        kv(t("users.roles"), h("span", null, roleBadges(me.roles))),
        kv(t("users.admin"), adminBadge(me.admin)),
      ]), pwCard));
}
