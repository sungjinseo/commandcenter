// SYSTEM → Backup (plan 14 Decision 5): the online backups under backupDir,
// "Back up now", verify (size, synchronous) and deep verify (a backup.verify
// task run), delete with the typed name, a link to schedule the backup task
// and the offline restore steps. Every name and number comes from the API
// and reaches the page through h()/textContent.
//   #admin/system/backup
import {
  state, h, put, api, encSeg, alive, fmtBytes, fmtNum, fmtDate, pageHead, card, tbl, handleError, icon, markBusy, copyButton,
} from "./util.js";
import { t } from "./i18n.js";
import { modal } from "./shell.js";

let uidN = 0;
const uid = (p) => "bk-" + p + "-" + (++uidN);

// confirmTyped is a second confirmation: Delete stays disabled until the
// user types expect exactly. warn is an extra amber notice above the field.
function confirmTyped(o) {
  return new Promise((resolve) => {
    let ok = false;
    const id = uid("typed");
    const input = h("input", { id, type: "text", autocomplete: "off", spellcheck: "false", autofocus: true });
    const no = h("button", { type: "button", class: "btn" }, t("common.cancel"));
    const yes = h("button", { type: "submit", class: "btn danger", disabled: true, form: id + "-f" }, o.yesLabel || t("common.delete"));
    const form = h("form", { id: id + "-f", novalidate: true },
      h("p", { class: "confirm-text" }, o.text),
      o.warn ? h("div", { class: "notice amber", role: "note" }, o.warn) : null,
      h("div", { class: "field" }, h("label", { for: id }, o.label, " ", h("code", null, o.expect)), input));
    const m = modal(o.title || t("del.title"), form, [no, yes], { cls: "dlg-small", onClose: () => resolve(ok) });
    no.addEventListener("click", m.close);
    input.addEventListener("input", () => { yes.disabled = input.value !== o.expect; });
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      if (input.value !== o.expect) return;
      ok = true;
      m.close();
    });
  });
}

const dash = "—";
const badge = (text, cls) => h("span", { class: "badge" + (cls ? " " + cls : "") }, text);
const RESULT = { OK: ["task.ok", "green"], Failed: ["task.failed", "red"], Stopped: ["task.stopped", "amber"] };

// verifiedBadge is a backup's last verify: never, OK or failed, with its kind and time.
function verifiedBadge(v) {
  if (!v) return badge(t("bk.never"));
  const kind = v.deep ? t("bk.deep") : t("bk.standard");
  return [badge(v.ok ? t("bk.ok") : t("bk.bad"), v.ok ? "green" : "red"),
    h("div", { class: "tiny muted" }, kind + " · " + fmtDate(v.at)),
    !v.ok ? h("div", { class: "tiny muted" }, t("bk.verifyCounts", { missing: fmtNum(v.missing), corrupt: fmtNum(v.corrupt) })) : null];
}

// verifyText sums up a verify result for the status line.
const verifyText = (name, v) => t(v.ok ? "bk.verifyOk" : "bk.verifyBad",
  { name, n: fmtNum(v.blobs), size: fmtBytes(v.bytes), missing: fmtNum(v.missing), corrupt: fmtNum(v.corrupt) });

// restoreCmd is the offline restore one-liner for a backup under dir.
// It is a template literal: TestJSImportsSameOrigin reads a quote right
// after the word from as an import specifier.
const restoreCmd = (dir, name) => `docker compose stop commandcenter && docker compose run --rm commandcenter restore --from ${dir.replace(/\/+$/, "")}/${name} --data /data --force && docker compose start commandcenter`;

export async function viewBackups(view) {
  const isLive = alive();
  let d;
  try {
    d = await api("backups");
  } catch (e) {
    if (e.status !== 501) throw e;
    put(view, pageHead(t("nav.backup"), h("p", null, t("bk.sub")), null, { eyebrow: t("bk.eyebrow") }),
      h("section", { class: "card" }, h("div", { class: "empty" }, t("bk.unavailable"))));
    return;
  }
  if (!isLive()) return;
  // PostgreSQL holds the metadata: the online backup is not available (the server says why).
  if (d.unsupported) {
    put(view, pageHead(t("nav.backup"), h("p", null, t("bk.sub")), null, { eyebrow: t("bk.eyebrow") }),
      h("section", { class: "card" }, h("div", { class: "notice amber", role: "note" }, t("bk.postgres"))));
    return;
  }

  const msg = h("div", { class: "notice", role: "status", hidden: true });
  const say = (text, cls) => {
    msg.className = "notice" + (cls ? " " + cls : "");
    msg.textContent = text;
    msg.hidden = !text;
  };
  const listBox = h("div", null);
  const restoreBox = h("div", null);
  const now = h("button", { type: "button", class: "btn primary" }, t("bk.now"));
  const schedule = h("a", { class: "btn", href: "#admin/system/tasks:create:backup" }, icon("tasks"), t("bk.schedule"));
  let running = ""; // the id of the backup or deep verify being followed
  const rowBtns = [];
  const sync = () => {
    now.disabled = !!running;
    for (const b of rowBtns) b.disabled = !!running || b.dataset.busy === "1";
  };

  let timer = 0;
  state.leaving.push(() => clearTimeout(timer));
  const sleep = (ms) => new Promise((res) => { timer = setTimeout(res, ms); });

  // follow polls a backup or deep verify run (every 1.5 s, 30 minutes at
  // most) until it finishes, then reloads the list.
  async function follow(id, label, kind) {
    running = id;
    sync();
    const unBusy = markBusy("backup");
    say(label, "");
    const until = Date.now() + 1800000;
    let out = null;
    try {
      for (;;) {
        await sleep(1500);
        if (!isLive()) return;
        let x;
        try {
          if (kind === "task") {
            // A scheduled or "run now" backup: the task's state, then its last result.
            const tk = await api("tasks/" + encSeg(id));
            x = tk.status === "Running" ? { status: "Running" } : { status: tk.lastResult, message: tk.lastMessage };
          } else {
            x = await api("backup/runs/" + encSeg(id));
          }
        } catch (e) {
          if (!isLive()) return;
          if (e.status === 404) { out = { status: "", message: t("bk.runGone") }; break; }
          throw e;
        }
        if (!isLive()) return;
        if (x.status !== "Running") { out = x; break; }
        if (Date.now() > until) { say(t("bk.slow"), "amber"); return; }
      }
      const r = RESULT[out.status];
      say(t("bk.runDone", { result: r ? t(r[0]) : out.status || dash }) + (out.message ? " — " + out.message : ""),
        out.status === "OK" ? "ok" : "amber");
    } catch (e) {
      if (isLive()) { say(e.message, "amber"); handleError(e); }
    } finally {
      unBusy();
      if (isLive()) {
        running = "";
        sync();
        await load();
      }
    }
  }

  // start posts a run (a backup or a deep verify) and follows it; a 409
  // names the task that runs instead.
  async function start(path, label) {
    running = "starting";
    sync();
    let id;
    try {
      id = (await api(path, { method: "POST" })).id;
    } catch (e) {
      running = "";
      if (!isLive()) return;
      sync();
      if (e.status === 409) {
        const x = e.data || {};
        say(t("task.conflict", { name: x.runningName || x.running || "?" }), "amber");
      } else {
        say(e.message, "amber");
        handleError(e);
      }
      return;
    }
    if (isLive()) follow(id, label, "run");
  }

  async function verify(b, btn) {
    btn.dataset.busy = "1";
    sync();
    const unBusy = markBusy("verify");
    say(t("bk.verifying", { name: b.name }), "");
    try {
      const v = await api("backups/" + encSeg(b.name) + "/verify", { method: "POST" });
      if (!isLive()) return;
      say(verifyText(b.name, v), v.ok ? "ok" : "amber");
      await load();
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 404) { say(t("bk.gone", { name: b.name }), "amber"); await load(); return; }
      say(e.message, "amber");
      handleError(e);
    } finally {
      unBusy();
      btn.dataset.busy = "";
      if (isLive()) sync();
    }
  }

  async function remove(b, btn) {
    const ok = await confirmTyped({
      title: t("bk.delTitle"), text: t("bk.delText", { name: b.name }), warn: t("bk.delWarn"),
      label: t("bk.delType"), expect: b.name,
    });
    if (!ok || !isLive()) return;
    btn.dataset.busy = "1";
    sync();
    const unBusy = markBusy("backup-delete");
    try {
      const res = await api("backups/" + encSeg(b.name), { method: "DELETE" });
      if (!isLive()) return;
      say(res.sweepSkipped ? t("bk.deletedSkipped", { name: res.name, why: res.sweepSkipped })
        : t("bk.deleted", { name: res.name, n: fmtNum(res.sweptBlobs), size: fmtBytes(res.sweptBytes) }), res.sweepSkipped ? "amber" : "ok");
    } catch (e) {
      if (!isLive()) return;
      if (e.status === 404) say(t("bk.gone", { name: b.name }), "amber");
      else { say(e.message, "amber"); handleError(e); }
    } finally {
      unBusy();
      btn.dataset.busy = "";
    }
    if (isLive()) await load();
  }

  function render(list, dir) {
    rowBtns.length = 0;
    const rows = list.map((b) => {
      const ver = h("button", { type: "button", class: "btn small" }, t("bk.verify"));
      const deep = h("button", { type: "button", class: "btn small" }, t("bk.deepVerify"));
      const del = h("button", { type: "button", class: "btn small danger" }, t("common.delete"));
      ver.addEventListener("click", () => verify(b, ver));
      deep.addEventListener("click", () => start("backups/" + encSeg(b.name) + "/verify?deep=true", t("bk.deepRunning", { name: b.name })));
      del.addEventListener("click", () => remove(b, del));
      const missing = Number(b.missingBlobs) || 0;
      if (b.incomplete) {
        // A directory without a manifest: nothing to verify, only to delete.
        rowBtns.push(del);
        return h("tr", null,
          h("td", { class: "name" }, h("code", null, b.name), h("div", { class: "tiny muted" }, t("bk.incompleteHint"))),
          h("td", { class: "nowrap" }, fmtDate(b.created)),
          h("td", { class: "num" }, fmtBytes(Number(b.metaBytes) || 0)),
          h("td", { class: "num" }, dash),
          h("td", { class: "num" }, dash),
          h("td", null, badge(t("bk.incomplete"), "red")),
          h("td", { class: "act" }, h("div", { class: "actions" }, del)));
      }
      rowBtns.push(ver, deep, del);
      return h("tr", null,
        h("td", { class: "name" }, h("code", null, b.name), b.appVersion ? h("div", { class: "tiny muted" }, b.appVersion) : null),
        h("td", { class: "nowrap" }, fmtDate(b.created)),
        h("td", { class: "num" }, fmtBytes((Number(b.bytes) || 0) + (Number(b.metaBytes) || 0))),
        h("td", { class: "num" }, fmtNum(b.blobs)),
        h("td", { class: "num" }, missing ? badge(fmtNum(missing), "red") : "0"),
        h("td", null, verifiedBadge(b.verified)),
        h("td", { class: "act" }, h("div", { class: "actions" }, ver, deep, del)));
    });
    put(listBox, tbl([t("bk.name"), t("bk.created"), { t: t("col.size"), cls: "num" }, { t: t("bk.blobs"), cls: "num" },
      { t: t("bk.missing"), cls: "num" }, t("bk.verified"), { t: h("span", { class: "sr-only" }, t("col.actions")), cls: "act" }],
    rows, t("bk.none")));
    const cmd = restoreCmd(dir || "/backup", list.length ? list[0].name : "<name>");
    const pre = h("pre", { class: "code" }, cmd);
    put(restoreBox,
      h("ol", { class: "steps" }, h("li", null, t("bk.r1")), h("li", null, t("bk.r2")), h("li", null, t("bk.r3"))),
      h("div", { class: "snippet" }, h("div", { class: "codebar" }, h("span", null, t("bk.restoreCmd")), copyButton(cmd, pre, t("bk.copyCmd"))), pre),
      h("div", { class: "notice" }, t("bk.absNote")),
      h("div", { class: "notice amber" }, t("bk.forceNote")));
    sync();
  }

  async function load() {
    try {
      const x = await api("backups");
      if (!isLive()) return;
      d = x;
      render(d.backups || [], d.dir);
    } catch (e) {
      if (isLive()) handleError(e);
    }
  }

  now.addEventListener("click", () => start("backup", t("bk.nowRunning")));
  const listCard = card(t("bk.listTitle"), t("bk.listSub", { dir: d.dir || "/backup" }), null);
  listCard.append(listBox);
  render(d.backups || [], d.dir);
  put(view,
    pageHead(t("nav.backup"), h("p", null, t("bk.sub")), h("div", { class: "actions" }, schedule, now), { eyebrow: t("bk.eyebrow") }),
    msg,
    listCard,
    card(t("bk.restoreTitle"), t("bk.restoreSub"), restoreBox),
    h("div", { class: "notice" }, t("bk.scheduleNote")));
  // A backup or deep verify started elsewhere: follow it to its end.
  if (d.running) follow(d.running, t("bk.otherRunning"), d.runningKind);
}

