// System screens (plan 12 Task 8): blob stores and the general settings with
// the e-mail (SMTP) configuration. The server enforces every rule; these
// screens collect input and show its answers. The SMTP password is cleared
// from its input as soon as it is read.
//   #admin/repository/blobstores[:create|:<name>]
//   #admin/system/general, emailserver and http, #admin/security/anonymous and
//   the user token settings of #admin/security/usertoken: one form, split by part
import {
  h, put, adminRepoLink, api, enc, alive, pageHead, loading, card, kv, bar, tbl, handleError, loadRepos, fmtBytes, fmtNum, fmtDate, typeBadge,
  OSV_FORMATS, FORMAT_NAMES, sevBadge, osvLink, icon,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete } from "./browse.js";
import { modal } from "./shell.js";
import { field, fields, sel, textIn, button, formState, errList, checks } from "./ops.js";

const dash = "—";
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const acts = (...b) => h("div", { class: "actions" }, b);
const sub = (text) => h("p", null, text);
const notice = (text, cls) => h("div", { class: "notice" + (cls ? " " + cls : "") }, text);
const errBox = () => h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
const notFound = (text) => h("section", { class: "card" }, h("div", { class: "empty" }, text));
const REENTER = /re-enter the password/; // pinned to the server text by TestSettingsMessagesPinned
const WAIT = 429;

const storeLink = (name) => "#admin/repository/blobstores" + (name ? ":" + enc(name) : "");
const known = (n) => typeof n === "number" && n > 0;
const pctOf = (used, total) => (known(total) ? (Number(used) / total) * 100 : 0);

const storeType = (s) => s.type === "s3" ? "S3" : s.type === "azure" ? "Azure" : s.type === "google" ? "Google Cloud" : s.type === "group" ? t("blob.group") : t("blob.file");
const cloud = (s) => ["s3", "azure", "google"].includes(s.type); // no local filesystem figures
const memberText = (m) => m.name + (m.readOnly ? " (" + t("blob.ro") + ")" : "");
const storeLocation = (s) => s.type === "s3" ? "s3://" + ((s.s3 || {}).bucket || "") + "/" + ((s.s3 || {}).prefix || "")
  : s.type === "azure" ? "azure://" + ((s.azure || {}).accountName || "") + "/" + ((s.azure || {}).containerName || "") + "/" + ((s.azure || {}).prefix || "")
  : s.type === "google" ? "gs://" + ((s.google || {}).bucket || "") + "/" + ((s.google || {}).prefix || "")
  : s.type === "group" ? (s.members || []).map(memberText).join(", ") : s.path;
// An Azure store's authentication: the methods and the fields each one takes; a
// secret is write-only (the view says only whether it is set).
const AZ_USES = { accountKey: ["accountKey"], sasToken: ["sasToken"], clientSecret: ["tenantId", "clientId", "clientSecret", "authorityHost"],
  managedIdentity: ["clientId", "imdsEndpoint"], workloadIdentity: ["tenantId", "clientId", "authorityHost"] };
const AZ = "blob.az.", AZ_HELP = "blob.azh."; // + the method or field name
const AZ_SECRET = { accountKey: "accountKeySet", sasToken: "sasTokenSet", clientSecret: "clientSecretSet" };
// azureAuth edits the method and its fields; cur is the store's view when the
// credentials are replaced (an empty secret keeps the stored one).
function azureAuth(cur) {
  cur = cur || {};
  const method = sel(Object.keys(AZ_USES).map((m) => [m, t(AZ + m)]), cur.authenticationMethod || "accountKey");
  const inp = {}, fl = {};
  for (const k of ["accountKey", "sasToken", "tenantId", "clientId", "clientSecret", "authorityHost", "imdsEndpoint"]) {
    const set = AZ_SECRET[k] && cur[AZ_SECRET[k]];
    inp[k] = AZ_SECRET[k] ? h("input", { type: "password", autocomplete: "new-password", placeholder: set ? t("rf.unchanged") : "" })
      : textIn(cur[k] || "", { class: "mono", type: /Host|Endpoint/.test(k) ? "url" : "text", maxlength: 300 });
    fl[k] = field(t(AZ + k), inp[k], set ? [h("span", { class: "rf-set" }, t("blob.secretSet")), " — ", t("rf.keepEmpty")] : t(AZ_HELP + k));
  }
  const sync = () => { for (const k in fl) fl[k].hidden = !AZ_USES[method.value].includes(k); };
  method.addEventListener("change", sync);
  sync();
  return {
    els: [field(t("blob.az.method"), method, t("blob.az.methodHelp"), { full: true }), ...Object.values(fl)],
    value() {
      const o = { authenticationMethod: method.value };
      for (const k of AZ_USES[method.value]) o[k] = AZ_SECRET[k] ? inp[k].value : inp[k].value.trim();
      return o;
    },
    clear() { for (const k in AZ_SECRET) inp[k].value = ""; },
  };
}
// mask hides the secrets just sent in the server's messages.
const mask = (list, secrets) => list.map((m) => secrets.filter(Boolean).reduce((x, v) => String(x).split(v).join("••••"), m));
const MiB = 1048576, GiB = 1073741824;
const quotaText = (q) => (q ? (q.type === "spaceRemainingBytes" ? t("blob.qRemainingText", { n: fmtBytes(q.limitBytes) }) : t("blob.qUsedText", { n: fmtBytes(q.limitBytes) })) : t("blob.qNone"));
// storeFlags: a violated quota and the group a store belongs to.
const storeFlags = (s) => [s.quotaViolated ? badge(t("blob.qViolated"), "red") : null, s.memberOf ? badge(t("blob.memberOf", { g: s.memberOf }), "blue") : null].filter(Boolean);
const FILL = () => [["roundRobin", t("blob.fillRR")], ["writeToFirst", t("blob.fillFirst")]];

// quotaEditor edits a soft quota: a type (none removes it), a limit and its
// unit. value() is null (none), {type, limitBytes}, or undefined (invalid).
function quotaEditor(q) {
  const type = sel([["", t("blob.qNone")], ["spaceUsedBytes", t("blob.qUsed")], ["spaceRemainingBytes", t("blob.qRemaining")]], q ? q.type : "");
  const inGiB = !q || q.limitBytes % GiB === 0;
  const unit = sel([["MiB", "MiB"], ["GiB", "GiB"]], inGiB ? "GiB" : "MiB");
  const shown = q ? String(+(q.limitBytes / (inGiB ? GiB : MiB)).toFixed(4)) : "";
  const num = h("input", { type: "number", inputmode: "decimal", min: 0, step: "any", value: shown });
  const fLimit = field(t("blob.qLimit"), num), fUnit = field(t("blob.qUnit"), unit);
  const sync = () => { fLimit.hidden = fUnit.hidden = !type.value; };
  type.addEventListener("change", sync);
  sync();
  return {
    els: [field(t("blob.quota"), type, t("blob.quotaHelp"), { full: true }), fLimit, fUnit],
    value: () => {
      if (!type.value) return null;
      if (q && type.value === q.type && num.value === shown && unit.value === (inGiB ? "GiB" : "MiB")) return { type: q.type, limitBytes: q.limitBytes }; // untouched: exact
      const v = Number(num.value);
      return v > 0 ? { type: type.value, limitBytes: Math.round(v * (unit.value === "GiB" ? GiB : MiB)) } : undefined;
    },
  };
}

// rowsEditor is a list of rows built by make(item) → {el, value}; rows can
// move up and be removed, and add() appends one (up to max).
function rowsEditor(items, make, addLabel, max) {
  const rows = [], box = h("div");
  const more = button(addLabel, "small");
  const sync = () => { more.disabled = !!max && rows.length >= max; };
  const add = (item) => {
    const r = make(item);
    const up = button("↑", "small"), rm = button(t("ldap.mapRemove"), "small danger");
    up.setAttribute("aria-label", t("blob.moveUp"));
    const line = h("div", { class: "row map-row" }, r.el, up, rm);
    const entry = { line, value: r.value };
    up.addEventListener("click", () => {
      const i = rows.indexOf(entry);
      if (i > 0) { rows.splice(i, 1); rows.splice(i - 1, 0, entry); box.insertBefore(line, rows[i].line); }
    });
    rm.addEventListener("click", () => { rows.splice(rows.indexOf(entry), 1); line.remove(); sync(); });
    rows.push(entry);
    box.appendChild(line);
    sync();
  };
  items.forEach(add);
  more.addEventListener("click", () => add(null));
  sync();
  return { el: h("div", null, box, h("div", { class: "actions" }, more)), values: () => rows.map((r) => r.value()) };
}

// membersEditor picks a group's members in order, each writable or read-only.
function membersEditor(names, members) {
  return rowsEditor(members, (m) => {
    const opts = names.slice();
    if (m && !opts.includes(m.name)) opts.unshift(m.name);
    const s = sel(opts.map((n) => [n, n]), m ? m.name : opts[0] || "", { "aria-label": t("blob.member") });
    const ro = h("input", { type: "checkbox", checked: !!(m && m.readOnly) });
    return { el: [h("div", { class: "grow" }, s), h("label", { class: "check" }, ro, t("blob.ro"))], value: () => ({ name: s.value, readOnly: ro.checked }) };
  }, t("blob.memberAdd"));
}
// groupCandidates: stores that may join a group (not groups, not default, in no other group).
const groupCandidates = (list, group) => list.filter((s) => s.type !== "group" && s.name !== "default" &&
  (group ? !s.memberOf || s.memberOf === group : !s.memberOf && !s.repoCount)).map((s) => s.name);

// ---- blob stores -----------------------------------------------------------------

export async function viewBlobStores(view, name) {
  if (name === "create") return storeCreate(view);
  if (name !== undefined) return storeDetail(view, name);
  const isLive = alive();
  const list = (await api("blobstores")).blobstores || [];
  if (!isLive()) return;
  const fs = formState(h("form")); // carries the "deleted" notice from the detail page
  put(view,
    pageHead(t("nav.blobstores"), sub(t("blob.sub")), acts(h("a", { class: "btn primary", href: storeLink("create") }, t("blob.create"))), { eyebrow: t("blob.eyebrow") }),
    fs.ok,
    card(t("nav.blobstores"), null, tbl([
      t("col.name"), t("col.type"), t("blob.path"),
      { t: t("blob.usage"), cls: "num" }, { t: t("blob.count"), cls: "num" },
      { t: t("pol.repos"), cls: "num" },
    ], list.map((s) => h("tr", null,
      h("td", null, h("a", { href: storeLink(s.name) }, h("strong", null, s.name)), storeFlags(s).length ? h("div", null, storeFlags(s)) : null),
      h("td", { class: "nowrap" }, storeType(s)), h("td", null, h("code", null, storeLocation(s))),
      h("td", { class: "num" }, fmtBytes(s.usedBytes || 0), known(s.fsTotal) && !cloud(s) ? " / " + fmtBytes(s.fsTotal) : null),
      h("td", { class: "num" }, fmtNum(s.blobCount || 0)),
      h("td", { class: "num" }, fmtNum(s.repoCount || 0)))), t("blob.none"))),
    notice(t("blob.note")));
}

async function storeCreate(view) {
  const isLive = alive();
  const all = (await api("blobstores")).blobstores || [];
  if (!isLive()) return;
  const back = h("a", { class: "btn", href: storeLink() }, t("blob.back"));
  const nameIn = textIn("", { required: true, maxlength: 64 });
  const pathIn = textIn("", { class: "mono", placeholder: "/data/blobstores/archive" });
  const type = sel([["file", t("blob.file")], ["s3", "S3"], ["azure", "Azure Blob Storage"], ["google", "Google Cloud Storage"], ["group", t("blob.group")]], "file");
  const quota = quotaEditor(null);
  const cand = groupCandidates(all, "");
  const members = membersEditor(cand, cand.slice(0, 2).map((n) => ({ name: n, readOnly: false })));
  const fill = sel(FILL(), "roundRobin");
  const groupFields = fields(
    h("div", { class: "field full" }, h("span", { class: "flabel" }, t("blob.members")), members.el, h("small", null, cand.length ? t("blob.membersHelp") : t("blob.noCandidates"))),
    field(t("blob.fill"), fill, t("blob.fillHelp"), { full: true }));
  const encType = sel([["", t("blob.encNone")], ["s3ManagedEncryption", "SSE-S3 (AES256)"], ["kmsManagedEncryption", "SSE-KMS"]], "");
  const kmsKey = textIn("", { class: "mono", placeholder: "arn:aws:kms:…:key/…", maxlength: 2048 });
  const kmsField = field(t("blob.kmsKey"), kmsKey, t("blob.kmsKeyHelp"));
  encType.addEventListener("change", () => { kmsField.hidden = encType.value !== "kmsManagedEncryption"; });
  kmsField.hidden = true;
  const failover = rowsEditor([], () => {
    const region = textIn("", { placeholder: "eu-west-1", "aria-label": t("blob.region") });
    const bucket = textIn("", { placeholder: "replica-bucket", "aria-label": t("blob.bucket") });
    const ep = textIn("", { type: "url", placeholder: "https://s3.eu-west-1.amazonaws.com", "aria-label": t("blob.endpoint") });
    return { el: [h("div", { class: "grow" }, region), h("div", { class: "grow" }, bucket), h("div", { class: "grow" }, ep)],
      value: () => ({ region: region.value.trim(), bucket: bucket.value.trim(), endpoint: ep.value.trim() }) };
  }, t("blob.failoverAdd"), 5);
  const bucket = textIn("", { required: true }), prefix = textIn(""), region = textIn(""), endpoint = textIn("", { type: "url", placeholder: "https://s3.example.com" });
  const accessKey = textIn("", { autocomplete: "off" });
  const secretKey = h("input", { type: "password", autocomplete: "new-password" });
  const pathStyle = h("input", { type: "checkbox" });
  const fileFields = fields(field(t("blob.path"), pathIn, t("blob.pathHelp"), { full: true }));
  const s3Fields = fields(field(t("blob.bucket"), bucket, null, { req: true }), field(t("blob.prefix"), prefix),
    field(t("blob.region"), region), field(t("blob.endpoint"), endpoint),
    field(t("blob.accessKey"), accessKey), field(t("blob.secretKey"), secretKey, t("blob.secretHelp")),
    field(t("blob.pathStyle"), pathStyle), field(t("blob.encryption"), encType, t("blob.encryptionHelp")), kmsField,
    h("div", { class: "field full" }, h("span", { class: "flabel" }, t("blob.failover")), failover.el, h("small", null, t("blob.failoverHelp"))));
  // Azure: the account, the container and the authentication.
  const azAccount = textIn("", { class: "mono", placeholder: "mystorageaccount", maxlength: 24 }), azContainer = textIn("", { class: "mono", maxlength: 63 });
  const azPrefix = textIn("", { class: "mono" }), azEndpoint = textIn("", { type: "url", class: "mono", placeholder: "https://<account>.blob.core.windows.net" });
  const az = azureAuth(null);
  const azFields = fields(field(t("blob.az.account"), azAccount, null, { req: true }), field(t("blob.az.container"), azContainer, t("blob.az.containerHelp"), { req: true }),
    field(t("blob.prefix"), azPrefix), field(t("blob.endpoint"), azEndpoint, t("blob.az.endpointHelp")), ...az.els);
  // Google Cloud Storage: the bucket and a service account key (pasted, or read from its file here).
  const gBucket = textIn("", { class: "mono" }), gPrefix = textIn("", { class: "mono" });
  const gEndpoint = textIn("", { type: "url", class: "mono", placeholder: "https://storage.googleapis.com" });
  const gAuth = sel([["accountKey", t("blob.g.key")], ["applicationDefault", t("blob.g.adc")], ["none", t("blob.g.none")]], "accountKey");
  const gKey = h("input", { type: "password", autocomplete: "off", placeholder: '{"type": "service_account", …}' });
  const gFile = h("input", { type: "file", accept: ".json,application/json", "aria-label": t("blob.g.file") });
  const gWho = h("span", { class: "rf-set" });
  const keyOf = () => {
    let email = "";
    try { email = JSON.parse(gKey.value).client_email || ""; } catch (e) { /* not JSON yet */ }
    gWho.textContent = gKey.value ? (email ? t("blob.g.keyFor", { email }) : t("blob.g.notKey")) : "";
  };
  gKey.addEventListener("input", keyOf);
  gFile.addEventListener("change", async () => {
    const f = gFile.files && gFile.files[0];
    gKey.value = f ? await f.text() : ""; // newlines go: the key's own are \n escapes
    gFile.value = "";
    keyOf();
  });
  const fKey = field(t("blob.g.accountKey"), gKey, [gWho, " ", t("blob.g.keyHelp")], { full: true });
  const fFile = h("div", { class: "field full" }, h("span", { class: "flabel" }, t("blob.g.file")), gFile);
  const syncG = () => { fKey.hidden = fFile.hidden = gAuth.value !== "accountKey"; };
  gAuth.addEventListener("change", syncG);
  syncG();
  const gFields = fields(field(t("blob.bucket"), gBucket, null, { req: true }), field(t("blob.prefix"), gPrefix),
    field(t("blob.g.auth"), gAuth, t("blob.g.authHelp")), field(t("blob.endpoint"), gEndpoint, t("blob.g.endpointHelp")), fKey, fFile);
  const syncType = () => {
    fileFields.hidden = type.value !== "file";
    s3Fields.hidden = type.value !== "s3";
    azFields.hidden = type.value !== "azure";
    gFields.hidden = type.value !== "google";
    if (type.value !== "azure") az.clear();
    if (type.value !== "google") gKey.value = "";
    groupFields.hidden = type.value !== "group";
    pathIn.disabled = type.value !== "file";
    for (const el of s3Fields.querySelectorAll("input, select")) el.disabled = type.value !== "s3";
    if (type.value !== "s3") secretKey.value = "";
  };
  for (const f of [fileFields, s3Fields, azFields, gFields, groupFields]) f.classList.add("more");
  type.addEventListener("change", syncType);
  syncType();
  const save = h("button", { type: "submit", class: "btn primary" }, t("blob.createBtn"));
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  put(form, fs.err,
    fields(field(t("blob.name"), nameIn, t("blob.nameHelp"), { req: true }), field(t("blob.type"), type)), fileFields, s3Fields, azFields, gFields, groupFields,
    h("h3", { class: "sub-title" }, t("blob.quotaTitle")), fields(...quota.els),
    h("div", { class: "actions end" }, back, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (save.disabled) return;
    fs.clear();
    const body = { name: nameIn.value.trim() };
    if (!body.name) { fs.show([t("blob.e.name")]); return; }
    const q = quota.value();
    if (q === undefined) { fs.show([t("blob.e.quota")]); return; }
    if (q) body.quota = q;
    if (type.value === "s3") {
      if (!bucket.value.trim()) { fs.show([t("blob.e.bucket")]); return; }
      const fo = failover.values().filter((f) => f.bucket || f.region || f.endpoint);
      if (fo.some((f) => !f.bucket)) { fs.show([t("blob.e.failover")]); return; }
      body.type = "s3";
      body.s3 = { bucket: bucket.value.trim(), prefix: prefix.value.trim(), region: region.value.trim(), endpoint: endpoint.value.trim(),
        pathStyle: pathStyle.checked, accessKeyId: accessKey.value.trim(), secretAccessKey: secretKey.value,
        encryption: encType.value ? { type: encType.value, kmsKeyId: encType.value === "kmsManagedEncryption" ? kmsKey.value.trim() : "" } : null,
        failover: fo };
    } else if (type.value === "azure") {
      if (!azAccount.value.trim() || !azContainer.value.trim()) { fs.show([t("blob.e.azure")]); return; }
      body.type = "azure";
      body.azure = Object.assign({ accountName: azAccount.value.trim(), containerName: azContainer.value.trim(), prefix: azPrefix.value.trim(), endpoint: azEndpoint.value.trim() }, az.value());
    } else if (type.value === "google") {
      if (!gBucket.value.trim()) { fs.show([t("blob.e.gBucket")]); return; }
      body.type = "google";
      body.google = { bucket: gBucket.value.trim(), prefix: gPrefix.value.trim(), endpoint: gEndpoint.value.trim(), authentication: gAuth.value };
      if (gAuth.value === "accountKey") body.google.accountKey = gKey.value;
    } else if (type.value === "group") {
      body.type = "group";
      body.members = members.values();
      body.fillPolicy = fill.value;
      if (!body.members.length) { fs.show([t("blob.e.members")]); return; }
      if (body.members.every((m) => m.readOnly)) { fs.show([t("blob.e.writable")]); return; }
    } else body.path = pathIn.value.trim(); // retain compatibility with file-only servers
    secretKey.value = "";
    const sent = [body.s3 && body.s3.secretAccessKey, ...(body.azure ? [body.azure.accountKey, body.azure.sasToken, body.azure.clientSecret] : []), body.google && body.google.accountKey];
    az.clear();
    gKey.value = "";
    save.disabled = true;
    try {
      const out = await api("blobstores", { method: "POST", body });
      if (isLive()) fs.done(storeLink(out.name), t("blob.created"));
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      fs.show(mask(errList(e), sent));
    } finally {
      if (body.s3) delete body.s3.secretAccessKey;
      delete body.azure;
      delete body.google;
      keyOf();
    }
  });
  put(view, pageHead(t("blob.createTitle"), sub(t("blob.createSub")), acts(back), { eyebrow: t("blob.eyebrow") }),
    h("div", { class: "two" }, card(t("blob.settings"), t("blob.createCardSub"), form), h("div", null)));
  nameIn.focus();
}

async function storeDetail(view, name) {
  const isLive = alive();
  const back = h("a", { class: "btn", href: storeLink() }, t("blob.back"));
  let s;
  try {
    s = await api("blobstores/" + enc(name));
  } catch (e) {
    if (e.status !== 404) throw e;
    if (!isLive()) return;
    put(view, pageHead(t("nav.blobstores"), null, acts(back), { eyebrow: t("blob.eyebrow") }), notFound(t("blob.notFound", { name })));
    return;
  }
  const [repos, all] = await Promise.all([loadRepos(true), s.type === "group" ? api("blobstores").then((d) => d.blobstores || []) : null]);
  if (!isLive()) return;
  const users = repos.filter((r) => (r.blobStore || "default") === s.name);
  const fs = formState(h("form"));
  const isGroup = s.type === "group";
  const err = errBox();
  const del = button(t("common.delete"), "danger");
  del.addEventListener("click", async () => {
    err.hidden = true;
    if (!(await confirmDelete(t("blob.delText", { name: s.name }), t("blob.delTitle")))) return;
    del.disabled = true;
    try {
      await api("blobstores/" + enc(s.name), { method: "DELETE" });
    } catch (e) {
      del.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      if (e.status === 409) {
        const n = e.data && Array.isArray(e.data.repositories) ? e.data.repositories.length : 0;
        put(err, n ? t("blob.inUse", { n: fmtNum(n) }) : s.name === "default" ? t("blob.defaultFixed") : e.message);
      } else {
        put(err, errList(e).join(" "));
      }
      err.hidden = false;
      err.focus();
      return;
    }
    if (isLive()) fs.done(storeLink(), t("blob.deleted", { name: s.name }));
  });
  const p = pctOf(s.usedBytes, s.fsTotal);
  const settings = card(t("blob.settings"), t("blob.settingsSub"), [
    err,
    kv(t("blob.name"), s.name),
    kv(t("blob.type"), storeType(s)),
    isGroup ? null : kv(t("blob.path"), h("span", { class: "mono" }, storeLocation(s))),
    s.type === "s3" ? [
      kv(t("blob.bucket"), (s.s3 || {}).bucket), kv(t("blob.prefix"), (s.s3 || {}).prefix),
      kv(t("blob.region"), (s.s3 || {}).region), kv(t("blob.endpoint"), (s.s3 || {}).endpoint),
      kv(t("blob.pathStyle"), String(!!(s.s3 || {}).pathStyle)), kv(t("blob.sse"), (s.s3 || {}).sse),
      kv(t("blob.accessKey"), (s.s3 || {}).accessKeyId || (s.s3 || {}).accessKey),
      kv(t("blob.secretKey"), typeof ((s.s3 || {}).secretSet ?? s.secretSet) === "boolean"
        ? (((s.s3 || {}).secretSet ?? s.secretSet) ? t("blob.secretSet") : t("blob.secretUnset")) : dash),
      kv(t("blob.encryption"), (s.s3 || {}).encryption ? (s.s3.encryption.type === "kmsManagedEncryption" ? "SSE-KMS" : "SSE-S3") + (s.s3.encryption.kmsKeyId ? " · " + s.s3.encryption.kmsKeyId : "") : t("blob.encNone")),
      kv(t("blob.failover"), ((s.s3 || {}).failover || []).length ? h("span", null, s.s3.failover.map((f) => h("div", { class: "mono tiny" }, [f.region, f.bucket, f.endpoint].filter(Boolean).join(" · ")))) : dash),
    ] : null,
    s.type === "azure" && s.azure ? [
      kv(t("blob.az.account"), s.azure.accountName), kv(t("blob.az.container"), s.azure.containerName), kv(t("blob.prefix"), s.azure.prefix),
      kv(t("blob.endpoint"), s.azure.endpoint), kv(t("blob.az.method"), t(AZ + s.azure.authenticationMethod)),
      ["tenantId", "clientId", "authorityHost", "imdsEndpoint"].filter((k) => s.azure[k]).map((k) => kv(t(AZ + k), h("span", { class: "mono" }, s.azure[k]))),
      Object.entries(AZ_SECRET).filter(([k]) => AZ_USES[s.azure.authenticationMethod]?.includes(k)).map(([k, f]) => kv(t(AZ + k), s.azure[f] ? t("blob.secretSet") : t("blob.secretUnset"))),
    ] : null,
    s.type === "google" && s.google ? [
      kv(t("blob.bucket"), s.google.bucket), kv(t("blob.prefix"), s.google.prefix), kv(t("blob.endpoint"), s.google.endpoint),
      kv(t("blob.g.auth"), { accountKey: t("blob.g.key"), applicationDefault: t("blob.g.adc"), none: t("blob.g.none") }[s.google.authentication] || s.google.authentication),
      s.google.authentication === "accountKey" ? kv(t("blob.g.accountKey"), s.google.accountKeySet ? t("blob.secretSet") : t("blob.secretUnset")) : null,
      s.google.serviceAccount ? kv(t("blob.g.account"), h("span", { class: "mono" }, s.google.serviceAccount)) : null,
    ] : null,
    isGroup ? [kv(t("blob.fill"), (FILL().find(([v]) => v === s.fillPolicy) || [0, s.fillPolicy])[1]),
      kv(t("blob.members"), h("span", null, (s.members || []).map((m) => h("div", null, h("a", { href: storeLink(m.name) }, m.name), m.readOnly ? [" ", badge(t("blob.ro"))] : null))))] : null,
    s.memberOf ? kv(t("blob.memberOfLabel"), h("a", { href: storeLink(s.memberOf) }, s.memberOf)) : null,
    kv(t("blob.quota"), h("span", null, quotaText(s.quota), s.quotaViolated ? [" ", badge(t("blob.qViolated"), "red")] : null)),
    h("p", { class: "hint" }, t("blob.pathFixed")),
    s.warning ? notice(t("blob.outside"), "amber") : null]);
  const usage = card(t("blob.usage"), t("blob.usageSub"), [
    kv(t("blob.used"), fmtBytes(s.usedBytes || 0)),
    kv(t("blob.count"), fmtNum(s.blobCount || 0)),
    kv(t("blob.repos"), fmtNum(s.repoCount || 0)),
    kv(t("blob.free"), known(s.fsTotal) && s.fsFree != null && !cloud(s) ? fmtBytes(s.fsFree) : t("blob.capUnknown")),
    known(s.fsTotal) && !cloud(s) ? bar(p, p > 70 ? "amber" : null) : null]);
  const using = card(t("blob.usedBy"), t("blob.usedBySub"), users.length ? tbl([t("col.repo"), t("col.type")],
    users.map((r) => h("tr", null, h("td", { class: "name" }, h("a", { href: adminRepoLink(r.name) }, h("strong", null, r.name))), h("td", null, typeBadge(r.type)))))
    : h("div", { class: "empty" }, t("blob.noRepos")));
  // soft quota: the one setting a file or S3 store can change (a group's members too)
  const qForm = h("form", { novalidate: true });
  const qfs = formState(qForm);
  const quota = quotaEditor(s.quota);
  const qSave = h("button", { type: "submit", class: "btn primary" }, t("blob.qSave"));
  put(qForm, qfs.err, fields(...quota.els), h("div", { class: "actions end" }, qSave));
  qForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    qfs.clear();
    const q = quota.value();
    if (q === undefined) { qfs.show([t("blob.e.quota")]); return; }
    qSave.disabled = true;
    try {
      await api("blobstores/" + enc(s.name), { method: "PUT", body: { quota: q } });
      if (isLive()) qfs.done(storeLink(s.name), q ? t("blob.qSaved") : t("blob.qRemoved"));
    } catch (e) {
      qSave.disabled = false;
      if (isLive()) { if (e.status === 401 || e.status === 403) handleError(e); else qfs.show(errList(e)); }
    }
  });
  const quotaCard = card(t("blob.quotaTitle"), t("blob.quotaSub"), qForm);

  // a group's members and fill policy
  let groupCard = null;
  if (isGroup) {
    const gForm = h("form", { novalidate: true });
    const gfs = formState(gForm);
    const members = membersEditor(groupCandidates(all, s.name), s.members || []);
    const fill = sel(FILL(), s.fillPolicy || "roundRobin");
    const gSave = h("button", { type: "submit", class: "btn primary" }, t("blob.gSave"));
    put(gForm, gfs.err, fields(
      h("div", { class: "field full" }, h("span", { class: "flabel" }, t("blob.members")), members.el, h("small", null, t("blob.membersEditHelp"))),
      field(t("blob.fill"), fill, t("blob.fillHelp"), { full: true })), h("div", { class: "actions end" }, gSave));
    gForm.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      gfs.clear();
      const m = members.values();
      if (!m.length) { gfs.show([t("blob.e.members")]); return; }
      gSave.disabled = true;
      try {
        await api("blobstores/" + enc(s.name), { method: "PUT", body: { members: m, fillPolicy: fill.value } });
        if (isLive()) gfs.done(storeLink(s.name), t("blob.gSaved"));
      } catch (e) {
        gSave.disabled = false;
        if (isLive()) { if (e.status === 401 || e.status === 403) handleError(e); else gfs.show(errList(e)); }
      }
    });
    groupCard = card(t("blob.groupTitle"), t("blob.groupSub"), gForm);
  }

  // an Azure store's credentials (its account, container, prefix and endpoint stay)
  let azCard = null;
  if (s.type === "azure" && s.azure) {
    const aForm = h("form", { novalidate: true });
    const afs = formState(aForm);
    const ed = azureAuth(s.azure);
    const ro = (v) => textIn(v || "", { readonly: true, class: "mono" });
    const aSave = h("button", { type: "submit", class: "btn primary" }, t("blob.az.save"));
    put(aForm, afs.err, fields(field(t("blob.az.account"), ro(s.azure.accountName)), field(t("blob.az.container"), ro(s.azure.containerName)),
      field(t("blob.prefix"), ro(s.azure.prefix)), field(t("blob.endpoint"), ro(s.azure.endpoint)), ...ed.els), h("div", { class: "actions end" }, aSave));
    aForm.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      afs.clear();
      const azure = ed.value();
      ed.clear();
      aSave.disabled = true;
      try {
        await api("blobstores/" + enc(s.name), { method: "PUT", body: { azure } });
        if (isLive()) afs.done(storeLink(s.name), t("blob.az.saved"));
      } catch (e) {
        aSave.disabled = false;
        if (isLive()) { if (e.status === 401 || e.status === 403) handleError(e); else afs.show(mask(errList(e), [azure.accountKey, azure.sasToken, azure.clientSecret])); }
      }
    });
    azCard = card(t("blob.az.title"), t("blob.az.sub"), aForm);
  }

  // health check of every target (directory, primary and failover buckets, members)
  const hRun = button(t("blob.health"), "small");
  const hOut = h("div", { "aria-live": "polite" }, h("p", { class: "hint" }, t("blob.healthIdle")));
  hRun.addEventListener("click", async () => {
    hRun.disabled = true;
    put(hOut, loading());
    try {
      const d = await api("blobstores/" + enc(s.name) + "/health");
      if (!isLive()) return;
      put(hOut, h("p", null, d.healthy ? badge(t("blob.healthOk"), "green") : badge(t("blob.healthBad"), "red")),
        (d.checks || []).map((c) => h("div", { class: "listrow" }, h("span", { class: "wrap" }, h("strong", { class: "mono" }, c.target), c.error ? h("small", null, c.error) : null),
          badge(c.ok ? t("st.ok") : t("st.bad"), c.ok ? "green" : "red"))));
    } catch (e) {
      if (isLive()) { put(hOut, notice(e.message, "amber")); if (e.status === 401 || e.status === 403) handleError(e); }
    } finally { hRun.disabled = false; }
  });
  const healthCard = card(t("blob.healthTitle"), t("blob.healthSub"), hOut, hRun);

  // convert a file or S3 store into a group with itself as the first member
  let convert = null;
  if (!isGroup && !s.memberOf) {
    convert = button(t("blob.convert"));
    convert.addEventListener("click", () => {
      const nm = textIn(s.name + "-member", { maxlength: 64, autofocus: true });
      const msg = h("div", { class: "form-msg", role: "alert" });
      const go = h("button", { type: "submit", class: "btn primary", form: "conv-f" }, t("blob.convert"));
      const cancel = button(t("common.cancel"));
      const form = h("form", { id: "conv-f", novalidate: true }, h("p", null, t("blob.convertText", { name: s.name })),
        field(t("blob.newMember"), nm, t("blob.newMemberHelp")), msg);
      const m = modal(t("blob.convertTitle"), form, [cancel, go], { cls: "modal-small" });
      cancel.addEventListener("click", m.close);
      form.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        if (!nm.value.trim()) { msg.textContent = t("blob.e.newMember"); return; }
        go.disabled = true;
        try {
          await api("blobstores/" + enc(s.name) + "/convert-to-group", { method: "POST", body: { newMemberName: nm.value.trim() } });
          m.close();
          if (isLive()) fs.done(storeLink(s.name), t("blob.converted", { name: s.name, member: nm.value.trim() }));
        } catch (e) {
          go.disabled = false;
          if (e.status === 401) { m.close(); handleError(e); return; }
          msg.textContent = errList(e).join(" ");
        }
      });
    });
  }

  put(view, pageHead(s.name, h("p", null, storeFlags(s), storeFlags(s).length ? " " : null, t("blob.detailSub")), acts(back, convert, del), { eyebrow: t("blob.eyebrow") }),
    fs.ok,
    h("div", { class: "two" }, h("div", { class: "stack" }, settings, azCard, groupCard, using), h("div", { class: "stack" }, usage, quotaCard, healthCard, notice(t("blob.note")))));
}

// ---- general settings ------------------------------------------------------------

const intOf = (el) => (/^\s*\d+\s*$/.test(el.value) ? Number(el.value) : NaN);
const numIn = (value, min, max) => h("input", { type: "number", inputmode: "numeric", min, max, step: 1, value: String(value) });
const checkRow = (box, label, help) => h("div", { class: "field full" }, h("label", { class: "check" }, box, label), help ? h("small", null, help) : null);

// viewGeneral renders one part of the settings ("general", "anonymous", "http",
// "email" or "usertoken"); a save sends only that part's changed fields, and
// PUT /-/api/settings keeps every field the body leaves out.
export async function viewGeneral(view, part) {
  const isLive = alive();
  const on = (p) => part === p;
  const [g0, sm0, repos, rl] = await Promise.all([api("settings"), on("email") ? api("settings/smtp") : {},
    on("general") ? loadRepos(true) : [], on("general") ? api("roles").catch(() => null) : null]);
  if (!isLive()) return;
  let g = Object.assign({}, g0), sm = Object.assign({}, sm0); // what the server holds now
  const hp = () => Object.assign({ host: "", port: 0, username: "", passwordSet: false, nonProxyHosts: [] }, (g.http || {}).proxy || {});

  const form = h("form", { novalidate: true });
  const fs = formState(form);

  // server and access, session, upload
  const baseURL = textIn(g.baseURL, { type: "url", placeholder: "https://repo.example.com", maxlength: 300 });
  const anon = h("input", { type: "checkbox", checked: !!g.anonymousRead });
  const authRead = h("input", { type: "checkbox", checked: !!g.authenticatedRead });
  const lang = sel([["ko", "한국어"], ["en", "English"]], g.defaultLanguage === "en" ? "en" : "ko");
  const idle = numIn(g.sessionIdleMinutes, 5, 10080);
  const maxUp = numIn(g.maxUploadMiB, 1, 1048576);
  // Terraform: the repository the host root's discovery document points at; "" is the first by name
  const tfNames = repos.filter((r) => r.format === "terraform").map((r) => r.name).sort();
  if (g.terraformDefaultRepository && !tfNames.includes(g.terraformDefaultRepository)) tfNames.unshift(g.terraformDefaultRepository);
  const tfRepo = tfNames.length ? sel([["", t("gen.tfFirst")]].concat(tfNames.map((n) => [n, n])), g.terraformDefaultRepository || "") : null;

  // default roles (every signed-in user) and user tokens
  const roleIds = rl ? (rl.roles || []).map((r) => r.id).filter((id) => id !== "admin") : null;
  for (const id of g.defaultRoles || []) if (roleIds && !roleIds.includes(id)) roleIds.push(id);
  const defRoles = roleIds ? checks(roleIds.map((id) => [id, id]), g.defaultRoles || []) : null;
  const ut0 = () => Object.assign({ enabled: false, required: false, expirationDays: 0 }, g.userTokens || {});
  const utOn = h("input", { type: "checkbox", checked: !!ut0().enabled });
  const utReq = h("input", { type: "checkbox", checked: !!ut0().required });
  const utDays = numIn(ut0().expirationDays || 0, 0, 3650);
  const utBody = () => ({ enabled: utOn.checked, required: utReq.checked, expirationDays: intOf(utDays) });
  const utDirty = () => { const a = utBody(), b = ut0(); return a.enabled !== !!b.enabled || a.required !== !!b.required || a.expirationDays !== (b.expirationDays || 0); };
  const rolesDirty = () => !!defRoles && defRoles.values().join(",") !== (g.defaultRoles || []).join(",");

  // e-mail
  const enabled = h("input", { type: "checkbox", checked: !!sm.enabled });
  const host = textIn(sm.host, { maxlength: 255, placeholder: "smtp.example.com" });
  const port = h("input", { type: "number", inputmode: "numeric", min: 1, max: 65535, step: 1, value: sm.port ? String(sm.port) : "" });
  const secure = sel([["none", t("smtp.secNone")], ["starttls", "STARTTLS"], ["tls", "TLS"]], sm.security || "none");
  const user = textIn(sm.username, { maxlength: 255, autocomplete: "off" });
  const pw = h("input", { type: "password", autocomplete: "new-password", placeholder: sm.passwordSet ? t("rf.unchanged") : "" });
  const clearBox = h("input", { type: "checkbox" });
  const clearRow = h("div", { class: "field" }, h("label", { class: "check" }, clearBox, t("smtp.clearPw")));
  clearRow.hidden = !sm.passwordSet;
  clearBox.addEventListener("change", () => { pw.disabled = clearBox.checked; if (clearBox.checked) pw.value = ""; });
  const sender = textIn(sm.from, { maxlength: 255, placeholder: "Command Center <noreply@example.com>" });
  const pwSet = h("span", null);
  const pwErr = errBox();
  const pwField = field(t("smtp.password"), pw, pwSet);
  pwField.append(pwErr);
  const syncSet = () => put(pwSet, sm.passwordSet ? [h("span", { class: "rf-set" }, t("rf.passSet")), " — ", t("rf.keepEmpty")] : null);
  syncSet();

  const smtpDirty = () => enabled.checked !== !!sm.enabled || host.value.trim() !== (sm.host || "") || (Number(port.value) || 0) !== (sm.port || 0) ||
    secure.value !== (sm.security || "none") || user.value !== (sm.username || "") || sender.value.trim() !== (sm.from || "") ||
    pw.value !== "" || clearBox.checked;

  // test message: uses the saved configuration
  const to = textIn("", { type: "email", placeholder: "you@example.com", maxlength: 254, "aria-label": t("smtp.testTo") });
  const testBtn = button(t("smtp.test"), "small");
  const result = h("div", { "aria-live": "polite" });
  const saveHint = notice(t("smtp.saveFirst"), "amber");
  saveHint.hidden = true;
  const syncHint = () => { saveHint.hidden = !smtpDirty(); };
  testBtn.addEventListener("click", async () => {
    const addr = to.value.trim();
    if (!addr) { put(result, badge(t("smtp.e.to"), "red")); return; }
    testBtn.disabled = true;
    put(result, loading());
    try {
      await api("settings/smtp/test", { method: "POST", body: { to: addr } });
      if (isLive()) put(result, badge(t("smtp.testOk", { to: addr }), "green"));
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); put(result); return; }
      put(result, badge(e.status === WAIT ? t("smtp.testWait") : e.message, "red"));
    } finally {
      testBtn.disabled = false;
    }
  });

  // outbound HTTP (plan: proxy, timeouts, retries, user agent); the whole object is sent when any of it changed
  const hc = g.http || {};
  const ua = textIn(hc.userAgentSuffix, { maxlength: 100, placeholder: "(acme-ci)" });
  const connT = numIn(hc.connectTimeoutSeconds ?? 10, 1, 600);
  const respT = numIn(hc.timeoutSeconds ?? 30, 1, 600);
  const retries = numIn(hc.retries ?? 0, 0, 10);
  const pxHost = textIn(hp().host, { maxlength: 253, placeholder: "proxy.corp.local" });
  const pxPort = h("input", { type: "number", inputmode: "numeric", min: 1, max: 65535, step: 1, value: hp().port ? String(hp().port) : "" });
  const pxUser = textIn(hp().username, { maxlength: 255, autocomplete: "off" });
  const pxPw = h("input", { type: "password", autocomplete: "new-password", placeholder: hp().passwordSet ? t("rf.unchanged") : "" });
  const pxClear = h("input", { type: "checkbox" });
  const pxClearRow = h("div", { class: "field" }, h("label", { class: "check" }, pxClear, t("http.clearPw")));
  pxClearRow.hidden = !hp().passwordSet;
  pxClear.addEventListener("change", () => { pxPw.disabled = pxClear.checked; if (pxClear.checked) pxPw.value = ""; });
  const pxPwSet = h("span", null);
  const syncPxSet = () => put(pxPwSet, hp().passwordSet ? [h("span", { class: "rf-set" }, t("rf.passSet")), " — ", t("rf.keepEmpty")] : t("http.pwHelp"));
  syncPxSet();
  const noProxy = h("textarea", { rows: 3, spellcheck: "false", autocomplete: "off", class: "mono", placeholder: "mirror.corp.local\n*.corp.local\n10.*" }, hp().nonProxyHosts.join("\n"));
  const hostsOf = () => noProxy.value.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
  const httpBody = () => ({ userAgentSuffix: ua.value.trim(), connectTimeoutSeconds: intOf(connT), timeoutSeconds: intOf(respT), retries: intOf(retries),
    proxy: { host: pxHost.value.trim(), port: Number(pxPort.value) || 0, username: pxUser.value.trim(), password: pxPw.value, clearPassword: pxClear.checked, nonProxyHosts: hostsOf() } });
  const httpDirty = () => {
    const b = httpBody(), p = hp();
    return b.userAgentSuffix !== (hc.userAgentSuffix || "") || b.connectTimeoutSeconds !== hc.connectTimeoutSeconds || b.timeoutSeconds !== hc.timeoutSeconds ||
      b.retries !== hc.retries || b.proxy.host !== (p.host || "") || b.proxy.port !== (p.port || 0) || b.proxy.username !== (p.username || "") ||
      b.proxy.password !== "" || b.proxy.clearPassword || b.proxy.nonProxyHosts.join("\n") !== p.nonProxyHosts.join("\n");
  };
  const httpBox = fields(
    field(t("http.ua"), ua, t("http.uaHelp"), { full: true }),
    field(t("http.connT"), connT, t("http.connTHelp")), field(t("http.respT"), respT, t("http.respTHelp")),
    field(t("http.retries"), retries, t("http.retriesHelp")),
    h("div", { class: "field full" }, h("h3", { class: "sub-title" }, t("http.proxy")), h("small", null, t("http.proxyHelp"))),
    field(t("http.pxHost"), pxHost, t("http.pxHostHelp")), field(t("http.pxPort"), pxPort),
    field(t("http.pxUser"), pxUser), field(t("http.pxPw"), pxPw, pxPwSet), pxClearRow,
    field(t("http.noProxy"), noProxy, t("http.noProxyHelp"), { full: true }));

  const save = h("button", { type: "button", class: "btn primary" }, t("gen.save"));
  const mailBox = h("div", null, fields(
    checkRow(enabled, t("smtp.enabled"), t("smtp.enabledHelp")),
    field(t("smtp.host"), host), field(t("smtp.port"), port),
    field(t("smtp.security"), secure), field(t("smtp.username"), user),
    pwField, clearRow,
    field(t("smtp.sender"), sender, t("smtp.senderHelp"), { full: true })),
    saveHint,
    h("div", { class: "row" }, h("div", { class: "field grow" }, h("span", { class: "flabel" }, t("smtp.testTitle")), to), testBtn),
    result);
  mailBox.addEventListener("input", syncHint);
  mailBox.addEventListener("change", syncHint);
  // the test address is not a setting: it must not mark the page busy
  to.addEventListener("input", (ev) => ev.stopPropagation());

  async function run() {
    fs.clear();
    fs.ok.hidden = true;
    pwErr.hidden = true;
    const errs = [];
    const idleN = intOf(idle), upN = intOf(maxUp);
    if (on("general")) {
      if (isNaN(idleN) || idleN < 5 || idleN > 10080) errs.push(t("gen.e.idle"));
      if (isNaN(upN) || upN < 1 || upN > 1048576) errs.push(t("gen.e.upload"));
      if (!baseURL.value.trim()) errs.push(t("gen.e.base"));
    }
    const ut = utBody();
    if (on("usertoken") && !(ut.expirationDays >= 0 && ut.expirationDays <= 3650)) errs.push(t("gen.e.utDays"));
    const hb = httpBody(), httpChanged = on("http") && httpDirty();
    if (httpChanged) {
      if (!(hb.connectTimeoutSeconds >= 1 && hb.connectTimeoutSeconds <= 600) || !(hb.timeoutSeconds >= 1 && hb.timeoutSeconds <= 600)) errs.push(t("http.e.timeout"));
      if (!(hb.retries >= 0 && hb.retries <= 10)) errs.push(t("http.e.retries"));
      if (hb.proxy.host && !(hb.proxy.port >= 1 && hb.proxy.port <= 65535)) errs.push(t("http.e.port"));
    }
    pxPw.value = ""; // the secret never stays in the page
    const body = {};
    if (!errs.length && on("general")) {
      if (baseURL.value.trim().replace(/\/+$/, "") !== g.baseURL) body.baseURL = baseURL.value.trim();
      if (lang.value !== g.defaultLanguage) body.defaultLanguage = lang.value;
      if (idleN !== g.sessionIdleMinutes) body.sessionIdleMinutes = idleN;
      if (upN !== g.maxUploadMiB) body.maxUploadMiB = upN;
      if (tfRepo && tfRepo.value !== (g.terraformDefaultRepository || "")) body.terraformDefaultRepository = tfRepo.value;
      if (rolesDirty()) body.defaultRoles = defRoles.values();
    }
    if (!errs.length && on("anonymous")) {
      if (anon.checked !== !!g.anonymousRead) body.anonymousRead = anon.checked;
      if (authRead.checked !== !!g.authenticatedRead) body.authenticatedRead = authRead.checked;
    }
    if (!errs.length && httpChanged) body.http = hb;
    if (!errs.length && on("usertoken") && utDirty()) body.userTokens = ut;
    const mailChanged = on("email") && smtpDirty();
    const mail = { host: host.value.trim(), port: Number(port.value) || 0, security: secure.value, username: user.value, from: sender.value.trim(),
      enabled: enabled.checked, password: pw.value, clearPassword: clearBox.checked };
    pw.value = ""; // the secret never stays in the page
    if (errs.length) { fs.show(errs, t("gen.errOne")); return; }
    if (!Object.keys(body).length && !mailChanged) { put(fs.ok, t("gen.nothing")); fs.ok.hidden = false; fs.clean(); return; }
    save.disabled = true;
    const saved = [], failed = [];
    let reenter = false;
    if (Object.keys(body).length) {
      try {
        const out = await api("settings", { method: "PUT", body });
        delete body.http; // never keep the proxy password around
        g = Object.assign(g, body, out && typeof out === "object" && !Array.isArray(out) ? out : {});
        if (out && out.http) {
          Object.assign(hc, out.http);
          pxClear.checked = false;
          pxPw.disabled = false;
          pxPw.placeholder = hp().passwordSet ? t("rf.unchanged") : "";
          pxClearRow.hidden = !hp().passwordSet;
          syncPxSet();
        }
        saved.push(t("gen.savedGeneral"));
      } catch (e) {
        if (e.status === 401 || e.status === 403) { save.disabled = false; handleError(e); return; }
        failed.push(...errList(e));
      }
    }
    if (mailChanged) {
      try {
        const out = await api("settings/smtp", { method: "PUT", body: mail });
        sm = Object.assign({}, mail, out || {});
        delete sm.password;
        delete sm.clearPassword;
        saved.push(t("gen.savedMail"));
      } catch (e) {
        if (e.status === 401 || e.status === 403) { save.disabled = false; handleError(e); return; }
        if (e.status === 400 && REENTER.test(e.message)) reenter = true;
        else failed.push(...errList(e));
      }
    }
    save.disabled = false;
    if (!isLive()) return;
    if (saved.includes(t("gen.savedMail"))) {
      clearBox.checked = false;
      pw.disabled = false;
      pw.placeholder = sm.passwordSet ? t("rf.unchanged") : "";
      clearRow.hidden = !sm.passwordSet;
      syncSet();
    }
    if (reenter) { put(pwErr, t("smtp.reenter")); pwErr.hidden = false; pw.focus(); }
    if (failed.length) fs.show(failed, t("gen.errOne"));
    if (saved.length) { put(fs.ok, t("gen.saved")); fs.ok.hidden = false; } // one screen saves one part
    syncHint();
    if (!failed.length && !reenter) fs.clean();
  }
  save.addEventListener("click", run);
  form.addEventListener("submit", (ev) => { ev.preventDefault(); run(); });

  const cards = {
    general: [
      card(t("gen.access"), t("gen.accessSub"), fields(
        field(t("gen.baseURL"), baseURL, t("gen.baseURLHelp"), { full: true }),
        field(t("gen.lang"), lang, t("gen.langHelp")))),
      card(t("gen.session"), t("gen.sessionSub"), fields(field(t("gen.idle"), idle, t("gen.idleHelp")))),
      card(t("gen.upload"), t("gen.uploadSub"), fields(field(t("gen.maxUpload"), maxUp, t("gen.maxUploadHelp")))),
      defRoles ? card(t("gen.defRoles"), t("gen.defRolesSub"), fields(h("div", { class: "field full" }, roleIds.length ? defRoles.el : h("p", { class: "hint" }, t("gen.defRolesNone")), h("small", null, t("gen.defRolesHelp"))))) : null,
      tfRepo ? card("Terraform", t("gen.tfSub"), fields(field(t("gen.tfRepo"), tfRepo, t("gen.tfRepoHelp"), { full: true }))) : null],
    anonymous: [card(t("anon.title"), null, fields(
      checkRow(anon, t("gen.anon"), t("gen.anonHelp")),
      checkRow(authRead, t("gen.authRead"), t("gen.authReadHelp"))))],
    usertoken: [card(t("ut.settings"), t("gen.utSub"), fields(
      checkRow(utOn, t("gen.utEnabled"), t("gen.utEnabledHelp")),
      checkRow(utReq, t("gen.utRequired"), t("gen.utRequiredHelp")),
      field(t("gen.utDays"), utDays, t("gen.utDaysHelp"))))],
    http: [card(t("http.title"), null, httpBox)],
    email: [card(t("smtp.title"), null, mailBox)],
  }[part];
  put(form, fs.err, h("div", { class: "stack" }, cards), h("div", { class: "savebar" }, save));
  if (on("usertoken")) { put(view, fs.ok, form); return; } // a part of the user tokens screen

  const head = { general: ["nav.general", "gen.sub"], anonymous: ["nav.anonymous", "anon.sub"], http: ["nav.http", "http.sub"], email: ["nav.email", "smtp.sub"] }[part];
  const help = h("aside", { class: "form-help" }, h("h3", null, t("gen.helpTitle")), h("p", null, t(on("http") ? "gen.helpHttp" : on("email") ? "gen.helpMail" : "gen.helpLive")));
  put(view,
    pageHead(t(head[0]), sub(t(head[1])), null, { eyebrow: t("gen.eyebrow") }),
    fs.ok,
    h("div", { class: "form-layout" }, form, help));
}

// ---- vulnerability data (OSV) and the firewall quarantine ---------------------------
//   #admin/firewall/settings
//   #admin/firewall/quarantine

const ECOSYSTEMS = ["npm", "PyPI", "Maven", "Go", "crates.io", "NuGet", "RubyGems", "Packagist", "ConanCenter", "CRAN"]; // OSV's names

// upload POSTs a file as the raw request body with upload progress (fetch has
// none) and resolves with the JSON answer, or rejects with its error.
function upload(path, file, onProgress) {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    x.open("POST", "/-/api/" + path);
    x.setRequestHeader("X-Command-Center-UI", "1");
    x.setRequestHeader("Content-Type", file.type || "application/zip");
    x.upload.addEventListener("progress", (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); });
    x.upload.addEventListener("load", () => onProgress(1));
    x.addEventListener("load", () => {
      let d = null;
      try { d = JSON.parse(x.responseText); } catch (e) { /* not JSON */ }
      if (x.status >= 200 && x.status < 300) resolve(d || {});
      else reject(Object.assign(new Error((d && d.error) || x.status + " " + x.statusText), { status: x.status, data: d }));
    });
    x.addEventListener("error", () => reject(new Error(t("err.connect"))));
    x.send(file);
  });
}

export async function viewVulnerability(view) {
  const isLive = alive();
  const eb = { eyebrow: t("sys.eyebrow") };
  let st;
  try { st = await api("vulnerability/settings"); } catch (e) {
    if (e.status !== 501) throw e;
    if (isLive()) put(view, pageHead(t("nav.vulnerability"), null, null, eb), notice(t("vs.unavailable"), "amber"));
    return;
  }
  if (!isLive()) return;

  // status
  const statusBody = h("div", null, loading());
  const refresh = button([icon("refresh"), t("st.refresh")], "small");
  const loadStatus = async () => {
    refresh.disabled = true;
    try {
      const d = await api("vulnerability/status");
      if (!isLive()) return;
      const eco = Object.entries(d.byEcosystem || {}).sort((a, b) => b[1] - a[1]);
      put(statusBody,
        kv(t("col.status"), d.enabled ? badge(t("vs.on", { mode: d.mode === "offline" ? t("vs.modeOffline") : t("vs.modeOnline") }), "green") : badge(t("vs.off"))),
        kv(t("vs.advisories"), fmtNum(d.advisories)),
        kv(t("vul.malicious"), fmtNum(d.malicious)),
        kv(t("vs.lastImport"), d.lastImport ? t("vs.lastImportText", { time: fmtDate(d.lastImport.importedAt), src: d.lastImport.source, n: fmtNum(d.lastImport.advisories) }) : dash),
        d.running ? kv(t("vs.running"), badge(t("task.running"), "blue")) : null,
        kv(t("vs.failures"), fmtNum(d.lookupFailures)),
        d.lastFailure ? kv(t("vs.lastFailure"), h("span", null, fmtDate(d.lastFailure.at), h("div", { class: "tiny muted mono" }, d.lastFailure.message))) : null,
        eco.length ? tbl([t("vs.ecosystem"), { t: t("vs.advisories"), cls: "num" }], eco.map(([k, n]) => h("tr", null, h("td", null, k), h("td", { class: "num" }, fmtNum(n))))) : h("p", { class: "hint" }, t("vs.noData")));
    } catch (e) {
      if (isLive()) { put(statusBody, notice(e.message, "amber")); if (e.status === 401 || e.status === 403) handleError(e); }
    } finally { refresh.disabled = false; }
  };
  refresh.addEventListener("click", loadStatus);

  // settings
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  const on = h("input", { type: "checkbox", checked: !!st.enabled });
  const mode = sel([["online", t("vs.modeOnline")], ["offline", t("vs.modeOffline")]], st.mode || "online");
  const apiURL = textIn(st.apiURL, { type: "url", class: "mono", maxlength: 300 });
  const dumpURL = textIn(st.dumpURL, { type: "url", class: "mono", maxlength: 300 });
  const hours = numIn(st.cacheHours || 24, 1, 720);
  const maxMiB = numIn(st.maxImportMiB || 1024, 1, 16384);
  const eco = checks(ECOSYSTEMS.map((x) => [x, x]), st.ecosystems || []);
  const evHosted = h("input", { type: "checkbox", checked: !!st.evaluateHosted });
  const evMembers = h("input", { type: "checkbox", checked: !!st.evaluateGroupMembers });
  const fApi = field(t("vs.apiURL"), apiURL, t("vs.apiURLHelp"), { full: true });
  const fHours = field(t("vs.cacheHours"), hours, t("vs.cacheHoursHelp"));
  const syncMode = () => { fApi.hidden = fHours.hidden = mode.value !== "online"; };
  mode.addEventListener("change", syncMode);
  syncMode();
  const save = h("button", { type: "submit", class: "btn primary" }, t("gen.save"));
  put(form, fs.err, fields(
    checkRow(on, t("vs.enabled"), t("vs.enabledHelp")),
    field(t("vs.mode"), mode, t("vs.modeHelp"), { full: true }),
    fApi, fHours,
    field(t("vs.dumpURL"), dumpURL, t("vs.dumpURLHelp"), { full: true }),
    field(t("vs.maxImport"), maxMiB, t("vs.maxImportHelp")),
    h("div", { class: "field full", role: "group" }, h("span", { class: "flabel" }, t("vs.ecosystems")), eco.el, h("small", null, t("vs.ecosystemsHelp"))),
    h("h3", { class: "sub-title field full" }, t("vs.evalTitle")),
    checkRow(evHosted, t("vs.evalHosted"), t("vs.evalHostedHelp")),
    checkRow(evMembers, t("vs.evalMembers"), t("vs.evalMembersHelp"))),
    h("div", { class: "actions end" }, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    fs.clear();
    const b = { enabled: on.checked, mode: mode.value, apiURL: apiURL.value.trim(), dumpURL: dumpURL.value.trim(),
      cacheHours: intOf(hours), maxImportMiB: intOf(maxMiB), ecosystems: eco.values(), evaluateHosted: evHosted.checked, evaluateGroupMembers: evMembers.checked };
    const errs = [];
    if (!(b.cacheHours >= 1 && b.cacheHours <= 720)) errs.push(t("vs.e.hours"));
    if (!(b.maxImportMiB >= 1 && b.maxImportMiB <= 16384)) errs.push(t("vs.e.max"));
    if (errs.length) { fs.show(errs); return; }
    save.disabled = true;
    try {
      await api("vulnerability/settings", { method: "PUT", body: b });
      if (!isLive()) return;
      fs.clean();
      put(fs.ok, t("gen.saved"));
      fs.ok.hidden = false;
      loadStatus();
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) handleError(e); else fs.show(errList(e));
    } finally { save.disabled = false; }
  });

  // offline import of an OSV dump (all.zip or an ecosystem's zip)
  const file = h("input", { type: "file", accept: ".zip,application/zip" });
  const go = button(t("vs.import"), "primary");
  const prog = h("p", { class: "tiny muted", role: "status" });
  const result = h("div", { "aria-live": "polite" });
  go.addEventListener("click", async () => {
    const f = file.files && file.files[0];
    if (!f) { put(result, notice(t("vs.e.file"), "amber")); return; }
    go.disabled = file.disabled = true;
    put(result);
    prog.textContent = t("vs.uploading", { pct: 0, size: fmtBytes(f.size) });
    try {
      const d = await upload("vulnerability/import", f, (p) => {
        prog.textContent = p < 1 ? t("vs.uploading", { pct: Math.floor(p * 100), size: fmtBytes(f.size) }) : t("vs.processing");
      });
      if (!isLive()) return;
      prog.textContent = "";
      put(result, notice(t("vs.imported", { n: fmtNum(d.advisories), rows: fmtNum(d.rows), skipped: fmtNum(d.skipped) }), "ok"));
      file.value = "";
      loadStatus();
    } catch (e) {
      if (!isLive()) return;
      prog.textContent = "";
      put(result, notice(e.status === 413 ? t("vs.tooBig") : e.status === 409 ? t("vs.busy") : e.message, "amber"));
      if (e.status === 401 || e.status === 403) handleError(e);
    } finally { go.disabled = file.disabled = false; }
  });

  put(view, pageHead(t("nav.vulnerability"), sub(t("vs.sub")), acts(h("a", { class: "btn", href: "#admin/firewall/quarantine" }, t("nav.quarantine"))), eb), fs.ok,
    h("div", { class: "two" },
      h("div", { class: "stack" }, card(t("vs.settings"), t("vs.settingsSub"), form)),
      h("div", { class: "stack" },
        card(t("vs.status"), t("vs.statusSub"), statusBody, refresh),
        card(t("vs.importTitle"), t("vs.importSub"), [field(t("vs.file"), file, t("vs.fileHelp"), { full: true }), h("div", { class: "actions" }, go), prog, result]),
        notice([t("vs.tasks"), " ", h("a", { href: "#admin/system/tasks:create" }, t("rf.vulnScanTask"))]))));
  loadStatus();
}

export async function viewQuarantine(view) {
  const isLive = alive();
  const repos = await loadRepos(true);
  if (!isLive()) return;
  const fw = repos.filter((r) => r.type === "proxy" && OSV_FORMATS.includes(r.format));
  const filter = sel([["", t("q.allRepos")]].concat(fw.map((r) => [r.name, r.name + " · " + (FORMAT_NAMES[r.format] || r.format)])), "", { "aria-label": t("col.repo") });
  const box = h("div", null, loading());
  const ok = h("div", { class: "notice ok", role: "status", hidden: true });
  const load = async () => {
    put(box, loading());
    try {
      const d = await api("firewall/quarantine" + (filter.value ? "?repository=" + enc(filter.value) : ""));
      if (!isLive()) return;
      const items = d.items || [];
      put(box, tbl([t("col.repo"), t("comp.component"), t("q.advisories"), t("vul.severity"), t("q.blocked"), { t: t("q.count"), cls: "num" }, { t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" }],
        items.map((x) => {
          const rel = button(t("q.release"), "small");
          rel.addEventListener("click", async () => {
            if (!(await confirmDelete(t("q.releaseText", { name: x.name, version: x.version, repo: x.repository }), t("q.releaseTitle"), t("q.release")))) return;
            rel.disabled = true;
            try {
              const out = await api("firewall/quarantine/release", { method: "POST", body: { repository: x.repository, name: x.name, version: x.version } });
              if (!isLive()) return;
              put(ok, t("q.released", { name: x.name + "@" + x.version, entry: out.allowListEntry, repo: x.repository }));
              ok.hidden = false;
              load();
            } catch (e) { rel.disabled = false; handleError(e); }
          });
          return h("tr", null,
            h("td", { class: "nowrap" }, h("a", { href: adminRepoLink(x.repository) }, x.repository), h("div", { class: "tiny muted" }, x.ecosystem)),
            h("td", { class: "name" }, h("strong", null, x.name + "@" + x.version), x.path ? h("div", { class: "tiny muted mono" }, x.path) : null),
            h("td", null, (x.ids || []).slice(0, 6).map((id) => h("div", null, osvLink(id))), (x.ids || []).length > 6 ? h("div", { class: "tiny muted" }, "+" + (x.ids.length - 6)) : null),
            h("td", { class: "nowrap" }, x.malicious ? [badge(t("vul.malicious"), "red"), " "] : null, sevBadge(x.severity)),
            h("td", { class: "nowrap" }, fmtDate(x.lastBlocked), h("div", { class: "tiny muted" }, t("q.first", { time: fmtDate(x.firstBlocked) }))),
            h("td", { class: "num" }, fmtNum(x.count)),
            h("td", { class: "act" }, rel));
        }), t("q.none")),
        d.truncated ? h("div", { class: "notice amber" }, t("q.truncated")) : null);
    } catch (e) {
      if (!isLive()) return;
      put(box, h("div", { class: "empty" }, e.message));
      if (e.status !== 404) handleError(e);
    }
  };
  filter.addEventListener("change", load);
  const refresh = button([icon("refresh"), t("st.refresh")], "small");
  refresh.addEventListener("click", load);
  const c = h("section", { class: "card" }, h("div", { class: "toolbar" }, filter, h("span", { class: "grow" }), refresh), box);
  put(view, pageHead(t("nav.quarantine"), sub(t("q.sub")), acts(h("a", { class: "btn", href: "#admin/firewall/settings" }, t("nav.vulnerability"))), { eyebrow: t("task.eyebrow") }),
    ok, c, fw.length ? null : notice(t("q.noFirewall"), "amber"), notice(t("q.note")));
  load();
}
