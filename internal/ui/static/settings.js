// System screens (plan 12 Task 8): blob stores and the general settings with
// the e-mail (SMTP) configuration. The server enforces every rule; these
// screens collect input and show its answers. The SMTP password is cleared
// from its input as soon as it is read.
//   #admin/repository/blobstores[:create|:<name>]
//   #admin/system/general
import {
  h, put, adminRepoLink, api, enc, alive, pageHead, loading, card, kv, bar, tbl, handleError, loadRepos, fmtBytes, fmtNum, typeBadge,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete } from "./browse.js";
import { field, fields, sel, textIn, button, formState, errList } from "./ops.js";

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

const storeType = (s) => s.type === "s3" ? "S3" : t("blob.file");
const storeLocation = (s) => s.type === "s3" ? "s3://" + ((s.s3 || {}).bucket || "") + "/" + ((s.s3 || {}).prefix || "") : s.path;

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
      h("td", null, h("a", { href: storeLink(s.name) }, h("strong", null, s.name))),
      h("td", null, storeType(s)), h("td", null, h("code", null, storeLocation(s))),
      h("td", { class: "num" }, fmtBytes(s.usedBytes || 0), known(s.fsTotal) ? " / " + fmtBytes(s.fsTotal) : null),
      h("td", { class: "num" }, fmtNum(s.blobCount || 0)),
      h("td", { class: "num" }, fmtNum(s.repoCount || 0)))), t("blob.none"))),
    notice(t("blob.note")));
}

function storeCreate(view) {
  const isLive = alive();
  const back = h("a", { class: "btn", href: storeLink() }, t("blob.back"));
  const nameIn = textIn("", { required: true, maxlength: 64 });
  const pathIn = textIn("", { class: "mono", placeholder: "/data/blobstores/archive" });
  const type = sel([["file", t("blob.file")], ["s3", "S3"]], "file");
  const bucket = textIn("", { required: true }), prefix = textIn(""), region = textIn(""), endpoint = textIn("", { type: "url", placeholder: "https://s3.example.com" });
  const accessKey = textIn("", { autocomplete: "off" });
  const secretKey = h("input", { type: "password", autocomplete: "new-password" });
  const pathStyle = h("input", { type: "checkbox" });
  const sse = sel([["", t("blob.sseDefault")], ["AES256", "AES256"], ["aws:kms", "aws:kms"]], "");
  const fileFields = fields(field(t("blob.path"), pathIn, t("blob.pathHelp"), { full: true }));
  const s3Fields = fields(field(t("blob.bucket"), bucket, null, { req: true }), field(t("blob.prefix"), prefix),
    field(t("blob.region"), region), field(t("blob.endpoint"), endpoint),
    field(t("blob.accessKey"), accessKey), field(t("blob.secretKey"), secretKey, t("blob.secretHelp")),
    field(t("blob.pathStyle"), pathStyle), field(t("blob.sse"), sse));
  const syncType = () => {
    fileFields.hidden = type.value !== "file";
    s3Fields.hidden = type.value !== "s3";
    pathIn.disabled = type.value !== "file";
    for (const el of s3Fields.querySelectorAll("input, select")) el.disabled = type.value !== "s3";
    if (type.value !== "s3") secretKey.value = "";
  };
  type.addEventListener("change", syncType);
  syncType();
  const save = h("button", { type: "submit", class: "btn primary" }, t("blob.createBtn"));
  const form = h("form", { novalidate: true });
  const fs = formState(form);
  put(form, fs.err,
    fields(field(t("blob.name"), nameIn, t("blob.nameHelp"), { req: true }), field(t("blob.type"), type)), fileFields, s3Fields,
    h("div", { class: "actions end" }, back, save));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (save.disabled) return;
    fs.clear();
    const body = { name: nameIn.value.trim() };
    if (!body.name) { fs.show([t("blob.e.name")]); return; }
    if (type.value === "s3") {
      if (!bucket.value.trim()) { fs.show([t("blob.e.bucket")]); return; }
      body.type = "s3";
      body.s3 = { bucket: bucket.value.trim(), prefix: prefix.value.trim(), region: region.value.trim(), endpoint: endpoint.value.trim(),
        pathStyle: pathStyle.checked, accessKeyId: accessKey.value.trim(), secretAccessKey: secretKey.value, sse: sse.value };
    } else body.path = pathIn.value.trim(); // retain compatibility with file-only servers
    secretKey.value = "";
    save.disabled = true;
    try {
      const out = await api("blobstores", { method: "POST", body });
      if (isLive()) fs.done(storeLink(out.name), t("blob.created"));
    } catch (e) {
      save.disabled = false;
      if (!isLive()) return;
      if (e.status === 401 || e.status === 403) { handleError(e); return; }
      const secret = body.s3 && body.s3.secretAccessKey;
      fs.show(errList(e).map((msg) => secret ? String(msg).split(secret).join("••••") : msg));
    } finally {
      if (body.s3) delete body.s3.secretAccessKey;
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
  const repos = await loadRepos(true);
  if (!isLive()) return;
  const users = repos.filter((r) => (r.blobStore || "default") === s.name);
  const fs = formState(h("form"));
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
        put(err, n ? t("blob.inUse", { n: fmtNum(n) }) : t("blob.defaultFixed"));
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
    kv(t("blob.path"), h("span", { class: "mono" }, storeLocation(s))),
    s.type === "s3" ? [
      kv(t("blob.bucket"), (s.s3 || {}).bucket), kv(t("blob.prefix"), (s.s3 || {}).prefix),
      kv(t("blob.region"), (s.s3 || {}).region), kv(t("blob.endpoint"), (s.s3 || {}).endpoint),
      kv(t("blob.pathStyle"), String(!!(s.s3 || {}).pathStyle)), kv(t("blob.sse"), (s.s3 || {}).sse),
      kv(t("blob.accessKey"), (s.s3 || {}).accessKeyId || (s.s3 || {}).accessKey),
      kv(t("blob.secretKey"), typeof ((s.s3 || {}).secretSet ?? s.secretSet) === "boolean"
        ? (((s.s3 || {}).secretSet ?? s.secretSet) ? t("blob.secretSet") : t("blob.secretUnset")) : dash),
    ] : null,
    h("p", { class: "hint" }, t("blob.pathFixed")),
    s.warning ? notice(t("blob.outside"), "amber") : null]);
  const usage = card(t("blob.usage"), t("blob.usageSub"), [
    kv(t("blob.used"), fmtBytes(s.usedBytes || 0)),
    kv(t("blob.count"), fmtNum(s.blobCount || 0)),
    kv(t("blob.repos"), fmtNum(s.repoCount || 0)),
    kv(t("blob.free"), known(s.fsTotal) && s.fsFree != null ? fmtBytes(s.fsFree) : t("blob.capUnknown")),
    known(s.fsTotal) ? bar(p, p > 70 ? "amber" : null) : null]);
  const using = card(t("blob.usedBy"), t("blob.usedBySub"), users.length ? tbl([t("col.repo"), t("col.type")],
    users.map((r) => h("tr", null, h("td", { class: "name" }, h("a", { href: adminRepoLink(r.name) }, h("strong", null, r.name))), h("td", null, typeBadge(r.type)))))
    : h("div", { class: "empty" }, t("blob.noRepos")));
  put(view, pageHead(s.name, sub(t("blob.detailSub")), acts(back, del), { eyebrow: t("blob.eyebrow") }),
    h("div", { class: "two" }, h("div", { class: "stack" }, settings, using), h("div", { class: "stack" }, usage, notice(t("blob.note")))));
}

// ---- general settings ------------------------------------------------------------

const intOf = (el) => (/^\s*\d+\s*$/.test(el.value) ? Number(el.value) : NaN);
const numIn = (value, min, max) => h("input", { type: "number", inputmode: "numeric", min, max, step: 1, value: String(value) });
const checkRow = (box, label, help) => h("div", { class: "field full" }, h("label", { class: "check" }, box, label), help ? h("small", null, help) : null);

export async function viewGeneral(view) {
  const isLive = alive();
  const [g0, sm0] = await Promise.all([api("settings"), api("settings/smtp")]);
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
    if (isNaN(idleN) || idleN < 5 || idleN > 10080) errs.push(t("gen.e.idle"));
    if (isNaN(upN) || upN < 1 || upN > 1048576) errs.push(t("gen.e.upload"));
    if (!baseURL.value.trim()) errs.push(t("gen.e.base"));
    const hb = httpBody(), httpChanged = httpDirty();
    if (httpChanged) {
      if (!(hb.connectTimeoutSeconds >= 1 && hb.connectTimeoutSeconds <= 600) || !(hb.timeoutSeconds >= 1 && hb.timeoutSeconds <= 600)) errs.push(t("http.e.timeout"));
      if (!(hb.retries >= 0 && hb.retries <= 10)) errs.push(t("http.e.retries"));
      if (hb.proxy.host && !(hb.proxy.port >= 1 && hb.proxy.port <= 65535)) errs.push(t("http.e.port"));
    }
    pxPw.value = ""; // the secret never stays in the page
    const body = {};
    if (!errs.length) {
      if (baseURL.value.trim().replace(/\/+$/, "") !== g.baseURL) body.baseURL = baseURL.value.trim();
      if (anon.checked !== !!g.anonymousRead) body.anonymousRead = anon.checked;
      if (authRead.checked !== !!g.authenticatedRead) body.authenticatedRead = authRead.checked;
      if (lang.value !== g.defaultLanguage) body.defaultLanguage = lang.value;
      if (idleN !== g.sessionIdleMinutes) body.sessionIdleMinutes = idleN;
      if (upN !== g.maxUploadMiB) body.maxUploadMiB = upN;
      if (httpChanged) body.http = hb;
    }
    const mailChanged = smtpDirty();
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
    if (saved.length) { put(fs.ok, t("gen.saved") + " " + saved.join(" · ")); fs.ok.hidden = false; }
    syncHint();
    if (!failed.length && !reenter) fs.clean();
  }
  save.addEventListener("click", run);
  form.addEventListener("submit", (ev) => { ev.preventDefault(); run(); });

  put(form, fs.err, h("div", { class: "stack" },
    card(t("gen.access"), t("gen.accessSub"), fields(
      field(t("gen.baseURL"), baseURL, t("gen.baseURLHelp"), { full: true }),
      checkRow(anon, t("gen.anon"), t("gen.anonHelp")),
      checkRow(authRead, t("gen.authRead"), t("gen.authReadHelp")),
      field(t("gen.lang"), lang, t("gen.langHelp")))),
    card(t("gen.session"), t("gen.sessionSub"), fields(field(t("gen.idle"), idle, t("gen.idleHelp")))),
    card(t("gen.upload"), t("gen.uploadSub"), fields(field(t("gen.maxUpload"), maxUp, t("gen.maxUploadHelp")))),
    card(t("http.title"), t("http.sub"), httpBox),
    card(t("smtp.title"), t("smtp.sub"), mailBox)), h("div", { class: "savebar" }, save));

  const help = h("aside", { class: "form-help" }, h("h3", null, t("gen.helpTitle")), h("p", null, t("gen.helpLive")), h("p", null, t("gen.helpHttp")), h("p", null, t("gen.helpMail")));
  put(view,
    pageHead(t("nav.general"), sub(t("gen.sub")), null, { eyebrow: t("gen.eyebrow") }),
    fs.ok,
    h("div", { class: "form-layout" }, form, help));
}
