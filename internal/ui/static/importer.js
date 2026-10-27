// SYSTEM → Import from Nexus Repository (plan 15 Decision 7) as the concept's
// three-step stepper: 1. the source and a connection test that lists its
// repositories, 2. the options with a dry run and the start, 3. the live
// progress, stop and the final report; then the job history.
//   #admin/system/nexusimport
// The source password lives only in its form field: every request reads it
// and clears the field at once, so it is never kept in a variable, and it
// must be typed again for the next request. Every name and number comes from
// the API and reaches the page through h()/textContent.
import {
  h, put, api, encSeg, alive, fmtBytes, fmtNum, fmtDate, pageHead, card, tbl, stat, typeBadge, handleError, markBusy, state,
} from "./util.js";
import { t } from "./i18n.js";
import { confirmDelete } from "./browse.js";
import { field, fields, sel, textIn, button, errList } from "./ops.js";

const dash = "—";
const POLL = 1500;
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const notice = (text, cls) => h("div", { class: "notice" + (cls ? " " + cls : "") }, text);
const STATUS = {
  ok: ["im.stOk", "green"], failed: ["im.stFailed", "red"], stopped: ["im.stStopped", "amber"],
  running: ["im.stRunning", "blue"], "dry-run": ["im.stDry", "purple"],
};
const statusBadge = (s) => { const x = STATUS[s]; return badge(x ? t(x[0]) : s || dash, x ? x[1] : ""); };
const DEF = {
  created: ["im.defCreated", "green"], exists: ["im.defExists", "blue"], "would-create": ["im.defWould", "purple"],
  conflict: ["im.defConflict", "red"], missing: ["im.defMissing", "amber"], failed: ["im.defFailed", "red"], skipped: ["im.defSkipped", ""],
};
const defBadge = (d) => { const x = DEF[d]; return badge(x ? t(x[0]) : d || dash, x ? x[1] : ""); };
// The source names Maven "maven2"; the other formats share their names.
const FMT_CLASS = { maven2: "maven", npm: "npm", pypi: "pypi", go: "go", cargo: "cargo" };
const srcFmt = (f) => h("span", { class: "badge" + (FMT_CLASS[f] ? " f-" + FMT_CLASS[f] : "") }, f || dash);
const supportBadge = (r) => (!r.supported ? badge(t("im.unsupported"), "amber")
  : r.content ? badge(t("im.supContent"), "green") : badge(t("im.supDef"), "blue"));
const pct = (a, b) => (b > 0 ? Math.min(100, (a / b) * 100) : 0);
const jobLink = (job) => "import/jobs/" + encSeg(job);
// notImportedText translates the report's fixed "not imported" lines by
// their prefix; anything else is shown as the server wrote it.
const NOT_IMPORTED = [["users, roles and privileges", "im.niUsers"], ["proxy remote credentials", "im.niProxyCreds"], ["proxy caches", "im.niCaches"]];
const notImportedText = (s) => { const m = NOT_IMPORTED.find(([p]) => String(s).startsWith(p)); return m ? t(m[1]) : s; };
const listOf = (title, items, extra) => (items && items.length ? h("div", { class: "im-list" },
  h("h3", null, title), h("ul", null, items.map((x) => h("li", null, x))), extra || null) : null);

// stepper is the concept's stepper (as in upload.js).
function stepper() {
  const steps = [t("im.step1"), t("im.step2"), t("im.step3")].map((label, i) =>
    h("span", { class: "step" }, h("i", null, String(i + 1)), h("span", { class: "step-label" }, label)));
  const el = h("div", { class: "stepper", role: "list", "aria-label": t("im.steps") },
    steps.flatMap((s, i) => [h("span", { role: "listitem", class: "step-wrap" }, s), i < 2 ? h("span", { class: "step-line", "aria-hidden": "true" }) : null]));
  const set = (n) => steps.forEach((s, i) => {
    s.classList.toggle("active", i === n);
    s.classList.toggle("done", i < n);
    if (i === n) s.setAttribute("aria-current", "step"); else s.removeAttribute("aria-current");
  });
  set(0);
  return { el, set };
}

// progressView shows the live counters of a running job.
function progressView(p) {
  if (!p) return h("div", { class: "loading" }, t("im.starting"));
  const handled = (p.assetsDone || 0) + (p.assetsSkipped || 0) + (p.assetsFailed || 0);
  return [h("div", { class: "stats" },
    stat({ label: t("im.pRepos"), value: fmtNum(p.repositoriesDone), unit: "/ " + fmtNum(p.repositoriesTotal), pct: pct(p.repositoriesDone, p.repositoriesTotal) }),
    stat({ label: t("im.pAssets"), value: fmtNum(handled), unit: "/ " + fmtNum(p.assetsTotal), pct: pct(handled, p.assetsTotal), color: "green" }),
    stat({ label: t("im.pResult"), value: fmtNum(p.assetsDone), unit: t("im.imported"),
      foot: t("im.skippedN", { n: fmtNum(p.assetsSkipped) }), right: t("im.failedN", { n: fmtNum(p.assetsFailed) }) }),
    stat({ label: t("im.pBytes"), value: fmtBytes(p.bytes) })),
  h("p", { class: "muted" }, t("im.current"), " ", p.repository ? h("code", null, p.repository) : dash)];
}

// reportView is a job's final report: the totals, the per-repository table
// and the lists (unsupported formats, proxies needing credentials,
// conflicts, warnings, what is not imported), with a JSON download.
function reportView(rep, job) {
  const tot = rep.totals || {};
  const dry = !!rep.dryRun;
  const dl = h("a", { class: "btn small", href: "/-/api/" + jobLink(job) + "?download=1", download: "import-" + job + ".json" }, t("im.download"));
  const tiles = dry ? [
    stat({ label: t("im.wouldCreate"), value: fmtNum(rep.created) }),
    stat({ label: t("im.wouldImport"), value: fmtNum(tot.wouldImport), foot: fmtBytes(tot.bytes) }),
    stat({ label: t("im.wouldSkip"), value: fmtNum((tot.wouldSkip || 0) + (tot.alreadyDone || 0)),
      foot: t("im.genSkipped", { n: fmtNum(tot.skipped) }) }),
    stat({ label: t("im.conflicts"), value: fmtNum((rep.conflicts || []).length + (rep.conflictsOmitted || 0)) }),
  ] : [
    stat({ label: t("im.created"), value: fmtNum(rep.created) }),
    stat({ label: t("im.importedT"), value: fmtNum(tot.imported), foot: fmtBytes(tot.bytes) }),
    stat({ label: t("im.skippedT"), value: fmtNum((tot.skipped || 0) + (tot.alreadyDone || 0)),
      foot: t("im.alreadyN", { n: fmtNum(tot.alreadyDone) }) }),
    stat({ label: t("im.failedT"), value: fmtNum((tot.failed || 0) + (tot.notRetried || 0)),
      foot: tot.notRetried ? t("im.notRetriedN", { n: fmtNum(tot.notRetried) }) : "" }),
  ];
  const rows = (rep.repositories || []).map((r) => {
    const c = r.counts || {};
    const errs = r.errors || [];
    const notes = r.notes || [];
    const more = errs.length || notes.length ? h("details", { class: "im-details" },
      h("summary", null, t("im.detailsN", { n: fmtNum(errs.length + (r.omitted || 0) + notes.length) })),
      h("ul", null, errs.map((x) => h("li", { class: "im-err" }, x)), r.omitted ? h("li", null, t("im.omittedN", { n: fmtNum(r.omitted) })) : null,
        notes.map((x) => h("li", null, x)))) : null;
    return h("tr", null,
      h("td", { class: "name" }, h("code", null, r.source), r.target && r.target !== r.source ? h("div", { class: "tiny muted" }, "→ " + r.target) : null, more),
      h("td", null, srcFmt(r.format), " ", typeBadge(r.type)),
      h("td", null, defBadge(r.definition)),
      h("td", { class: "num" }, r.content ? fmtNum(dry ? c.wouldImport : c.imported) : dash),
      h("td", { class: "num" }, r.content ? fmtNum((dry ? c.wouldSkip || 0 : 0) + (c.skipped || 0) + (c.alreadyDone || 0)) : dash),
      h("td", { class: "num" }, r.content ? ((c.failed || 0) + (c.notRetried || 0) ? badge(fmtNum((c.failed || 0) + (c.notRetried || 0)), "red") : "0") : dash),
      h("td", { class: "num" }, r.content ? fmtBytes(c.bytes) : dash));
  });
  const unsup = (rep.unsupported || []).map((u) => [h("code", null, u.format), " ", t("im.unsupCount", { n: fmtNum(u.count) }), ": ", (u.repositories || []).join(", "),
    u.format === "docker" ? h("div", { class: "muted tiny" }, t("im.dockerNote")) : null]);
  return [
    h("div", { class: "im-repohead" }, statusBadge(rep.status), dry && rep.status !== "dry-run" ? badge(t("im.dryBadge"), "purple") : null,
      h("span", { class: "muted tiny" }, rep.source || "", " · ", fmtDate(rep.started), rep.finished ? " – " + fmtDate(rep.finished) : ""), dl),
    rep.message ? notice(rep.message, rep.status === "ok" || rep.status === "dry-run" ? "" : "amber") : null,
    h("div", { class: "stats" }, tiles),
    tbl([t("im.colRepo"), t("im.colFormat"), t("im.colDef"), { t: dry ? t("im.colWould") : t("im.colImported"), cls: "num" },
      { t: t("im.colSkipped"), cls: "num" }, { t: t("im.colFailed"), cls: "num" }, { t: t("col.size"), cls: "num" }], rows, t("im.noRepos")),
    listOf(t("im.unsupTitle"), unsup, h("p", { class: "tiny muted" }, t("im.unsupHint"))),
    listOf(t("im.proxyTitle"), rep.proxiesNeedingCredentials, h("p", { class: "tiny muted" }, t("im.proxyHint"))),
    listOf(t("im.conflictTitle"), rep.conflicts, rep.conflictsOmitted ? h("p", { class: "tiny muted" }, t("im.omittedN", { n: fmtNum(rep.conflictsOmitted) })) : null),
    listOf(t("im.warnTitle"), rep.warnings),
    listOf(t("im.notImportedTitle"), (rep.notImported || []).map(notImportedText)),
  ];
}

export async function viewImport(view) {
  const isLive = alive();
  const head = pageHead(t("nav.importNexus"), h("p", null, t("im.sub")), null, { eyebrow: t("bk.eyebrow") });
  let jobs;
  try {
    jobs = await api("import/jobs");
  } catch (e) {
    if (e.status !== 501) throw e;
    put(view, head, h("section", { class: "card" }, h("div", { class: "empty" }, t("im.unavailable"))));
    return;
  }
  let stores = [];
  try { stores = ((await api("blobstores")).blobstores || []).map((b) => b.name); } catch (e) { /* the default store */ }
  if (!isLive()) return;
  if (!stores.includes("default")) stores.unshift("default");

  let timer = 0;
  state.leaving.push(() => clearTimeout(timer));
  const sleep = (ms) => new Promise((res) => { timer = setTimeout(res, ms); });
  const st = stepper();
  const msg = h("div", { class: "notice", role: "status", hidden: true });
  const say = (text, cls) => {
    msg.className = "notice" + (cls ? " " + cls : "");
    msg.textContent = text;
    msg.hidden = !text;
  };
  const errBox = h("div", { class: "form-errors", role: "alert", tabindex: "-1", hidden: true });
  const showErrs = (list) => {
    put(errBox, h("ul", null, list.map((x) => h("li", null, x))));
    errBox.hidden = false;
    errBox.focus();
  };
  const fail = (e) => {
    if (e.status === 401 || e.status === 403) { handleError(e); return; }
    if (e.status === 409) { say(t("task.conflict", { name: (e.data && (e.data.runningName || e.data.running)) || "?" }), "amber"); return; }
    showErrs(errList(e));
  };

  // ---- step 1: the source -------------------------------------------------------------
  const form = h("form", { novalidate: true });
  let unBusyForm = null;
  const dirty = () => { if (!unBusyForm) unBusyForm = markBusy("import-form"); };
  form.addEventListener("input", dirty);
  form.addEventListener("change", dirty);
  const urlIn = textIn("", { placeholder: "https://nexus.example.com", maxlength: 500, inputmode: "url" });
  const userIn = textIn("", { maxlength: 200 });
  const pwIn = h("input", { type: "password", autocomplete: "off", maxlength: 1000 });
  const insecure = h("input", { type: "checkbox" });
  const insecureWarn = notice(t("im.insecureWarn"), "amber");
  insecureWarn.hidden = true;
  insecure.addEventListener("change", () => { insecureWarn.hidden = !insecure.checked; });
  const testBtn = h("button", { type: "submit", class: "btn primary" }, t("im.test"));
  const repoBox = h("div", null);
  // takePassword reads the field and clears it: the value goes into one request only.
  const takePassword = () => { const p = pwIn.value; pwIn.value = ""; return p; };
  const source = () => ({ url: urlIn.value.trim(), username: userIn.value.trim(), insecureSkipVerify: insecure.checked });
  const sourceKey = () => JSON.stringify(source());
  let tested = ""; // sourceKey of the last successful test
  let list = [];   // the source repositories of that test
  const boxes = new Map();

  // needPassword: a user without a password in the field cannot be sent.
  const needPassword = () => {
    if (!urlIn.value.trim()) { showErrs([t("im.eUrl")]); urlIn.focus(); return true; }
    if (userIn.value.trim() && !pwIn.value) { showErrs([t("im.ePw")]); pwIn.focus(); return true; }
    return false;
  };

  function renderRepos() {
    boxes.clear();
    const all = h("input", { type: "checkbox", "aria-label": t("im.selectAll") });
    const count = h("span", { class: "end", role: "status" });
    const sync = () => {
      const sup = list.filter((r) => r.supported);
      const n = sup.filter((r) => boxes.get(r.name).checked).length;
      all.checked = n === sup.length && n > 0;
      all.indeterminate = n > 0 && n < sup.length;
      count.textContent = t("im.selectedN", { n: fmtNum(n), total: fmtNum(sup.length) });
    };
    all.addEventListener("change", () => { for (const r of list) if (r.supported) boxes.get(r.name).checked = all.checked; sync(); });
    const rows = list.map((r) => {
      const box = h("input", { type: "checkbox", checked: !!r.supported, disabled: !r.supported, "aria-label": r.name });
      box.addEventListener("change", sync);
      boxes.set(r.name, box);
      return h("tr", { class: r.supported ? null : "muted" },
        h("td", null, box),
        h("td", { class: "name" }, h("code", null, r.name)),
        h("td", null, srcFmt(r.format)),
        h("td", null, typeBadge(r.type)),
        h("td", null, supportBadge(r)));
    });
    put(repoBox,
      h("div", { class: "im-pickbar" }, h("label", { class: "check" }, all, t("im.selectAll")), count),
      tbl([{ t: h("span", { class: "sr-only" }, t("im.colPick")), cls: "act" }, t("im.colRepo"), t("im.colFormat"), t("im.colType"), t("im.colSupport")],
        rows, t("im.noSource")),
      h("p", { class: "tiny muted" }, t("im.supportHint")));
    sync();
  }

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    errBox.hidden = true;
    if (needPassword()) return;
    const body = Object.assign(source(), { password: takePassword() });
    const key = sourceKey();
    testBtn.disabled = true;
    say(t("im.testing"), "");
    try {
      const d = await api("import/nexus/test", { method: "POST", body });
      if (!isLive()) return;
      list = d.repositories || [];
      tested = key;
      renderRepos();
      say(t("im.testOk", { n: fmtNum(list.length), sup: fmtNum(list.filter((r) => r.supported).length) }), "ok");
      step2Card.hidden = false;
      toStep(Math.max(1, stepNow));
    } catch (e) {
      if (!isLive()) return;
      say("", "");
      fail(e);
    } finally {
      body.password = "";
      if (isLive()) testBtn.disabled = !!following;
    }
  });
  put(form,
    fields(
      field(t("im.url"), urlIn, t("im.urlHelp"), { full: true, req: true }),
      field(t("im.user"), userIn, t("im.userHelp")),
      field(t("im.password"), pwIn, t("im.pwHelp")),
      h("div", { class: "field full" }, h("label", { class: "check" }, insecure, t("im.insecure")), insecureWarn)),
    h("div", { class: "actions" }, testBtn));
  const step1Card = card(t("im.step1Title"), t("im.step1Sub"), [form, repoBox]);

  // ---- step 2: options, dry run, start ----------------------------------------------------
  const defs = h("input", { type: "checkbox", checked: true });
  const content = h("input", { type: "checkbox", checked: true });
  const retry = h("input", { type: "checkbox" });
  const blob = sel(stores.map((n) => [n, n]), "default");
  const prefix = textIn("", { maxlength: 60, placeholder: "nx-" });
  const jobIn = textIn("", { maxlength: 100, placeholder: t("im.jobAuto") });
  const conc = h("input", { type: "number", inputmode: "numeric", min: 1, max: 16, step: 1, value: "4" });
  const dryBtn = button(t("im.dryRun"));
  const startBtn = button(t("im.start"), "primary");
  const dryBox = h("div", null);
  const options = () => {
    const sup = list.filter((r) => r.supported);
    const picked = sup.filter((r) => boxes.get(r.name) && boxes.get(r.name).checked).map((r) => r.name);
    // Every supported repository picked: send none (all), so the report also counts the unsupported ones.
    return Object.assign(source(), {
      repositories: picked.length === sup.length ? [] : picked, importDefinitions: defs.checked, importContent: content.checked,
      blobStore: blob.value, renamePrefix: prefix.value.trim(), job: jobIn.value.trim(), concurrency: Number(conc.value) || 0, retryFailed: retry.checked,
    });
  };
  const step2Card = card(t("im.step2Title"), t("im.step2Sub"), [
    fields(
      h("div", { class: "field" }, h("label", { class: "check" }, defs, t("im.defs")), h("small", null, t("im.defsHelp"))),
      h("div", { class: "field" }, h("label", { class: "check" }, content, t("im.content")), h("small", null, t("im.contentHelp"))),
      field(t("im.blobStore"), blob, t("im.blobHelp")),
      field(t("im.prefix"), prefix, t("im.prefixHelp")),
      field(t("im.job"), jobIn, t("im.jobHelp")),
      field(t("im.concurrency"), conc, t("im.concHelp")),
      h("div", { class: "field full" }, h("label", { class: "check" }, retry, t("im.retry")), h("small", null, t("im.retryHelp")))),
    notice(t("im.pwAgain"), ""),
    h("div", { class: "actions" }, dryBtn, startBtn),
    dryBox]);
  step2Card.hidden = true;

  // ---- step 3: progress and report ----------------------------------------------------------
  const progBox = h("div", null);
  const stopBtn = button(t("im.stop"), "danger");
  stopBtn.hidden = true;
  const step3Card = card(t("im.step3Title"), t("im.step3Sub"), progBox, stopBtn);
  step3Card.hidden = true;
  let stepNow = 0;
  const toStep = (n) => { stepNow = n; st.set(n); };

  let following = null; // {job, run, dry} of the run followed
  const sync = () => {
    const busy = !!following;
    testBtn.disabled = busy;
    dryBtn.disabled = busy;
    startBtn.disabled = busy;
    stopBtn.hidden = !busy || following.dry || !following.job;
  };

  // follow polls the job until it ends, showing the progress in box, then
  // its report (or the run's outcome when it ended without one).
  async function follow(job, run, dry, box) {
    following = { job, run, dry };
    sync();
    const unBusy = markBusy("import-run");
    const q = run ? "?run=" + encodeURIComponent(run) : "";
    try {
      for (;;) {
        let x;
        try {
          x = await api(jobLink(job) + q);
        } catch (e) {
          if (!isLive()) return;
          if (e.status === 404) { await sleep(POLL); if (!isLive()) return; continue; } // not started yet
          throw e;
        }
        if (!isLive()) return;
        if (x.status !== "running") {
          if (!x.report) {
            put(box, notice(t("im.noReport", { msg: x.message || dash }), "amber"));
            say(t("im.endedNoReport", { job }), "amber");
          } else {
            put(box, reportView(x.report, job));
            say(t(dry ? "im.dryDone" : "im.done", { job, status: STATUS[x.status] ? t(STATUS[x.status][0]) : x.status }),
              x.status === "ok" || x.status === "dry-run" ? "ok" : "amber");
          }
          break;
        }
        put(box, h("p", { class: "muted" }, t(dry ? "im.dryRunning" : "im.running", { job })), progressView(x.progress));
        await sleep(POLL);
        if (!isLive()) return;
      }
    } catch (e) {
      if (isLive()) { say(e.message, "amber"); handleError(e); }
    } finally {
      unBusy();
      if (isLive()) {
        following = null;
        sync();
        loadHistory();
      }
    }
  }

  async function launch(dry) {
    errBox.hidden = true;
    if (!tested || tested !== sourceKey()) { showErrs([t("im.eRetest")]); return; }
    const o = options();
    if (!o.repositories.length && !list.some((r) => r.supported && boxes.get(r.name).checked)) { showErrs([t("im.ePick")]); return; }
    if (needPassword()) return;
    const body = Object.assign(o, { dryRun: dry, password: takePassword() });
    following = { job: "", run: "", dry }; // starting: no stop yet
    sync();
    let res;
    try {
      res = await api("import/nexus", { method: "POST", body });
    } catch (e) {
      following = null;
      if (!isLive()) return;
      sync();
      fail(e);
      return;
    } finally {
      body.password = "";
    }
    if (!isLive()) return;
    say(t(dry ? "im.dryStarted" : "im.started", { job: res.job }), "");
    loadHistory();
    if (dry) {
      put(dryBox, h("div", { class: "loading" }, t("im.starting")));
      follow(res.job, res.id, true, dryBox);
    } else {
      step3Card.hidden = false;
      toStep(2);
      follow(res.job, res.id, false, progBox);
      step3Card.scrollIntoView({ block: "start" });
    }
  }
  dryBtn.addEventListener("click", () => launch(true));
  startBtn.addEventListener("click", () => launch(false));
  stopBtn.addEventListener("click", async () => {
    if (!following) return;
    const job = following.job;
    const ok = await confirmDelete(t("im.stopText", { job }), t("im.stopTitle"), t("im.stop"));
    if (!ok || !isLive()) return;
    try {
      await api(jobLink(job) + "/stop", { method: "POST" });
      if (isLive()) say(t("im.stopping", { job }), "amber");
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 409) say(t("im.notRunning", { job }), "amber");
      else { say(e.message, "amber"); handleError(e); }
    }
  });

  // ---- history ----------------------------------------------------------------------------
  const histBox = h("div", null);
  async function showReport(job) {
    try {
      const x = await api(jobLink(job));
      if (!isLive()) return;
      step3Card.hidden = false;
      toStep(2);
      if (x.status === "running") { if (!following) follow(job, "", !!x.dryRun, progBox); return; }
      put(progBox, x.report ? reportView(x.report, job) : notice(t("im.noReport", { msg: x.message || dash }), "amber"));
      step3Card.scrollIntoView({ block: "start" });
    } catch (e) {
      if (isLive()) { say(e.message, "amber"); handleError(e); }
    }
  }
  function renderHistory(d) {
    const all = (d.jobs || []).concat(d.dryRuns || []);
    all.sort((a, b) => String(b.started || "").localeCompare(String(a.started || "")));
    const running = d.running;
    // The API serves the newest run of a job id, so only that row gets a report button.
    const seen = new Set();
    const rows = all.map((j) => {
      const isRun = running && running.job === j.job && !!running.dryRun === !!j.dryRun;
      const newest = !seen.has(j.job);
      seen.add(j.job);
      const view = newest ? button(isRun ? t("im.follow") : t("im.report"), "small") : null;
      if (view) view.addEventListener("click", () => showReport(j.job));
      return h("tr", null,
        h("td", { class: "name" }, h("code", null, j.job), j.dryRun && j.status !== "dry-run" ? h("div", null, badge(t("im.dryBadge"), "purple")) : null),
        h("td", null, j.source || dash),
        h("td", null, statusBadge(isRun ? "running" : j.status)),
        h("td", { class: "nowrap" }, fmtDate(j.started)),
        h("td", { class: "nowrap" }, j.finished ? fmtDate(j.finished) : dash),
        h("td", null, j.actor || dash),
        h("td", { class: "act" }, h("div", { class: "actions" }, view)));
    });
    put(histBox, tbl([t("im.colJob"), t("im.colSource"), t("im.colStatus"), t("im.colStarted"), t("im.colFinished"), t("im.colActor"),
      { t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" }], rows, t("im.noJobs")));
  }
  async function loadHistory() {
    try {
      const d = await api("import/jobs");
      if (isLive()) renderHistory(d);
    } catch (e) {
      if (isLive()) handleError(e);
    }
  }
  const histCard = card(t("im.histTitle"), t("im.histSub"), null);
  histCard.append(histBox);
  renderHistory(jobs);

  put(view, head, st.el, msg, errBox, step1Card, step2Card, step3Card, histCard,
    h("div", { class: "notice" }, t("im.slotNote")));
  sync();
  // A job started elsewhere (another tab, the CLI): follow it.
  if (jobs.running) {
    step3Card.hidden = false;
    toStep(2);
    follow(jobs.running.job, "", !!jobs.running.dryRun, progBox);
  }
}

