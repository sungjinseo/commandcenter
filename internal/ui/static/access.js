// Access control screens (plan 12 Task 7): users, roles, realms with the
// LDAP settings, and the account page with the password change. The server
// enforces every rule; these screens only collect input and show its answers.
// Passwords are cleared from their inputs as soon as they are read.
//   #admin/security/users[:create|:<id>]
//   #admin/security/roles[:create|:<id>]
//   #admin/security/realms
//   #user/account
import {
  state, h, put, api, enc, alive, filterBar, row, pageHead, loading, card, kv, tbl, handleError, needLogin, icon,
  loadRepos, FORMATS, FORMAT_NAMES,
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
const ACTIONS = ["browse", "read", "add", "edit", "delete"];

const userLink = (id) => "#admin/security/users" + (id ? ":" + enc(id) : "");
const roleLink = (id) => "#admin/security/roles" + (id ? ":" + enc(id) : "");
// dynamic keys go through tables, so every key stays a literal for the i18n test
const REALM_KEYS = { local: ["realms.name.local", "realms.desc.local"], token: ["realms.name.token", "realms.desc.token"], ldap: ["realms.name.ldap", "realms.desc.ldap"] };
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
// a user with both keeps the stored record and the directory's roles as the
// effective ones.
function mergeUsers(list) {
  const stored = new Map(), dir = new Map();
  for (const u of list) (u.directory ? dir : stored).set(u.id, u);
  const out = [...stored.values()].map((u) => Object.assign({}, u, { eff: dir.has(u.id) ? dir.get(u.id).roles : u.roles }));
  for (const [id, u] of dir) if (!stored.has(id)) out.push(Object.assign({}, u, { eff: u.roles }));
  return out;
}
const realmBadge = (u) => (u.source === "ldap" ? badge(realmName("ldap"), "purple") : badge(realmName("local")));

export function viewUsers(view, id) {
  if (id === "create") return userCreate(view);
  return id === undefined ? userList(view) : userDetail(view, id);
}

async function userList(view) {
  const isLive = alive();
  const users = mergeUsers((await api("users")).users || []);
  if (!isLive()) return;
  const rows = users.map((u) => row([
    [h("a", { href: userLink(u.id) }, h("strong", null, u.id)), "name"],
    [u.email || dash],
    [realmBadge(u)],
    [roleBadges(u.eff)],
    [userState(u)],
    [h("a", { class: "btn small", href: userLink(u.id) }, t("common.details")), "act"],
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
  const source = sel([["local", realmName("local")], ["ldap", realmName("ldap")]], "local");
  const pw = pwIn(), pw2 = pwIn();
  const status = sel([["active", t("users.active")], ["disabled", t("users.disabled")]], "active");
  const rg = checkGroup(roleItems(roles), []);
  const pwF = field(t("users.pw"), pw, t("users.pwHelp"), { req: true });
  const pw2F = field(t("users.pwConfirm"), pw2, null, { req: true });
  const ldapNote = notice(t("users.ldapNote"), "");
  ldapNote.hidden = true;
  const syncSource = () => {
    const ldap = source.value === "ldap";
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
    groupField(t("users.roles"), rg.el)), ldapNote, h("div", { class: "actions end" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const ldap = source.value === "ldap";
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
  const u = mergeUsers(ul.users || []).find((x) => x.id === id);
  const back = h("a", { class: "btn", href: userLink() }, t("users.back"));
  if (!u) {
    put(view, pageHead(t("nav.users"), null, acts(back), eyebrow()), notFound(t("users.notFound", { id })));
    return;
  }
  const stored = !u.directory, ldap = u.source === "ldap";
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const effective = (u.eff || []);
  const access = card(t("users.access"), t("users.accessSub"), [
    effective.length ? effective.map((r) => h("div", { class: "listrow" }, h("a", { href: roleLink(r) }, h("strong", null, r)), r === "admin" ? badge(t("users.isAdmin"), "amber") : null))
      : h("div", { class: "empty" }, t("users.noRoles")),
    notice(t("users.tokenNote"))]);
  let head;
  if (!stored) {
    put(form, kv(t("users.id"), u.id), kv(t("users.name"), u.name), kv(t("users.email"), u.email), kv(t("users.realm"), realmBadge(u)),
      kv(t("users.roles"), h("span", null, roleBadges(u.eff))), notice(t("users.dirNote")));
    head = acts(back);
  } else {
    const nameIn = textIn(u.name, { disabled: ldap, maxlength: 200 });
    const emailIn = textIn(u.email, { disabled: ldap, maxlength: 200 });
    const status = sel([["active", t("users.active")], ["disabled", t("users.disabled")]], u.status || "active");
    const rg = checkGroup(roleItems(rl.roles || []), u.roles || []);
    const save = h("button", { type: "submit", class: "btn primary" }, t("users.save"));
    const idIn = textIn(u.id, { disabled: true });
    const srcIn = textIn(realmName(ldap ? "ldap" : "local"), { disabled: true });
    put(form, fs.err, fields(
      field(t("users.id"), idIn), field(t("users.source"), srcIn),
      field(t("users.name"), nameIn), field(t("users.email"), emailIn),
      field(t("col.status"), status), groupField(t("users.roles"), rg.el, ldap ? t("users.overrideHelp") : null)),
    ldap ? notice(t("users.ldapNote")) : null, h("div", { class: "actions end" }, save));
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
    head = acts(back, reset, toggle, del);
  }
  put(view, pageHead(u.id, sub(t("users.realmSub", { realm: realmName(ldap ? "ldap" : "local") })), head, eyebrow()),
    fs.ok, h("div", { class: "two" }, card(t("users.info"), t("users.infoSub"), form), access));
}

// ---- roles -----------------------------------------------------------------------

const targetKind = (tg) => (tg === "*" ? "all" : tg.startsWith("format:") ? "format" : "repo");
const targetValue = (tg) => (tg === "*" ? "" : tg.slice(tg.indexOf(":") + 1));
const scopeText = (tg) => {
  const k = targetKind(tg), v = targetValue(tg);
  return k === "all" ? t("roles.scopeAll") : k === "format" ? t("roles.scopeFormat", { v: FORMAT_NAMES[v] || v }) : t("roles.scopeRepo", { v });
};

export async function viewRoles(view, id) {
  const isLive = alive();
  const [rd, repos] = await Promise.all([api("roles"), loadRepos(true)]);
  if (!isLive()) return;
  const roles = rd.roles || [];
  const creating = id === "create";
  let cur = creating ? { id: "", description: "", privileges: [] } : roles.find((r) => r.id === id) || (id === undefined ? roles[0] : null);
  const add = h("a", { class: "btn primary", href: roleLink("create") }, t("roles.create"));
  const eb = eyebrow();
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
    const kind = sel([["all", t("roles.scopeAll")], ["format", t("roles.kFormat")], ["repo", t("roles.kRepo")]], targetKind(p.target));
    const val = h("select", { "aria-label": t("roles.target") });
    const fill = () => {
      const k = kind.value, keep = val.value || targetValue(p.target);
      const opts = k === "format" ? FORMATS.map((f) => [f, FORMAT_NAMES[f]]) : k === "repo" ? (repoNames.includes(keep) || !keep ? repoNames : repoNames.concat(keep)).map((n) => [n, n]) : [];
      put(val, opts.map(([v, l]) => h("option", { value: v }, l)));
      if (opts.some(([v]) => v === keep)) val.value = keep;
      val.hidden = k === "all";
    };
    kind.id = uid("k");
    fill();
    kind.addEventListener("change", () => { val.value = ""; fill(); });
    const ag = checkGroup(ACTIONS.map((a) => [a, a]), p.actions || []);
    const rm = button(t("roles.removePriv"), "small danger");
    const el = h("div", { class: "edit-row" }, h("div", { class: "row" },
      h("div", { class: "field" }, h("label", { for: kind.id }, t("roles.scope")), kind), h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("roles.target")), val), rm), ag.el);
    const item = { el, target: () => (kind.value === "all" ? "*" : kind.value === "format" ? "format:" + val.value : "repo:" + val.value), actions: ag.values, kind };
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
        (cur.privileges || []).map((p) => h("tr", null, h("td", null, h("code", null, p.target)), h("td", null, scopeText(p.target)),
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
      card(t("roles.privs"), t("roles.privsEditSub"), [rowsBox, h("div", { class: "actions" }, addBtn)]),
      h("div", { class: "actions end" }, cancel, save));
    main.onsubmit = async (ev) => {
      ev.preventDefault();
      fs.clear();
      const rid = creating ? idIn.value.trim() : cur.id;
      const body = { description: descIn.value.trim(), privileges: privs.map((p) => ({ target: p.target(), actions: p.actions() })) };
      const errs = [];
      if (!rid) errs.push(t("roles.e.id"));
      const seen = new Set();
      body.privileges.forEach((p, i) => {
        if (/^(format|repo):$/.test(p.target)) errs.push(t("roles.e.target", { n: i + 1 }));
        if (!p.actions.length) errs.push(t("roles.e.actions", { n: i + 1 }));
        if (seen.has(p.target)) errs.push(t("roles.e.dup", { n: i + 1 }));
        seen.add(p.target);
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

const STEPS = { connect: "ldap.step.connect", starttls: "ldap.step.starttls", bind: "ldap.step.bind", search: "ldap.step.search", userBind: "ldap.step.userBind", groups: "ldap.step.groups" };

export async function viewRealms(view) {
  const isLive = alive();
  const [rd, ld, rl] = await Promise.all([api("realms"), api("realms/ldap"), api("roles")]);
  if (!isLive()) return;
  const avail = rd.available && rd.available.length ? rd.available : ["local", "token", "ldap"];
  let active = (rd.realms || []).filter((r) => r.enabled).map((r) => r.id);
  const root = h("div", { class: "stack" });
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
  const timeout = textIn(ld.timeoutSeconds || "", { type: "number", min: 1, max: 300 });
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
    h("div", { class: "actions" }, addMapBtn), adminCnWarn, adminDefWarn, saveHint, h("div", { class: "actions end" }, save));
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
  // the test fields are not part of the settings: they must not mark the page busy
  put(realmBox, fsR.err, fsR.ok,
    h("div", { class: "equal" }, card(t("realms.active"), t("realms.activeSub"), activeBox), card(t("realms.available"), t("realms.availableSub"), availBox)),
    noLocal, notice(t("realms.lockNote"), "amber"));
  put(root, realmBox, card(t("ldap.title"), t("ldap.sub"), form));
  put(view, pageHead(t("nav.realms"), sub(t("realms.sub")), acts(saveR), eyebrow()), root, testCard);
}

// ---- account ---------------------------------------------------------------------

export async function viewAccount(view) {
  if (state.me.anonymous) return needLogin(view, t("nav.account"));
  const me = state.me;
  const tokens = me.admin ? "#admin/security/usertoken" : "#user/usertoken";
  const ldap = me.realm === "LDAP";
  let pwCard;
  if (ldap) {
    pwCard = card(t("acct.pwTitle"), null, notice(t("acct.ldap")));
  } else {
    const cur = pwIn(), pw = pwIn(), pw2 = pwIn();
    cur.autocomplete = "current-password";
    const form = h("form", { novalidate: true });
    const fs = formState(form);
    const save = h("button", { type: "submit", class: "btn primary" }, t("acct.pwSave"));
    put(form, fs.err, fs.ok, fields(field(t("acct.current"), cur, null, { full: true }), field(t("users.newPw"), pw, t("users.pwHelp")), field(t("users.pwConfirm"), pw2)),
      h("div", { class: "actions end" }, save));
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      fs.clear();
      fs.ok.hidden = true;
      const c = cur.value, a = pw.value, b = pw2.value;
      cur.value = pw.value = pw2.value = ""; // the secrets never stay in the page
      const msg = !c ? t("acct.e.current") : runes(a) < 8 ? t("access.e.pwShort") : a !== b ? t("access.e.pwMatch") : "";
      if (msg) { fs.show([msg], t("acct.errOne")); return; }
      save.disabled = true;
      try {
        await api("account/password", { method: "PUT", body: { currentPassword: c, newPassword: a } });
        fs.clean();
        put(fs.ok, t("acct.pwDone"));
        fs.ok.hidden = false;
      } catch (e) {
        if (e.status === 401 || (e.status === 403 && !WRONG_PW.test(e.message))) handleError(e);
        else fs.show(e.status === 403 ? [t("acct.wrong")] : errList(e), t("acct.errOne"));
      } finally {
        save.disabled = false;
      }
    });
    pwCard = card(t("acct.pwTitle"), t("acct.pwSub"), form);
  }
  put(view, pageHead(t("nav.account"), sub(me.user), acts(h("a", { class: "btn", href: tokens }, icon("token"), t("nav.tokens"))), eyebrow()),
    h("div", { class: "two" },
      card(t("users.info"), t("users.infoSub"), [
        kv(t("users.id"), me.user),
        kv(t("users.realm"), badge(realmName(ldap ? "ldap" : "local"), ldap ? "purple" : null)),
        kv(t("users.roles"), h("span", null, roleBadges(me.roles))),
        kv(t("users.admin"), adminBadge(me.admin)),
      ]), pwCard));
}
