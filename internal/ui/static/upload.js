// Upload (plan 9 Decision 8) as the concept's three-step stepper (plan 10
// Decision 8): target and files, metadata, review and upload.
//   #browse/upload          step 1 with only the repository choice
//   #browse/upload/<repo>   the full stepper for that repository's format
// The browser builds the requests the real clients send (a Maven PUT per
// file, an npm publish document, a twine multipart POST) and sends them to
// the repository endpoints with the session cookie and X-Command-Center-UI. All
// validation stays on the server; its message is shown inline.
import {
  state, h, put, api, apiRepo, enc, encPath, alive, browseLink, uploadLink, canUpload, repoBase, fmtBadge, typeBadge,
  fmtBytes, FORMAT_NAMES, pageHead, needLogin, card, kv, icon, markBusy,
} from "./util.js";
import { t } from "./i18n.js";
import { rerender } from "./shell.js";

let uidN = 0;
const uid = (p) => "up-" + p + "-" + (++uidN);

function field(label, control, help, full) {
  if (help) control.setAttribute("aria-describedby", control.id + "-help");
  return h("div", { class: "field" + (full ? " full" : "") }, h("label", { for: control.id }, label), control,
    help ? h("small", { id: control.id + "-help" }, help) : null);
}
const input = (attrs) => h("input", Object.assign({ id: uid("t"), type: "text", autocomplete: "off", spellcheck: "false" }, attrs));
const fileInput = (accept) => h("input", { id: uid("f"), type: "file", accept });
const head = (sub) => pageHead(t("nav.upload"), sub || h("p", null, t("up.sub")), null, { eyebrow: t("up.eyebrow") });

function stepper() {
  const steps = [t("up.step1"), t("up.step2"), t("up.step3")].map((label, i) =>
    h("span", { class: "step" }, h("i", null, String(i + 1)), h("span", { class: "step-label" }, label)));
  const el = h("div", { class: "stepper", role: "list", "aria-label": t("up.steps") },
    steps.flatMap((s, i) => [h("span", { role: "listitem", class: "step-wrap" }, s), i < 2 ? h("span", { class: "step-line", "aria-hidden": "true" }) : null]));
  const set = (n) => steps.forEach((s, i) => {
    s.classList.toggle("active", i === n);
    s.classList.toggle("done", i < n);
    if (i === n) s.setAttribute("aria-current", "step"); else s.removeAttribute("aria-current");
  });
  set(0);
  return { el, set };
}

// helpAside is the concept's form help, with the chosen format highlighted.
function helpAside(format) {
  const text = { maven: t("up.helpMaven"), npm: t("up.helpNpm"), pypi: t("up.helpPypi") };
  return h("aside", { class: "form-help" }, h("h3", null, t("up.helpTitle")),
    ["maven", "npm", "pypi"].map((f) => h("p", { class: f === format ? "on" : null }, h("b", null, FORMAT_NAMES[f]), h("br"), text[f])),
    h("p", null, t("up.helpClients")));
}

// targetSelect lists the repositories the user can upload to; choosing one
// opens its upload stepper.
function targetSelect(repos, current) {
  const sel = h("select", { id: uid("target") },
    current ? null : h("option", { value: "" }, t("up.pick")),
    repos.map((r) => h("option", { value: r.name, selected: r.name === current }, r.name + " · " + (FORMAT_NAMES[r.format] || r.format))));
  sel.addEventListener("change", () => { if (sel.value) location.hash = uploadLink(sel.value); });
  return field(t("up.target"), sel, null, true);
}

// ---- step 1 without a repository --------------------------------------------------

export async function viewUploadList(view) {
  if (state.me.anonymous) return needLogin(view, t("nav.upload"));
  const isLive = alive();
  const d = await api("repositories");
  if (!isLive()) return;
  state.repos = d.repositories || [];
  const repos = state.repos.filter(canUpload);
  const st = stepper();
  put(view, head(), st.el, h("div", { class: "form-layout" },
    card(t("up.card1"), t("up.card1Sub"), repos.length
      ? [h("div", { class: "fields" }, targetSelect(repos, "")), h("div", { class: "dropzone" }, h("strong", null, t("up.dropTitle")), h("p", null, t("up.pickFirst")))]
      : h("div", { class: "empty" }, t("up.none"))),
    helpAside("")));
}

// ---- requests -------------------------------------------------------------------

// errText is the server's message: text/plain, or {"error": …} from npm.
function errText(x) {
  let s = String(x.responseText || "").trim();
  try { const d = JSON.parse(s); if (d && d.error) s = String(d.error); } catch (e) { /* plain text */ }
  if (x.status === 401) s = t("up.login401", { msg: s || "401" });
  return x.status + " " + (s.slice(0, 500) || x.statusText);
}

// sender returns send(method, url, body, type, onProgress): one XHR with the
// UI header. Leaving the route aborts the request in flight.
function sender() {
  let cur = null;
  state.leaving.push(() => { if (cur) cur.abort(); });
  return (method, url, body, type, onProg) => new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    cur = x;
    x.open(method, url);
    x.setRequestHeader("X-Command-Center-UI", "1");
    if (type) x.setRequestHeader("Content-Type", type);
    x.upload.addEventListener("progress", (e) => { if (e.lengthComputable) onProg(e.loaded / e.total); });
    x.addEventListener("load", () => { cur = null; if (x.status >= 200 && x.status < 300) resolve(); else reject(new Error(errText(x))); });
    x.addEventListener("error", () => { cur = null; reject(new Error(t("err.connect"))); });
    x.addEventListener("abort", () => { cur = null; reject(new Error(t("up.cancelled"))); });
    x.send(body);
  });
}

// ---- Maven ----------------------------------------------------------------------

const XML_ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&apos;" };
const xml = (v) => String(v).replace(/[&<>"']/g, (ch) => XML_ESC[ch]);

function genPom(g, a, v, p) {
  return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
    "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n  <modelVersion>4.0.0</modelVersion>\n" +
    "  <groupId>" + xml(g) + "</groupId>\n  <artifactId>" + xml(a) + "</artifactId>\n" +
    "  <version>" + xml(v) + "</version>\n  <packaging>" + xml(p) + "</packaging>\n</project>\n";
}

// pomCoords reads the coordinates from POM text. The XML is parsed only to
// read text values; no parsed node ever enters the page.
function pomCoords(text) {
  const doc = new DOMParser().parseFromString(text, "application/xml");
  const root = doc.documentElement;
  if (!root || root.localName !== "project" || doc.getElementsByTagName("parsererror").length) throw new Error(t("up.e.pomBad"));
  const kid = (el, n) => {
    for (const c of el ? el.children : []) if (c.localName === n) return c.textContent.trim();
    return "";
  };
  const par = [...root.children].find((c) => c.localName === "parent");
  const c = {
    g: kid(root, "groupId") || kid(par, "groupId"), a: kid(root, "artifactId"),
    v: kid(root, "version") || kid(par, "version"), p: kid(root, "packaging"),
  };
  if (!c.g || !c.a || !c.v) throw new Error(t("up.e.pomCoords"));
  return c;
}

const extOf = (n) => { const m = /\.(tar\.(?:gz|bz2|xz)|[^.]+)$/.exec(n); return m ? m[1] : ""; };
const GROUP_RE = /^[\w-]+(\.[\w-]+)*$/, ID_RE = /^[\w~+-][\w.~+-]*$/, CLS_RE = /^[\w-]+$/, EXT_RE = /^[A-Za-z][\w.-]*$/;

function mavenForm(r, ui) {
  const rowsEl = h("div", { class: "up-files" });
  const rows = [];
  const g = input({ maxlength: 200 }), a = input({ maxlength: 200 }), v = input({ maxlength: 100 }), pk = input({ placeholder: "jar", maxlength: 50 });
  const genBox = h("input", { type: "checkbox", id: uid("gen"), checked: true });
  let pomRow = null;
  const lock = (on) => {
    for (const el of [g, a, v, pk]) el.readOnly = on;
    genBox.checked = !on;
    genBox.disabled = on;
  };
  function addRow() {
    const file = fileInput(""), cls = input({ placeholder: t("up.noneCls") }), ext = input({ class: "up-ext" });
    const rm = h("button", { type: "button", class: "btn small" }, t("up.remove"));
    const cell = (label, el) => h("label", { class: "up-cell" }, h("span", null, label), el);
    const it = { file, cls, ext, el: h("div", { class: "up-row" }, cell(t("up.file"), file), cell("Classifier", cls), cell("Extension", ext), rm) };
    file.addEventListener("change", async () => {
      if (pomRow === it) { pomRow = null; lock(false); }
      const f = file.files[0];
      if (!f) return;
      ext.value = extOf(f.name);
      if (ext.value.toLowerCase() !== "pom") return;
      try {
        if (f.size > 1 << 20) throw new Error(t("up.e.pomBig"));
        const c = pomCoords(await f.text());
        if (file.files[0] !== f || !rows.includes(it)) return;
        g.value = c.g; a.value = c.a; v.value = c.v; pk.value = c.p;
        pomRow = it;
        lock(true);
        ui.clear();
      } catch (e) {
        ui.fail([f.name + ": " + e.message]);
      }
    });
    rm.addEventListener("click", () => {
      rows.splice(rows.indexOf(it), 1);
      it.el.remove();
      if (pomRow === it) { pomRow = null; lock(false); }
      if (!rows.length) addRow();
    });
    rows.push(it);
    rowsEl.appendChild(it.el);
  }
  addRow();
  const add = h("button", { type: "button", class: "btn small" }, t("up.addFile"));
  add.addEventListener("click", addRow);

  const picked = () => rows.filter((x) => x.file.files[0]).map((x) => ({ f: x.file.files[0], c: x.cls.value.trim(), e: x.ext.value.trim() }));
  // check1 validates step 1: the files, their classifiers and extensions.
  function check1() {
    const errs = [];
    const files = picked();
    if (!files.length) errs.push(t("up.e.noFile"));
    for (const x of files) {
      if (x.c && !CLS_RE.test(x.c)) errs.push(t("up.e.cls", { file: x.f.name }));
      if (!EXT_RE.test(x.e)) errs.push(t("up.e.ext", { file: x.f.name }));
    }
    return errs;
  }

  function plan() {
    const errs = check1();
    const files = picked();
    const gv = g.value.trim(), av = a.value.trim(), vv = v.value.trim();
    if (!GROUP_RE.test(gv)) errs.push(t("up.e.group"));
    if (!ID_RE.test(av)) errs.push(t("up.e.artifact"));
    if (!ID_RE.test(vv)) errs.push(t("up.e.version"));
    if (errs.length) return { errs };
    const dir = gv.replace(/\./g, "/") + "/" + av + "/" + vv;
    const items = files.map((x) => ({ label: x.f.name, path: dir + "/" + av + "-" + vv + (x.c ? "-" + x.c : "") + "." + x.e, body: x.f }));
    const hasPom = files.some((x) => !x.c && x.e.toLowerCase() === "pom");
    if (genBox.checked && !genBox.disabled && !hasPom) {
      const main = files.find((x) => !x.c);
      const p = pk.value.trim() || (main ? main.e : "jar");
      items.push({ label: t("up.pomGenerated"), path: dir + "/" + av + "-" + vv + ".pom", body: new Blob([genPom(gv, av, vv, p)], { type: "application/xml" }) });
    }
    const seen = new Set();
    for (const it of items) {
      if (seen.has(it.path)) errs.push(t("up.e.dup", { name: it.path.slice(dir.length + 1) }));
      seen.add(it.path);
    }
    if (errs.length) return { errs };
    const base = repoBase(r);
    return {
      comp: gv + ":" + av + ":" + vv,
      steps: items.map((it) => ({ label: it.label + " → " + it.path, go: (p) => ui.send("PUT", base + encPath(it.path), it.body, "", p) })),
      link: browseLink(r.name, dir),
    };
  }
  return {
    files: [rowsEl, h("div", { class: "up-add" }, add), h("p", { class: "hint" }, t("up.pomHint"))],
    meta: [h("div", { class: "fields" }, field("Group ID", g), field("Artifact ID", a), field("Version", v), field("Packaging", pk)),
      h("label", { class: "check up-gen" }, genBox, t("up.genPom"))],
    check1, plan,
  };
}

// ---- npm ------------------------------------------------------------------------

// npmName mirrors the server's npm.ValidName (without the 214-character cap):
// the parts of a scoped name may start with "_" but not ".", an unscoped name
// may start with neither, and no name may contain "..".
function npmName(n) {
  return /^(?:@[a-zA-Z0-9_~-][a-zA-Z0-9._~-]*\/[a-zA-Z0-9_~-][a-zA-Z0-9._~-]*|[a-zA-Z0-9~-][a-zA-Z0-9._~-]*)$/.test(n) && !n.includes("..");
}
const MAX_MANIFEST = 10 << 20; // the server's npm manifest limit

// tarPackageJson gunzips the tarball as a stream and scans the tar headers
// (512-byte blocks, ustar prefix + name) for package/package.json.
async function tarPackageJson(file) {
  const rd = file.stream().pipeThrough(new DecompressionStream("gzip")).getReader();
  const td = new TextDecoder();
  let buf = new Uint8Array(0), off = 0;
  const more = async () => {
    let x;
    try { x = await rd.read(); } catch (e) { throw new Error(t("up.e.gzip")); }
    if (x.done) throw new Error(t("up.e.early"));
    return x.value;
  };
  const need = async (n) => { // at least n unread bytes in buf
    while (buf.length - off < n) {
      const c = await more(), nb = new Uint8Array(buf.length - off + c.length);
      nb.set(buf.subarray(off));
      nb.set(c, buf.length - off);
      buf = nb;
      off = 0;
    }
  };
  const skip = async (n) => {
    while (n > 0) {
      if (off >= buf.length) { buf = await more(); off = 0; }
      const k = Math.min(n, buf.length - off);
      off += k;
      n -= k;
    }
  };
  try {
    for (let i = 0; i < 100000; i++) {
      await need(512);
      const hd = buf.subarray(off, off + 512);
      off += 512;
      if (hd.every((b) => b === 0)) break;
      const str = (s, e) => { const b = hd.subarray(s, e), z = b.indexOf(0); return td.decode(z < 0 ? b : b.subarray(0, z)); };
      const size = parseInt(str(124, 136).trim() || "0", 8);
      if (!(size >= 0)) throw new Error(t("up.e.tar"));
      let name = str(0, 100);
      // The prefix field exists only in POSIX ustar (magic "ustar" NUL, version "00");
      // GNU tar ("ustar  " NUL) uses those bytes for other fields.
      const posix = str(257, 262) === "ustar" && hd[262] === 0 && hd[263] === 48 && hd[264] === 48;
      if (posix && str(345, 500)) name = str(345, 500) + "/" + name;
      if ((hd[156] === 0 || hd[156] === 48) && name === "package/package.json") {
        if (size > 1 << 20) throw new Error(t("up.e.pkgBig"));
        await need(size);
        return JSON.parse(td.decode(buf.subarray(off, off + size)));
      }
      await skip(Math.ceil(size / 512) * 512);
    }
    throw new Error(t("up.e.noPkg"));
  } finally {
    rd.cancel().catch(() => {});
  }
}

// b64parts encodes the file in 768 KiB slices (a multiple of 3, so the parts
// concatenate to one valid base64 string) without one giant string.
async function b64parts(f) {
  const out = [], STEP = 3 << 18;
  for (let o = 0; o < f.size; o += STEP) {
    const b = new Uint8Array(await f.slice(o, o + STEP).arrayBuffer()), bin = [];
    for (let i = 0; i < b.length; i += 0x8000) bin.push(String.fromCharCode.apply(null, b.subarray(i, i + 0x8000)));
    out.push(btoa(bin.join("")));
  }
  return out;
}

function npmForm(r, ui) {
  const file = fileInput(".tgz,.tar.gz");
  const info = h("div", { class: "up-info" });
  const meta = h("div");
  let pkg = null, reading = null;
  file.addEventListener("change", () => {
    reading = (async () => {
      pkg = null;
      put(info);
      ui.clear();
      const f = file.files[0];
      if (!f) return;
      put(info, h("p", { class: "hint" }, t("up.reading")));
      try {
        if (typeof DecompressionStream !== "function") throw new Error(t("up.e.gzipApi"));
        const j = await tarPackageJson(f);
        if (file.files[0] !== f) return;
        if (!j || typeof j.name !== "string" || !npmName(j.name) || j.name.length > 214) throw new Error(t("up.e.npmName"));
        if (typeof j.version !== "string" || !/^[^\s/\\]+$/.test(j.version)) throw new Error(t("up.e.npmVer"));
        pkg = j;
        put(info, kv(t("col.name"), j.name), kv(t("col.version"), j.version), kv(t("col.bytes"), fmtBytes(f.size)),
          f.size > MAX_MANIFEST ? h("div", { class: "notice amber", role: "status" }, t("up.npmBig")) : null);
      } catch (e) {
        if (file.files[0] !== f) return;
        put(info);
        ui.fail([f.name + ": " + e.message]);
      }
    })();
  });

  async function check1() {
    if (reading) await reading;
    const f = file.files[0];
    if (!f) return [t("up.e.tgz")];
    if (!pkg) return [t("up.e.unread")];
    put(meta, h("div", { class: "fields" },
      field(t("col.name"), input({ value: pkg.name, readOnly: true })), field(t("col.version"), input({ value: pkg.version, readOnly: true }))),
    h("p", { class: "hint" }, t("up.npmMeta")));
    return [];
  }

  async function plan() {
    const errs = await check1();
    if (errs.length) return { errs };
    const f = file.files[0];
    const name = pkg.name, ver = pkg.version, bare = name.slice(name.lastIndexOf("/") + 1);
    const man = Object.assign({}, pkg, { _id: name + "@" + ver });
    delete man.dist; // the server sets dist (tarball, shasum, integrity) from the stored content
    const doc = JSON.stringify({ _id: name, name, "dist-tags": { latest: ver }, versions: { [ver]: man } });
    const body = new Blob([doc.slice(0, -1), ",\"_attachments\":{", JSON.stringify(name + "-" + ver + ".tgz"),
      ":{\"content_type\":\"application/octet-stream\",\"length\":" + f.size + ",\"data\":\"", ...await b64parts(f), "\"}}}"]);
    const url = repoBase(r) + (name[0] === "@" ? "@" + enc(name.slice(1)) : enc(name));
    return {
      comp: name + "@" + ver,
      steps: [{ label: f.name + " → " + name + "@" + ver, go: (p) => ui.send("PUT", url, body, "application/json", p) }],
      link: browseLink(r.name, name + "/-/" + bare + "-" + ver + ".tgz"),
    };
  }
  return { files: [field(t("up.tarball"), file, t("up.tarHelp"), true), info], meta: [meta], check1, plan };
}

// ---- PyPI -----------------------------------------------------------------------

// pyParse reads name and version from a wheel or sdist file name.
function pyParse(fn) {
  let m = /^([^-]+)-([^-]+)-(?:[^-]+-)?([^-]+)-[^-]+-[^-]+\.whl$/.exec(fn);
  if (m) return { name: m[1], version: m[2], filetype: "bdist_wheel", pyversion: m[3] };
  m = /^(.+?)-(\d[^-]*)\.(?:tar\.gz|zip)$/.exec(fn);
  return m ? { name: m[1], version: m[2], filetype: "sdist", pyversion: "source" } : null;
}

const hex = (buf) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");

function pypiForm(r, ui) {
  const file = fileInput(".whl,.tar.gz,.zip");
  const nm = input({ maxlength: 200 }), ver = input({ maxlength: 100 });
  let kind = null;
  file.addEventListener("change", () => {
    ui.clear();
    const f = file.files[0];
    kind = f ? pyParse(f.name) : null;
    if (f && !kind) ui.fail([t("up.e.pyName", { file: f.name })]);
    if (kind) { nm.value = kind.name; ver.value = kind.version; }
  });
  const check1 = () => (file.files[0] && kind ? [] : [t("up.e.pyPick")]);

  async function plan() {
    const f = file.files[0], name = nm.value.trim(), version = ver.value.trim();
    const errs = check1();
    if (!name) errs.push(t("up.e.name"));
    if (!version) errs.push(t("up.e.ver"));
    if (errs.length) return { errs };
    const fd = new FormData();
    for (const [k, val] of [[":action", "file_upload"], ["protocol_version", "1"], ["metadata_version", "2.1"], ["name", name],
      ["version", version], ["filetype", kind.filetype], ["pyversion", kind.pyversion]]) fd.append(k, val);
    // crypto.subtle exists only in a secure context (https or localhost); the
    // server treats sha256_digest as optional and always computes its own.
    if (window.crypto && crypto.subtle) fd.append("sha256_digest", hex(await crypto.subtle.digest("SHA-256", await f.arrayBuffer())));
    fd.append("content", f, f.name);
    return {
      comp: name + " " + version,
      steps: [{ label: f.name, go: (p) => ui.send("POST", repoBase(r), fd, "", p) }],
      link: browseLink(r.name, "packages/" + name.toLowerCase().replace(/[-_.]+/g, "-") + "/" + f.name),
    };
  }
  return {
    files: [field(t("up.file"), file, t("up.pyHelp"), true)],
    meta: [h("div", { class: "fields" }, field(t("col.name"), nm), field(t("col.version"), ver))],
    check1, plan,
  };
}

// ---- the stepper ----------------------------------------------------------------

const FORMS = { maven: mavenForm, npm: npmForm, pypi: pypiForm };

function why(r) {
  if (r.type !== "hosted") return t("up.whyHosted");
  if (!FORMS[r.format]) return t("up.whyFormat");
  return t("up.whyPerm");
}

export async function viewUpload(view, name) {
  if (state.me.anonymous) return needLogin(view, t("nav.upload"));
  const isLive = alive();
  let r = null;
  try {
    r = await api(apiRepo(name));
  } catch (e) {
    if (e.status !== 404) throw e;
  }
  if (!isLive()) return;
  if (!r || !canUpload(r)) {
    put(view, head(r ? h("p", { class: "badges" }, fmtBadge(r.format), typeBadge(r.type)) : null), h("section", { class: "card" },
      h("div", { class: "empty" }, r ? why(r) : t("up.notFound", { name })),
      h("p", { class: "center" }, h("a", { class: "btn", href: uploadLink() }, t("up.back")))));
    return;
  }
  if (!state.repos) {
    try { state.repos = (await api("repositories")).repositories || []; } catch (e) { state.repos = [r]; }
    if (!isLive()) return;
  }
  const targets = state.repos.filter(canUpload);
  if (!targets.some((x) => x.name === r.name)) targets.unshift(r);

  const errBox = h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
  const okBox = h("div", { class: "notice green", role: "status", hidden: true });
  const prog = h("ul", { class: "up-prog", hidden: true });
  const review = h("div");
  const ui = {
    send: sender(),
    clear() { errBox.hidden = true; },
    fail(msgs) {
      put(errBox, h("strong", null, t("up.failed")), h("ul", null, msgs.map((m) => h("li", null, m))));
      errBox.hidden = false;
      errBox.focus();
    },
  };
  const f = FORMS[r.format](r, ui);
  const st = stepper();
  const btn = (label, cls, type) => h("button", { type: type || "button", class: "btn" + (cls ? " " + cls : "") }, label);
  const next1 = btn(t("up.next"), "primary"), prev2 = btn(t("up.prev")), next2 = btn(t("up.toReview"), "primary");
  const prev3 = btn(t("up.prev")), submit = btn(t("up.upload"), "primary", "submit");
  submit.prepend(icon("upload"));
  const again = btn(t("up.again"));
  again.addEventListener("click", rerender); // a fresh stepper for the same repository
  const done3 = h("div", { class: "actions end", hidden: true }, again);
  const acts3 = h("div", { class: "actions end" }, prev3, submit);
  const steps = [
    card(t("up.card1"), t("up.card1Sub"), [
      h("div", { class: "fields" }, targetSelect(targets, r.name)),
      h("div", { class: "dropzone" }, h("strong", null, t("up.dropTitle")), h("p", null, t("up.dropFormat", { f: FORMAT_NAMES[r.format] })),
        h("div", { class: "drop-body" }, f.files)),
      h("div", { class: "actions end" }, next1)]),
    card(t("up.card2"), r.name + " · " + FORMAT_NAMES[r.format], [f.meta, h("div", { class: "actions end" }, prev2, next2)]),
    card(t("up.card3"), t("up.card3Sub"), [review, prog, okBox, acts3, done3]),
  ];
  let step = 0, busy = false, plan = null;
  // A language change must not rebuild the screen while a file is chosen or an upload runs.
  let unBusy = null;
  const syncBusy = () => {
    const want = busy || [...form.querySelectorAll("input[type=file]")].some((i) => i.files && i.files.length);
    if (want && !unBusy) unBusy = markBusy("upload");
    else if (!want && unBusy) { unBusy(); unBusy = null; }
  };
  const show = (n, focus) => {
    step = n;
    st.set(n);
    steps.forEach((c, i) => { c.hidden = i !== n; });
    if (focus) { const t2 = steps[n].querySelector("h2"); t2.tabIndex = -1; t2.focus(); }
  };
  const guard = async (fn) => {
    if (busy) return;
    busy = true;
    syncBusy();
    try { await fn(); } catch (e) { if (isLive()) ui.fail([e.message || String(e)]); } finally { busy = false; syncBusy(); }
  };
  next1.addEventListener("click", () => guard(async () => {
    ui.clear();
    const errs = await f.check1();
    if (!isLive()) return;
    if (errs.length) { ui.fail(errs); return; }
    show(1, true);
  }));
  prev2.addEventListener("click", () => { ui.clear(); show(0, true); });
  next2.addEventListener("click", () => guard(async () => {
    ui.clear();
    const p = await f.plan();
    if (!isLive()) return;
    if (p.errs) { ui.fail(p.errs); return; }
    plan = p;
    put(review, h("div", null, kv(t("up.target"), r.name), kv(t("comp.component"), p.comp), kv(t("up.files"), String(p.steps.length))),
      h("ul", { class: "up-list" }, p.steps.map((s) => h("li", null, s.label))));
    prog.hidden = true;
    okBox.hidden = true;
    acts3.hidden = false;
    done3.hidden = true;
    show(2, true);
  }));
  prev3.addEventListener("click", () => { ui.clear(); show(1, true); });

  const form = h("form", { class: "up", novalidate: true }, errBox, steps);
  form.addEventListener("change", syncBusy);
  form.addEventListener("click", () => setTimeout(syncBusy, 0)); // a removed row drops its file
  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    // Enter in a field moves on a step; only step 3 uploads.
    if (step === 0) return next1.click();
    if (step === 1) return next2.click();
    if (busy || !plan) return;
    const p = plan;
    guard(async () => {
      submit.disabled = true;
      prev3.disabled = true;
      ui.clear();
      const lis = p.steps.map((s) => {
        const bar = h("progress", { max: "1", value: "0" }), sx = h("span", { class: "up-st" }, t("up.waiting"));
        return { bar, st: sx, el: h("li", null, h("span", { class: "up-name" }, s.label), bar, sx) };
      });
      put(prog, lis.map((x) => x.el));
      prog.hidden = false;
      try {
        for (let i = 0; i < p.steps.length; i++) {
          if (!isLive()) return; // the view was left between steps: do not start the next upload
          const x = lis[i];
          x.st.textContent = t("up.sending");
          try {
            await p.steps[i].go((v) => { x.bar.value = v; });
          } catch (e) {
            x.st.textContent = t("up.stFailed");
            throw e;
          }
          x.bar.value = 1;
          x.st.textContent = t("up.done");
        }
        if (!isLive()) return;
        put(okBox, icon("ok"), " " + t("up.ok") + " ", h("a", { href: p.link }, t("up.view")));
        okBox.hidden = false;
        acts3.hidden = true;
        done3.hidden = false;
        plan = null;
      } finally {
        submit.disabled = false;
        prev3.disabled = false;
      }
    });
  });
  put(view, head(), st.el, h("div", { class: "form-layout" }, form, helpAside(r.format)));
  show(0);
}
