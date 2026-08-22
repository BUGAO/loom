/* loom SPA — hash routing, no build step, no dependencies. */
"use strict";

const $main = document.getElementById("main");
const $overlay = document.getElementById("overlay");

// ---------- utils ----------

const esc = (s) =>
  String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, opts = {}) {
  const res = await fetch("/api" + path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const isJSON = (res.headers.get("content-type") || "").includes("json");
  const data = isJSON ? await res.json() : await res.text();
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// autoGrow makes a textarea track its content height (Claude-style): height
// resets then follows scrollHeight, capped by the element's CSS max-height and
// a JS-side viewport clamp (layout can be mid-settle at page build, so an
// empty box is never measured — its height comes from CSS alone).
// Returns the resize function so callers can re-run it after setting .value.
function autoGrow(ta) {
  const resize = () => {
    if (!ta.value) { ta.style.height = ""; return; }
    ta.style.height = "auto";
    const cap = Math.round((document.documentElement.clientHeight || 720) * 0.4);
    ta.style.height = Math.min(ta.scrollHeight + 2, cap) + "px";
  };
  ta.addEventListener("input", resize);
  resize();
  return resize;
}

// Inline SVG for the attach-image button — emoji glyphs render inconsistently
// across platforms; an outlined icon stays on-theme everywhere.
const ICON_IMG = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="4"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.5-3.5a2 2 0 0 0-2.8 0L6 20"/></svg>`;
// Folder outline for the workspace selector — same stroke language as ICON_IMG,
// and an SVG rather than 📁 for the same reason: emoji glyphs are inconsistent.
const ICON_FOLDER = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>`;

function toast(msg) {
  const el = document.createElement("div");
  el.className = "toast";
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

// modalDialog replaces the browser's native confirm()/prompt() with the app's
// own centered modal. Resolves to null on cancel; to true (or the input's
// value when inputPlaceholder is given) on confirm.
function modalDialog({ title, body, inputPlaceholder, confirmText = "确定", danger = false }) {
  return new Promise((resolve) => {
    const hasInput = inputPlaceholder !== undefined;
    // Own host element, stacked above whatever is already open (including the
    // agent editor, which lives in $overlay) — a dialog must never destroy
    // the surface that asked for it.
    const host = document.createElement("div");
    host.innerHTML = `
      <div class="modal-bg" style="z-index:60">
        <div class="modal" style="width:440px">
          ${title ? `<h2>${esc(title)}</h2>` : ""}
          ${body ? `<div style="font-size:13px;color:var(--muted);line-height:1.65;margin-bottom:14px;word-break:break-word">${esc(body)}</div>` : ""}
          ${hasInput ? `<textarea id="md-input" rows="2" placeholder="${esc(inputPlaceholder)}" style="margin-bottom:14px"></textarea>` : ""}
          <div class="row modal-foot">
            <button id="md-cancel">取消</button>
            <button class="${danger ? "danger" : "primary"}" id="md-ok">${esc(confirmText)}</button>
          </div>
        </div>
      </div>`;
    document.body.appendChild(host);
    const onKey = (e) => { if (e.key === "Escape") done(null); };
    const done = (v) => {
      document.removeEventListener("keydown", onKey);
      host.remove();
      resolve(v);
    };
    document.addEventListener("keydown", onKey);
    host.querySelector("#md-cancel").onclick = () => done(null);
    host.querySelector(".modal-bg").addEventListener("click", (e) => {
      if (e.target.classList.contains("modal-bg")) done(null);
    });
    host.querySelector("#md-ok").onclick = () => {
      const inp = host.querySelector("#md-input");
      done(inp ? inp.value : true);
    };
    (host.querySelector("#md-input") || host.querySelector("#md-ok")).focus();
  });
}

// confirmModal is the yes/no form: resolves to a boolean.
async function confirmModal(title, body, confirmText = "确定") {
  return (await modalDialog({ title, body, confirmText, danger: true })) !== null;
}

const RUN_LABEL = {
  planning: "规划中", awaiting_approval: "待审批", running: "运行中",
  replanning: "重规划", succeeded: "成功", failed: "失败",
  canceled: "已取消", interrupted: "已中断",
};
const NODE_LABEL = {
  pending: "等待", running: "运行中", succeeded: "成功",
  failed: "失败", skipped: "跳过", canceled: "取消",
};
// A2A task lifecycle — deliberately the protocol's own vocabulary.
const TASK_LABEL = {
  submitted: "排队", working: "执行中", "input-required": "待答复",
  completed: "完成", failed: "失败", canceled: "取消",
};
const MSG_LABEL = {
  instruction: "指令", followup: "追加/答复", question: "反问",
  progress: "进度", result: "结果", peer: "同伴消息",
};
const chip = (st, labels = RUN_LABEL) =>
  `<span class="chip ${esc(st)}">${esc(labels[st] || st)}</span>`;

// shortModel compresses a catalog id for badges: claude-opus-5 → opus-5.
const shortModel = (m) => String(m || "").replace(/^claude-/, "");
const modelBadge = (m) => (m ? `<span class="badge mbadge" title="${esc(m)}">${esc(shortModel(m))}</span>` : "");

// Costs are API-list-price equivalents, not a bill — every display of one says
// so, because these runs usually ride a subscription and are billed at zero.
const fmtCost = (c) => (c > 0 ? "$" + c.toFixed(4) : "—");
const fmtTokens = (u) => {
  if (!u) return "—";
  const k = (n) => (n >= 1000 ? (n / 1000).toFixed(1) + "k" : String(n || 0));
  return `in ${k(u.input)} · out ${k(u.output)} · cache ${k(u.cache_write)}/${k(u.cache_read)}`;
};
const fmtDur = (ms) => {
  if (!ms) return "—";
  if (ms < 1000) return ms + "ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
  return Math.floor(ms / 60000) + "m" + Math.round((ms % 60000) / 1000) + "s";
};
const fmtTime = (t) => {
  if (!t || t.startsWith("0001")) return "—";
  const d = new Date(t);
  return d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
};
const runDuration = (r) => {
  const end = r.ended_at && !r.ended_at.startsWith("0001") ? new Date(r.ended_at) : new Date();
  return fmtDur(end - new Date(r.created_at));
};

let meta = {
  models: [{ id: "claude-sonnet-5", label: "Sonnet 5" }],
  default_model: "claude-sonnet-5",
  runtimes: [{ id: "claude", label: "Claude Code(ACP 会话)" }],
  default_runtime: "claude",
  default_dry_run: false,
};
api("/meta").then((m) => (meta = m)).catch(() => {});

// modelSelect renders real model IDs with friendly labels; a stored value not
// in the server's list (legacy alias, custom id) stays selectable at the top.
// worker:true hides coordinator-only tiers — pool agents cap at opus, the top
// tier belongs to the main agent alone.
function modelSelect(id, current, { worker = false } = {}) {
  const models = meta.models.filter((m) => !worker || !m.coordinator_only);
  if (current && !models.some((m) => m.id === current)) {
    models.unshift({ id: current, label: current + "(当前值)" });
  }
  const selected = current || meta.default_model;
  return `<select id="${id}">${models.map((m) =>
    `<option value="${esc(m.id)}" ${m.id === selected ? "selected" : ""}>${esc(m.label)} — ${esc(m.id)}</option>`).join("")}</select>`;
}

// ---------- router ----------

let cleanup = null; // per-page teardown (SSE, timers)

function router() {
  if (cleanup) { cleanup(); cleanup = null; }
  $overlay.innerHTML = "";
  const hash = location.hash || "#/sessions";
  const parts = hash.slice(2).split("/").filter(Boolean); // e.g. ["runs","run_x"]
  let section = parts[0] || "sessions";
  // Workflow editing lives under 设置 now; the old entry points still work.
  if (section === "workflows" && !parts[1]) { location.hash = "#/sessions"; return; }
  const navKey = section === "workflows" ? "settings" : section;
  document.querySelectorAll("[data-nav]").forEach((a) =>
    a.classList.toggle("active", a.dataset.nav === navKey));
  // The conversation page is the one full-bleed surface; everything else
  // keeps the readable 1280px column.
  $main.classList.toggle("wide", section === "sessions");
  if (section === "workflows" && parts[1] === "new") return wfEditPage(null);
  if (section === "workflows" && parts[2] === "edit") return wfEditPage(parts[1]);
  if (section === "sessions") return sessionsPage();
  if (section === "settings") return settingsPage(parts[1] || "main");
  if (section === "runs" && parts[1] && parts[2] === "topology") return topologyPage(parts[1]);
  if (section === "runs" && parts[1]) return runPage(parts[1]);
  if (section === "runs") return runsListPage();
  if (section === "agents") return agentsPage();
  sessionsPage();
}
window.addEventListener("hashchange", router);

// openSession jumps to the workflow page with a specific session selected —
// the bridge from run records back into the conversation.
function openSession(wfId, runId) {
  sessionStorage.setItem("wfSes", runId);
  location.hash = "#/sessions";
}

// ---------- settings: main agent / templates / general ----------

const settingsTabs = (tab) => `
  <div class="settings-tabs">
    <a href="#/settings" class="${tab === "main" ? "on" : ""}">主 agent</a>
    <a href="#/settings/templates" class="${tab === "templates" ? "on" : ""}">静态模板</a>
    <a href="#/settings/general" class="${tab === "general" ? "on" : ""}">通用</a>
  </div>`;

async function settingsPage(tab) {
  if (tab === "main") {
    const main = await api("/main");
    return wfEditPage(main.id, { prefix: settingsTabs("main"), back: "#/settings" });
  }
  if (tab === "templates") {
    const wfs = (await api("/workflows")).filter((w) => w.mode !== "dynamic");
    $main.innerHTML = settingsTabs("templates") + `
      <div class="page-head">
        <h1>静态模板</h1>
        <button class="primary" onclick="location.hash='#/workflows/new'">+ 新建模板</button>
      </div>
      <div class="muted" style="font-size:12.5px;margin-bottom:12px">每个模板是一个 planner + 确定性执行的 DAG。main agent 在会话里可用 run_template 把它当一个任务跑;这里也可以直接运行(产生一条静态记录)。</div>
      <div id="tpl-list">${wfs.map((w) => `
        <div class="tpl-card">
          <div class="tpl-main">
            <div><b>${esc(w.name)}</b> <span class="mono muted" style="font-size:11px">${esc(w.id)}</span></div>
            <div class="tpl-desc">${esc(w.description || "")}</div>
          </div>
          <button class="small" data-run="${esc(w.id)}" title="用这个模板直接发起一次静态运行">运行</button>
          <button class="small" onclick="location.hash='#/workflows/${esc(w.id)}/edit'">编辑</button>
        </div>`).join("") || '<div class="empty">还没有模板</div>'}</div>`;
    $main.querySelectorAll("[data-run]").forEach((b) => b.addEventListener("click", async () => {
      const wf = wfs.find((w) => w.id === b.dataset.run);
      const goal = await modalDialog({ title: `运行模板「${wf.name}」`, body: "目标会交给模板的 planner 组装 DAG;工作区用默认工作区(~/workflow-output)。",
        inputPlaceholder: "写下这次运行的目标…", confirmText: "运行" });
      if (goal === null || !goal.trim()) return;
      try {
        const run = await api(`/workflows/${wf.id}/runs`, { method: "POST", body: { goal, dry_run: !!meta.default_dry_run } });
        toast("已发起静态运行");
        location.hash = "#/runs/" + run.id;
      } catch (e) { toast(e.message); }
    }));
    return;
  }
  // general
  let ws = {};
  try { ws = await api("/workspaces"); } catch {}
  $main.innerHTML = settingsTabs("general") + `
    <div class="page-head"><h1>通用</h1></div>
    <div class="panel" style="max-width:760px">
      <label class="field"><span>默认工作区 — 新会话不选工作区时用它(启动参数 --output / LOOM_OUTPUT)</span>
        <input value="${esc(ws.default || "")}" readonly></label>
      <label class="field"><span>运行时</span>
        <input value="${esc((meta.runtimes || []).find((r) => r.id === meta.default_runtime)?.label || meta.default_runtime || "")}" readonly></label>
      <label class="field"><span>默认演示模式(dry run)</span>
        <input value="${meta.default_dry_run ? "开" : "关"}" readonly></label>
      <div class="muted" style="font-size:12px">这些来自服务进程的启动参数;改它们请用 <span class="mono">loom start --help</span>。</div>
    </div>`;
}

// ---------- sessions: one area, one main agent ----------

// The workflow page is a conversation surface: pick a workflow on the left,
// talk to its main agent on the right. The main agent decomposes, delegates,
// and reports back; the runtime status above the chat is the live task tree.
async function sessionsPage() {
  // ONE session area: every session is a conversation with THE main agent
  // (the single dynamic configuration, GET /api/main). Static workflows are
  // templates the main agent runs as tasks; they live under 设置.
  const mainWf = await api("/main");
  const wfs = [mainWf];
  let selId = mainWf.id;
  let sessions = []; // the selected workflow's runs, newest first — each IS a session
  let sesId = null;  // selected session; null = compose a new one
  let run = null;    // the selected session, fully loaded
  let es = null;
  let threadId = null; // task whose thread panel is open; null = closed

  // ---- render cache & disclosure state ----
  // The server pushes a full snapshot per streamed delta. Rebuilding the whole
  // DOM per snapshot janks the stream and resets every <details> the user
  // collapsed, so each region only re-renders when its HTML actually changed,
  // and manual fold state survives re-renders.
  let traceOpen = true;    // 「本轮动作」fold state
  let evidenceOpen = null; // 「完成判据」; null = follow default (open while live)
  let lastStatusHtml = null, lastMsgsHtml = null, lastTailHtml = null;
  let lastChatRunId = null, lastThreadKey = null;
  let renderQueued = false;

  // ---- workspace selector state ----
  // The directory the NEXT run works in and delivers to — project and output
  // are one folder. A run freezes its workspace at birth, so this is a draft
  // for the session being composed; a live dynamic session shows its own
  // run.workspace instead, read-only.
  let selectedWorkspace = ""; // "" = the default workspace (wsDefault)
  let wsDefault = "";         // the server's default workspace (~/workflow-output)
  let wsList = [];            // recent workspaces from the server, newest first
  let wsHome = "";            // the user's home dir, for ~-shortening and browsing
  let wsOpen = false;         // dropdown visibility
  let wsBrowsePath = "";      // the folder browser's current directory
  let wsBrowseDirs = [];      // the folder browser's current listing

  const terminalRun = (r) => ["succeeded", "failed", "canceled", "interrupted"].includes(r.status);
  const selWf = () => wfs.find((w) => w.id === selId);
  // Sessions are dynamic runs — of any dynamic workflow, so history from
  // before the single-config UI stays reachable. Template sub-runs (static)
  // belong to 记录, not here.
  const fetchSessions = async () => (await api("/runs")).filter((r) => r.mode === "dynamic");
  const sesKey = () => "wfSes";
  const wsKey = () => "wfWs:" + selId; // per-workflow draft, survives a reload

  const loadRun = async () => {
    run = null;
    sessions = [];
    threadId = null;
    if (!selId) return;
    try {
      sessions = await fetchSessions(); // newest first
    } catch { /* no runs yet */ }
    const stored = sessionStorage.getItem(sesKey());
    if (stored === "new") { sesId = null; return; }
    const pick = sessions.find((r) => r.id === stored)
      || sessions.find((r) => !terminalRun(r))
      || sessions[0];
    sesId = pick ? pick.id : null;
    if (pick) {
      try { run = await api("/runs/" + pick.id); } catch { run = null; }
    }
  };

  const resub = () => {
    if (es) { es.close(); es = null; }
    if (!run) return;
    // The server closes the stream for a finished run, and EventSource would
    // reconnect every few seconds forever. Nothing left to follow anyway.
    if (terminalRun(run)) return;
    es = new EventSource(`/api/runs/${run.id}/events`);
    es.onmessage = (m) => {
      run = JSON.parse(m.data);
      const s = sessions.find((x) => x.id === run.id);
      if (s && s.status !== run.status) { s.status = run.status; renderSessions(); }
      scheduleRender();
    };
  };
  // Deltas can arrive faster than the display refreshes; coalescing them into
  // one render per animation frame keeps the stream smooth. rAF never fires
  // while the page is hidden, so a timer keeps the DOM current in background.
  const scheduleRender = () => {
    if (renderQueued) return;
    renderQueued = true;
    const flush = () => { renderQueued = false; renderRight(); };
    if (document.hidden) setTimeout(flush, 250);
    else requestAnimationFrame(flush);
  };
  // Outside click closes the workspace dropdown. Registered once for the page
  // and torn down with the stream, so leaving the page leaves nothing behind.
  const onDocClick = (e) => { if (wsOpen && !e.target.closest("#wf-ws-wrap")) closeWsMenu(); };
  cleanup = () => {
    if (es) es.close();
    document.removeEventListener("click", onDocClick);
  };

  const renderLeft = () => renderSessions();

  const renderHead = () => {
    const head = $main.querySelector("#wf-head");
    const wf = selWf();
    if (!head) return;
    if (!wf) { head.innerHTML = ""; return; }
    head.innerHTML = `
      <h2 style="margin:0">main agent</h2>
      <span class="badge" title="主 agent 的配置在「设置 › 主 agent」">${esc(wf.name)}</span>
      <span style="flex:1"></span>
      <button class="small" id="wf-new-session" title="开始一个全新会话(新 run)">+ 新会话</button>
      <button class="small" id="wf-feedback" title="复盘记录与行为规范:待确认的规范在这里批;已确认的注入之后每次 run">复盘</button>
      <button class="small" onclick="location.hash='#/settings'">设置</button>
      <a class="btn small" href="#/runs">记录</a>`;
    const nb = head.querySelector("#wf-new-session");
    if (nb) nb.addEventListener("click", () => {
      sesId = null;
      run = null;
      threadId = null;
      sessionStorage.setItem(sesKey(), "new");
      renderSessions(); renderRight(); resub();
    });
    const fbBtn = head.querySelector("#wf-feedback");
    fbBtn.addEventListener("click", () => feedbackModal(wf));
    // Badge: pending rules need the user's decision — that's the number that
    // matters here, not how many retrospectives exist.
    api("/lessons?workflow_id=" + wf.id).then((ls) => {
      const pending = ls.filter((l) => l.status === "pending").length;
      const approved = ls.filter((l) => l.status === "approved").length;
      if (pending) fbBtn.innerHTML = `复盘 · <b style="color:var(--accent)">待确认 ${pending}</b>`;
      else if (approved) fbBtn.textContent = `复盘 (${approved})`;
    }).catch(() => {});
  };

  // Sessions are first-class and visible: one chip per run, click to continue
  // the conversation (finished ones get reopened by the next message), × to
  // delete it for good.
  const renderSessions = () => {
    const box = $main.querySelector("#wf-list");
    if (!box) return;
    if (!sessions.length) { box.innerHTML = '<div class="empty">还没有会话 — 在右边说出你的目标就开始了</div>'; return; }
    const when = (r) => new Date(r.created_at).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
    const goal = (r) => (r.goal || "").replace(/\s+/g, " ").slice(0, 60);
    box.innerHTML = sessions.map((r) => `
      <div class="ses-item ${esc(r.status)} ${r.id === sesId ? "selected" : ""}" data-ses="${esc(r.id)}"
           title="${esc(r.goal || "")} · ${esc(RUN_LABEL[r.status] || r.status)}">
        <div class="ses-row"><span class="tdot"></span><span class="ses-goal">${esc(goal(r)) || "(无标题)"}</span>
          <span class="ses-x" data-del="${esc(r.id)}" title="删除该会话(含其任务台账与产物记录)">×</span></div>
        <div class="ses-meta muted">${when(r)} · ${esc(RUN_LABEL[r.status] || r.status)}${r.level ? " · " + esc(r.level) : ""}${r.workspace ? " · " + esc(wsBase(r.workspace)) : ""}</div>
      </div>`).join("");
    box.querySelectorAll("[data-ses]").forEach((chip) =>
      chip.addEventListener("click", async (e) => {
        if (e.target.dataset.del) return;
        sesId = chip.dataset.ses;
        sessionStorage.setItem(sesKey(), sesId);
        run = null;
        threadId = null;
        try { run = await api("/runs/" + sesId); } catch {}
        renderSessions(); renderRight(); resub();
      }));
    box.querySelectorAll("[data-del]").forEach((x) =>
      x.addEventListener("click", async (e) => {
        e.stopPropagation();
        const id = x.dataset.del;
        const ses = sessions.find((r) => r.id === id);
        if (!(await confirmModal("删除会话",
          `「${(ses?.goal || id).slice(0, 40)}」的对话、任务台账与记录将被移除,不可恢复。`, "删除"))) return;
        try {
          await api("/runs/" + id, { method: "DELETE" });
          sessions = sessions.filter((r) => r.id !== id);
          if (sesId === id) {
            sesId = sessions[0]?.id || null;
            sessionStorage.setItem(sesKey(), sesId || "new");
            run = null;
            if (sesId) { try { run = await api("/runs/" + sesId); } catch {} }
            resub();
          }
          renderSessions(); renderRight();
        } catch (err) { toast("删除失败:" + err.message); }
      }));
  };

  // The run's collaboration level — what the main agent may do with its own
  // hands — with the user's override. Enforced by the engine's tool gate; a
  // change takes effect at the main agent's next tool call.
  const LEVEL_LABEL = { solo: "solo · 自己动手", pair: "pair · 动手 + 常驻伙伴", orchestrate: "orchestrate · 只派单" };
  const levelControl = (run) => {
    const lvl = run.level || "orchestrate";
    const src = run.level_source ? `(${{ user: "你设定", workflow: "工作流钉死", default: "默认", triage: "triage 判定" }[run.level_source] || run.level_source})` : "";
    if (terminalRun(run)) return `<span class="badge lvl ${esc(lvl)}" title="level ${src}">${esc(LEVEL_LABEL[lvl] || lvl)}</span>`;
    return `<select class="lvl-sel ${esc(lvl)}" data-level title="level ${src} — 改它 = 覆盖引擎判定;main agent 的下一次工具调用起生效">
      ${["solo", "pair", "orchestrate"].map((l) => `<option value="${l}" ${l === lvl ? "selected" : ""}>${esc(LEVEL_LABEL[l])}</option>`).join("")}
    </select>`;
  };

  // Status zone: the selected run's live shape, compact. Deep inspection stays
  // on the run page — every element here links into it.
  const renderStatus = () => {
    if (!run) return '<div class="empty" style="padding:18px">新会话 — 在下面对 main agent 说出你的目标,它会拆解并派发 agent 开始干活。会话保存在本地,随时可以回来继续。</div>';
    const dyn = run.mode === "dynamic";
    const header = `
      <div class="wf-run-head">
        <a href="#/runs/${esc(run.id)}" class="mono" style="font-size:11px">${esc(run.id)}</a>
        ${chip(run.status)}
        ${run.coordinator?.rounds ? `<span class="badge">第 ${run.coordinator.rounds} 轮</span>` : ""}
        ${dyn ? levelControl(run) : ""}
        <span class="badge" title="按 API 牌价折算">est. ${fmtCost(run.cost_usd)}</span>
        <span class="badge">${runDuration(run)}</span>
        ${terminalRun(run) ? "" : '<button class="small danger" data-act="cancel">取消</button>'}
        ${run.status === "interrupted" && dyn ? '<button class="small primary" data-act="resume">▶ 恢复</button>' : ""}
        ${run.workspace || run.output_dir ? `<span class="mono" style="font-size:10.5px;color:var(--muted)" title="工作区(删除会话不影响它)">📁 ${esc(run.workspace || run.output_dir)}</span>` : ""}
      </div>`;
    if (dyn) {
      const tasks = (run.task_order || []).map((id) => run.tasks[id]).filter(Boolean);
      const rows = tasks.map((t) => `
        <div class="wf-task ${esc(t.status)}" data-thread="${esc(t.id)}" title="打开该任务的 thread">
          <span class="tdot"></span>
          <span class="ttitle">${esc(t.title || t.id)}</span>
          <span class="tmeta"><span class="mono">${esc(t.agent)}</span>
            ${modelBadge(t.model)}
            <span>${t.status === "working" && t.activity ? "⚙ " + esc(t.activity) : esc(TASK_LABEL[t.status] || t.status)}</span>
            ${t.failure_kind ? `<span class="mono" style="color:var(--bad)">${esc(t.failure_kind)}</span>` : ""}
          </span>
        </div>`).join("");
      return header + renderEvidence(run) + (rows ? `<div class="wf-tasks">${rows}</div>` : '<div class="muted" style="font-size:12px;padding:6px 2px">main agent 还没有派发任务…</div>');
    }
    const nodes = run.plan?.nodes || [];
    const rows = nodes.map((n) => {
      const ns = run.nodes[n.id] || {};
      return `
        <a class="wf-task ${esc(ns.status || "pending")}" href="#/runs/${esc(run.id)}">
          <span class="tdot"></span>
          <span class="ttitle">${esc(n.title || n.id)}</span>
          <span class="tmeta"><span class="mono">${esc(n.agent)}</span><span>${esc(NODE_LABEL[ns.status] || ns.status || "")}</span></span>
        </a>`;
    }).join("");
    return header + (rows ? `<div class="wf-tasks">${rows}</div>` : '<div class="muted" style="font-size:12px;padding:6px 2px">planner 正在组装 DAG…</div>');
  };

  // The definition of done: the proofs the main agent declared up front, and
  // how each was settled at finish. Shown compactly above the task list — this
  // is what "succeeded" means for this session, so it belongs in view.
  const renderEvidence = (run) => {
    const ev = run.evidence || [];
    if (!ev.length) return "";
    const met = ev.filter((e) => e.met).length;
    const items = ev.map((e) => `
      <div class="ev-item ${e.met ? "met" : ""}" title="${esc(e.met ? "已验证:" + (e.how || "") : (e.needs_from_user ? "需要你提供:" + e.needs_from_user : "未验证"))}">
        <span class="ev-dot">${e.met ? "✓" : "○"}</span>
        <span class="ev-claim">${esc(e.claim)}</span>
        ${e.needs_from_user && !e.met ? `<span class="ev-need">需你提供:${esc(e.needs_from_user)}</span>` : ""}
        ${e.met && e.how ? `<span class="ev-how">${esc(e.how)}</span>` : ""}
      </div>`).join("");
    return `<details class="wf-evidence" ${(evidenceOpen ?? !terminalRun(run)) ? "open" : ""}>
      <summary>完成判据 <span class="badge">${met}/${ev.length} 已验证</span></summary>
      ${items}
    </details>`;
  };

  // A dispatch card is one delegation shown inline in the conversation — the
  // thread root, Lark-style: click it to open the coordinator ↔ worker thread.
  const dispatchCard = (t) => {
    const who = t.agent?.startsWith("template:") ? "main agent · 运行模板"
      : t.created_by === "coordinator" ? "main agent · 派发任务"
      : t.created_by === "external" ? "外部 · 提交任务"
      : `任务 ${esc(String(t.created_by).slice(-8))} · 交接子任务`;
    const sub = t.status === "working" && t.activity
      ? `<span class="tact">⚙ ${esc(t.activity)}</span>`
      : `<span>${esc(TASK_LABEL[t.status] || t.status)}</span>`;
    const n = (t.messages || []).length;
    return `
      <div class="cmsg agent">
        <div class="cwho">${who} · ${fmtTime(t.created_at)}</div>
        <div class="dispatch ${esc(t.status)} ${threadId === t.id ? "open" : ""}" data-thread="${esc(t.id)}"
             title="打开 thread,查看 main agent 与 worker 的完整往来">
          <span class="tdot"></span>
          <div class="dmain">
            <div class="dtitle">${esc(t.title || t.id)}</div>
            <div class="dmeta">
              <span class="mono">${esc(t.agent)}</span>
              ${modelBadge(t.model)}
              ${sub}
              ${t.failure_kind ? `<span class="mono" style="color:var(--bad)">${esc(t.failure_kind)}</span>` : ""}
            </div>
          </div>
          <span class="dthread">${t.sub_run_id ? `<a href="#/runs/${esc(t.sub_run_id)}" title="打开模板子运行(静态 DAG)" onclick="event.stopPropagation()">⧉ 子运行 ›</a>` : `💬 ${n} ›`}</span>
        </div>
      </div>`;
  };

  // System cards in the conversation: a triage verdict (the level the engine
  // chose for the task just assessed, and why — change it with the level
  // control in the header), or an engine notice (e.g. the listener failing).
  const systemCard = (m) => {
    if (m.kind === "triage") {
      const [head, ...rest] = String(m.text).split("\n");
      const lvl = head.split(" ")[0];
      return `
      <div class="cmsg agent sys">
        <div class="cwho">triage · ${fmtTime(m.ts)}</div>
        <div class="cbody triage-card">
          <span class="badge lvl ${esc(lvl)}">${esc(LEVEL_LABEL[lvl] || lvl)}</span> ${esc(head.slice(lvl.length + 3))}
          ${rest.length ? `<div class="muted" style="font-size:11.5px;margin-top:4px">${esc(rest.join(" "))}</div>` : ""}
        </div>
      </div>`;
    }
    return `
      <div class="cmsg agent sys">
        <div class="cwho">系统 · ${fmtTime(m.ts)}</div>
        <div class="cbody" style="color:var(--warn)">${esc(m.text)}</div>
      </div>`;
  };

  // Chat zone: the conversation with the main agent, with every delegation
  // shown inline as a thread root at the moment it happened, plus decision
  // moments (approval, verdict) where they occur.
  // Returns {msgs, tail}: the settled history and the live tail (draft,
  // approval, feedback box). During streaming only the tail changes, so the
  // caller can leave the heavy history DOM alone.
  const renderChat = () => {
    if (!run) return { msgs: "", tail: "" };
    const chatImgs = (m) => (m.images || []).map((n) =>
      `<a href="/api/runs/${esc(run.id)}/uploads/${esc(n)}" target="_blank" rel="noopener">
         <img class="cimg" src="/api/runs/${esc(run.id)}/uploads/${esc(n)}" alt="${esc(n)}" loading="lazy"></a>`).join("");
    const items = (run.chat || []).map((m) => ({
      ts: m.ts,
      html: m.from === "system" ? systemCard(m) : `
      <div class="cmsg ${m.from === "user" ? "me" : "agent"}">
        <div class="cwho">${m.from === "user" ? (m.kind === "feedback" ? "你 · 复盘反馈" : m.kind === "consolidate" ? "你 · 规范整理" : "你") : "main agent"} · ${fmtTime(m.ts)}</div>
        <div class="cbody">${esc(m.text)}${m.images?.length ? `<div class="cimgs">${chatImgs(m)}</div>` : ""}</div>
      </div>`,
    }));
    if (run.mode === "dynamic") {
      (run.task_order || []).map((id) => run.tasks[id]).filter(Boolean).forEach((t) =>
        items.push({ ts: t.created_at, html: dispatchCard(t) }));
    }
    // Stable merge by timestamp: a dispatch lands right after the round reply
    // that announced it.
    items.sort((a, b) => new Date(a.ts) - new Date(b.ts));
    const msgs = items.map((i) => i.html).join("");
    let tail = "";
    if (terminalRun(run) && run.mode === "dynamic") {
      const label = run.status === "succeeded" ? "已交付" : RUN_LABEL[run.status] || run.status;
      tail = `<div class="cmsg agent"><div class="cwho">系统</div><div class="cbody" style="color:var(--muted)">会话${label}${run.status === "failed" && run.error ? ":" + esc(run.error) : ""}。继续发消息会在同一会话唤醒 main agent 接着做。</div></div>`;
      // The feedback loop's landing point. Real dynamic runs digest feedback
      // CONVERSATIONALLY: submitting wakes the coordinator in postmortem mode.
      // Its conclusion is a RECORD (never injected); the behavior rules it
      // distills wait in the 复盘 panel for the user's confirmation — only
      // confirmed rules ride into future runs. Dry runs have a scripted
      // coordinator, so they keep the verbatim record.
      const dryish = run.dry_run || run.backend === "mock";
      const distilled = run.feedback
        ? `<div style="font-size:12.5px;margin-bottom:6px">复盘记录(仅存档,不注入):<i>${esc(run.feedback)}</i></div>` : "";
      tail += dryish ? `
        <div class="cmsg agent"><div class="cwho">复盘反馈${run.feedback ? " · 已记录 ✓" : ""}</div><div class="cbody">
          <div class="muted" style="font-size:12px;margin-bottom:6px">演示模式没有真的 main agent:反馈原文保存为复盘记录,不注入。要注入之后 run 的行为规范,在 workflow 的「复盘」面板手动添加。</div>
          <textarea id="fb-input" rows="2" placeholder="例:报告把结论埋在最后——下次先给结论" style="width:100%;resize:vertical">${esc(run.feedback || "")}</textarea>
          <div class="row" style="margin-top:6px"><button class="small primary" data-act="feedback">保存反馈</button></div>
        </div></div>` : `
        <div class="cmsg agent"><div class="cwho">复盘反馈</div><div class="cbody">
          ${distilled}
          <div class="muted" style="font-size:12px;margin-bottom:6px">提交后唤醒 main agent 消化这条反馈:指代不清它会反问;值得沉淀的进项目记忆或修订提案;复盘结论只存档,从中提炼的行为规范会等你在「复盘」面板逐条确认——确认后才注入之后的 run,事件经过不会被注入。</div>
          <textarea id="fb-input" rows="2" placeholder="例:报告把结论埋在最后——下次先给结论" style="width:100%;resize:vertical"></textarea>
          <div class="row" style="margin-top:6px"><button class="small primary" data-act="feedback">发起复盘</button></div>
        </div></div>`;
    }
    if (run.status === "awaiting_approval" && run.proposal) {
      tail = `
        <div class="cmsg agent">
          <div class="cwho">main agent · 计划待批准</div>
          <div class="cbody">
            ${esc(run.proposal.summary || "")}
            <ol style="margin:8px 0 0 18px">${(run.proposal.tasks || []).map((t) =>
              `<li>${esc(t.title || "")} → <span class="mono">${esc(t.agent)}</span>${modelBadge(t.model)}</li>`).join("")}</ol>
            <div class="row" style="margin-top:10px">
              <button class="small primary" data-act="approve">✓ 批准</button>
              <button class="small danger" data-act="reject">✗ 拒绝</button>
            </div>
          </div>
        </div>`;
    } else if (!terminalRun(run) && run.mode === "dynamic") {
      const c = run.coordinator || {};
      const trace = (c.trace || []).length
        ? `<details class="ctrace"${traceOpen ? " open" : ""}><summary>⚙ 本轮动作 ${c.trace.length}</summary>${c.trace.map((l) => `<div class="ctrace-line mono">${esc(l)}</div>`).join("")}</details>` : "";
      tail = c.status === "awaiting_user"
        ? '<div class="cmsg agent typing"><div class="cbody" style="color:var(--warn);font-style:normal">❓ main agent 在等你回答上面的问题——直接在下面输入即可</div></div>'
        : c.draft
          ? `<div class="cmsg agent"><div class="cwho">main agent · 正在输入…</div><div class="cbody">${esc(c.draft)}<span class="cursor">▍</span></div>${trace}</div>`
          : `<div class="cmsg agent typing"><div class="cbody">⋯ main agent 工作中(${esc(c.activity || "等待任务推进")})</div>${trace}</div>`;
    }
    return { msgs, tail };
  };

  const renderRight = () => {
    const st = $main.querySelector("#wf-status");
    const log = $main.querySelector("#wf-chat-log");
    if (!st || !log) return;
    const openThread = (el) => el.addEventListener("click", () => {
      threadId = threadId === el.dataset.thread ? null : el.dataset.thread;
      renderRight();
    });
    // Listeners are (re)wired only when a region's DOM was actually rebuilt —
    // untouched DOM keeps its old listeners, so wiring again would double them.
    const wire = (root) => {
      root.querySelectorAll("[data-act]").forEach(wireAct);
      root.querySelectorAll("[data-thread]").forEach(openThread);
    };
    // Fold state is captured from the live DOM right before rendering — the
    // user's click lands on the DOM synchronously, so this can't lose a
    // toggle to an already-queued render the way a toggle listener could.
    const liveTrace = log.querySelector("details.ctrace");
    if (liveTrace) traceOpen = liveTrace.open;
    const liveEv = st.querySelector("details.wf-evidence");
    if (liveEv) evidenceOpen = liveEv.open;
    const stHtml = renderStatus();
    if (stHtml !== lastStatusHtml) {
      lastStatusHtml = stHtml;
      st.innerHTML = stHtml;
      wire(st);
      st.querySelectorAll("[data-level]").forEach((sel) => sel.addEventListener("change", async () => {
        try {
          run = await api(`/runs/${run.id}/level`, { method: "POST", body: { level: sel.value } });
          toast("level → " + sel.value);
          renderRight();
        } catch (e) { toast(e.message); renderRight(); }
      }));
    }
    // Chat log is split into settled history and live tail (display: contents,
    // so both stay transparent to the flex layout). Streaming only touches the
    // tail; the history DOM — the expensive part — is left alone.
    if ((run?.id || "") !== lastChatRunId || !log.querySelector("#wf-chat-msgs")) {
      lastChatRunId = run?.id || "";
      lastMsgsHtml = lastTailHtml = null;
      log.innerHTML = '<div id="wf-chat-msgs"></div><div id="wf-chat-tail"></div>';
    }
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
    const { msgs, tail } = renderChat();
    if (msgs !== lastMsgsHtml) {
      lastMsgsHtml = msgs;
      const el = log.querySelector("#wf-chat-msgs");
      el.innerHTML = msgs;
      wire(el);
    }
    if (tail !== lastTailHtml) {
      lastTailHtml = tail;
      const el = log.querySelector("#wf-chat-tail");
      const prevFb = el.querySelector("#fb-input")?.value; // survive re-renders
      el.innerHTML = tail;
      if (prevFb !== undefined) {
        const fb = el.querySelector("#fb-input");
        if (fb) fb.value = prevFb;
      }
      wire(el);
    }
    if (atBottom) log.scrollTop = log.scrollHeight;
    // Dry-run is a property fixed at session birth; the toggle only applies
    // to a session being composed, so it only shows then.
    const dryWrap = $main.querySelector("#wf-dry-wrap");
    if (dryWrap) dryWrap.style.display = !run ? "" : "none";
    renderWsBar(); // locked ↔ editable follows the same rule as the dry-run toggle
    renderThread();
  };

  // Thread panel — the Lark-style side sheet over the conversation: one task's
  // full coordinator ↔ worker message history, live, with an input to speak
  // into the task (same audited channel as the coordinator's send_message).
  const renderThread = () => {
    const panel = $main.querySelector("#wf-thread");
    if (!panel) return;
    const t = threadId && run && run.tasks ? run.tasks[threadId] : null;
    if (!t) {
      threadId = null;
      lastThreadKey = null;
      panel.style.display = "none";
      panel.innerHTML = "";
      return;
    }
    // Re-rendered on every SSE push: preserve the half-typed reply and the
    // reading position (unless pinned to the bottom, where new messages land).
    const prevInput = panel.querySelector("#th-input")?.value ?? "";
    const prevList = panel.querySelector(".th-msgs");
    const atBottom = !prevList || prevList.scrollHeight - prevList.scrollTop - prevList.clientHeight < 60;
    const prevScroll = prevList ? prevList.scrollTop : 0;
    const live = !["completed", "failed", "canceled"].includes(t.status);
    const fromLabel = (m) =>
      m.from === "coordinator" ? "main agent"
        : m.from === t.id ? t.agent
        : m.from === "external" ? "外部"
        : m.from === "user" ? "你"
        : `任务 ${String(m.from).slice(-8)}`;
    panel.style.display = "";
    const html = `
      <div class="th-head">
        <div class="th-title">
          <b>${esc(t.title || t.id)}</b>
          <div class="th-sub">
            <span class="mono">${esc(t.agent)}</span>
            ${modelBadge(t.model)}
            ${chip(t.status, TASK_LABEL)}
            ${t.turns > 1 ? `<span>${t.turns} 轮</span>` : ""}
            ${t.cost_usd ? `<span title="按 API 牌价折算">est. ${fmtCost(t.cost_usd)}</span>` : ""}
          </div>
        </div>
        <a class="btn small" href="#/runs/${esc(run.id)}" title="到运行详情页看完整输出与验收契约">详情</a>
        <button class="small" id="th-close" title="关闭 thread">✕</button>
      </div>
      <div class="th-msgs">
        ${(t.messages || []).map((m) => `
          <div class="msg ${esc(m.role)}">
            <div class="who"><b>${esc(MSG_LABEL[m.role] || m.role)}</b>
              <span>${esc(fromLabel(m))}</span><span>${fmtTime(m.ts)}</span></div>
            <div class="body">${esc(m.text)}</div>
          </div>`).join("") || '<div class="muted" style="font-size:12px;text-align:center;padding:14px 0">还没有消息</div>'}
        ${t.status === "working" && t.activity ? `<div class="th-live">⚙ ${esc(t.activity)}</div>` : ""}
        ${t.error ? `<div class="msg" style="border-color:rgba(251,113,133,.5)"><div class="who"><b>错误</b>${t.failure_kind ? `<span class="mono">${esc(t.failure_kind)}</span>` : ""}</div><div class="body" style="color:var(--bad)">${esc(t.error)}</div></div>` : ""}
        ${t.artifacts?.length ? `<div class="msg result"><div class="who"><b>产物</b></div><div class="body mono" style="font-size:11.5px">${t.artifacts.map(esc).join("<br>")}</div></div>` : ""}
      </div>
      ${live ? `
      <div class="th-input">
        <textarea id="th-input" rows="1" placeholder="${t.status === "input-required" ? "回答 worker 的反问…" : "对该任务插话或调整方向…"}(Enter 发送)"></textarea>
        <button class="primary small" id="th-send">发送</button>
      </div>` : `<div class="th-done muted">任务已${esc(TASK_LABEL[t.status] || t.status)},thread 只读</div>`}`;
    // Unchanged content: leave the DOM alone — input, scroll position and
    // listeners all survive for free.
    const key = t.id + " " + html;
    if (key === lastThreadKey) return;
    lastThreadKey = key;
    panel.innerHTML = html;
    const list = panel.querySelector(".th-msgs");
    list.scrollTop = atBottom ? list.scrollHeight : prevScroll;
    panel.querySelector("#th-close").onclick = () => { threadId = null; renderRight(); };
    const input = panel.querySelector("#th-input");
    if (input) {
      input.value = prevInput;
      const grow = autoGrow(input);
      const sendThread = async () => {
        const text = input.value.trim();
        if (!text) return;
        try {
          await api(`/runs/${run.id}/tasks/${t.id}/message`, { method: "POST", body: { text } });
          input.value = "";
          grow();
        } catch (e) { toast("发送失败:" + e.message); }
      };
      panel.querySelector("#th-send").onclick = sendThread;
      input.addEventListener("keydown", (e) => {
        if (e.isComposing || e.keyCode === 229) return;
        if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendThread(); }
      });
    }
  };

  const wireAct = (btn) => btn.addEventListener("click", async () => {
    const act = btn.dataset.act;
    let body = {};
    if (act === "feedback") {
      const dryish = run.dry_run || run.backend === "mock";
      const text = $main.querySelector("#fb-input")?.value ?? "";
      if (!dryish && !text.trim()) return toast("请先写下反馈内容");
      try {
        run = await api(`/runs/${run.id}/feedback`, { method: "POST", body: { text } });
        if (dryish) {
          toast("反馈已保存为复盘记录(不注入)");
        } else {
          toast("已发起复盘,main agent 正在消化;提炼出的规范会等你在「复盘」面板确认");
          resub(); // the session just reactivated; follow it live
        }
        renderRight();
      } catch (e) { toast(e.message); }
      return;
    }
    if (act === "reject") {
      const reason = await modalDialog({
        title: "拒绝该计划", inputPlaceholder: "拒绝理由(会告知 main agent)…",
        confirmText: "拒绝", danger: true,
      });
      if (reason === null) return;
      body = { reason };
    }
    try {
      await api(`/runs/${run.id}/${act}`, { method: "POST", body });
      if (act === "resume") { await loadRun(); resub(); }
    } catch (e) { toast(e.message); }
  });

  // Images staged for the next message: pasted into the box or picked with
  // the attach button. Sent as base64 alongside the text; the server stores
  // them as run uploads and the main agent sees them inline.
  let pendingImgs = []; // {mime, data (base64), url (object URL for preview)}
  const IMG_MIMES = ["image/png", "image/jpeg", "image/webp", "image/gif"];

  const renderAttach = () => {
    const strip = $main.querySelector("#wf-attach");
    if (!strip) return;
    strip.style.display = pendingImgs.length ? "" : "none";
    strip.innerHTML = pendingImgs.map((p, i) => `
      <span class="attach-thumb"><img src="${p.url}" alt="">
        <button data-rm="${i}" title="移除">✕</button></span>`).join("");
    strip.querySelectorAll("[data-rm]").forEach((b) => b.addEventListener("click", () => {
      URL.revokeObjectURL(pendingImgs[b.dataset.rm]?.url);
      pendingImgs.splice(b.dataset.rm, 1);
      renderAttach();
    }));
  };

  const addImages = (files) => {
    for (const f of files) {
      if (!IMG_MIMES.includes(f.type)) { toast(`不支持的图片类型:${f.type || f.name}`); continue; }
      if (f.size > 8 * 1024 * 1024) { toast(`图片超过 8MB:${f.name || "剪贴板图片"}`); continue; }
      if (pendingImgs.length >= 10) { toast("每条消息最多 10 张图片"); break; }
      const reader = new FileReader();
      const url = URL.createObjectURL(f);
      reader.onload = () => {
        pendingImgs.push({ mime: f.type, data: reader.result.split(",", 2)[1], url });
        renderAttach();
      };
      reader.readAsDataURL(f);
    }
  };

  // ---- workspace selector ----
  // The directory this session works in and delivers to, picked here instead of
  // typed into the prompt. A run freezes its workspace at birth, so the bar is
  // only editable while a session is being composed. Nothing selected means the
  // default workspace — there is no "no workspace" state any more.

  // A dynamic session CONTINUES the selected run, so that run's workspace is
  // frozen and the bar is read-only. Static mode starts a brand-new run on
  // every send, so it stays editable even with a past run selected.
  const wsLocked = () => !!run && selWf()?.mode === "dynamic";
  const wsCurrent = () => (wsLocked() ? run.workspace || "" : selectedWorkspace || wsDefault);
  const wsIsDefault = () => !wsLocked() && !selectedWorkspace;
  // ~/x for anything under home, bare ~ for home itself (the browse root shows
  // it, even though home is never a legal workspace), absolute otherwise.
  const wsShort = (p) => {
    if (!wsHome) return p;
    if (p === wsHome) return "~";
    return p.startsWith(wsHome + "/") ? "~" + p.slice(wsHome.length) : p;
  };
  const wsBase = (p) => p.split("/").filter(Boolean).pop() || p;

  const renderWsBar = () => {
    const wrap = $main.querySelector("#wf-ws-wrap");
    const label = $main.querySelector("#wf-ws-label");
    if (!wrap || !label) return;
    const cur = wsCurrent();
    const locked = wsLocked();
    const isDefault = wsIsDefault();
    // "on" = an explicit choice; the default is shown quietly, with a tag.
    wrap.classList.toggle("on", !!cur && !isDefault);
    wrap.classList.toggle("locked", locked);
    if (cur) {
      label.innerHTML = `<b>${esc(wsBase(cur))}</b><span class="ws-path">${esc(wsShort(cur))}</span>` +
        (isDefault ? '<span class="ws-tag">默认</span>' : "");
    } else {
      label.innerHTML = `<span class="ws-none">${locked ? "本会话未记录工作区" : "选择工作区"}</span>`;
    }
    $main.querySelector("#wf-ws-btn").title = locked
      ? (cur ? "本会话的工作区(创建时已固定):" + cur : "该会话创建时未记录工作区,无法更改")
      : (isDefault ? "本次运行的工作区(默认):" + cur + " — 点击更换或新建文件夹" : "本次运行的工作区:" + cur);
    // The ✕ returns an explicit choice to the default; nothing to clear on a
    // frozen session or on the default itself.
    $main.querySelector("#wf-ws-x").hidden = isDefault || locked;
    if (locked) closeWsMenu();
  };

  const setWorkspace = (p) => {
    selectedWorkspace = p || "";
    if (selectedWorkspace) sessionStorage.setItem(wsKey(), selectedWorkspace);
    else sessionStorage.removeItem(wsKey());
    renderWsBar();
    renderWsRecent();
  };

  const loadWsList = async () => {
    try {
      const d = await api("/workspaces");
      wsList = (d.workspaces || []).filter((p) => p !== d.default);
      wsHome = d.home || wsHome;
      wsDefault = d.default || wsDefault;
    } catch { wsList = []; }
  };

  const renderWsRecent = () => {
    const box = $main.querySelector("#wf-ws-recent");
    if (!box) return;
    const cur = wsCurrent();
    // The default workspace heads the list; "" is its selection value.
    const head = wsDefault ? `
      <div class="ws-recent-item ${!selectedWorkspace ? "selected" : ""}" data-ws="" title="${esc(wsDefault)}">
        <span class="ws-item-name">${esc(wsBase(wsDefault))}</span>
        <span class="ws-item-path">${esc(wsShort(wsDefault))}</span>
        <span class="ws-tag">默认</span>
      </div>` : "";
    const rest = wsList.map((p) => `
      <div class="ws-recent-item ${p === cur ? "selected" : ""}" data-ws="${esc(p)}" title="${esc(p)}">
        <span class="ws-item-name">${esc(wsBase(p))}</span>
        <span class="ws-item-path">${esc(wsShort(p))}</span>
        <span class="ws-item-x" data-wsrm="${esc(p)}" title="从历史中移除">✕</span>
      </div>`).join("");
    box.innerHTML = head + (rest || '<div class="ws-empty">还没有用过其它工作区——在上面输入路径,或点「浏览文件夹」新建一个</div>');
    box.querySelectorAll("[data-ws]").forEach((el) => el.addEventListener("click", (e) => {
      if (e.target.closest("[data-wsrm]")) return; // the ✕ is not a selection
      setWorkspace(el.dataset.ws);
      closeWsMenu();
    }));
    box.querySelectorAll("[data-wsrm]").forEach((el) => el.addEventListener("click", async (e) => {
      e.stopPropagation();
      const p = el.dataset.wsrm;
      try { await api("/workspaces?path=" + encodeURIComponent(p), { method: "DELETE" }); } catch {}
      wsList = wsList.filter((x) => x !== p);
      renderWsRecent(); // forgetting the history does NOT unselect a live choice
    }));
  };

  const closeWsMenu = () => {
    wsOpen = false;
    const m = $main.querySelector("#wf-ws-menu");
    if (m) m.hidden = true;
    $main.querySelector("#wf-ws-wrap")?.classList.remove("open");
    showWsBrowser(false);
  };

  const openWsMenu = async () => {
    if (wsLocked()) {
      toast("工作区在会话创建时固定;要换项目请开一个新会话");
      return;
    }
    wsOpen = true;
    $main.querySelector("#wf-ws-menu").hidden = false;
    $main.querySelector("#wf-ws-wrap").classList.add("open");
    showWsBrowser(false);
    const inp = $main.querySelector("#wf-ws-input");
    inp.value = selectedWorkspace || "";
    await loadWsList(); // always fresh: another tab may have started runs
    inp.placeholder = "输入工作区路径…如 ~/projects/app";
    renderWsRecent();
    inp.focus();
  };

  // applyManualWs pre-checks a typed path through the browse endpoint: anything
  // it can list exists and is readable. A path OUTSIDE the home directory is
  // refused by that endpoint on purpose (it is the browser's confinement rule,
  // not a limit on workspaces), so we accept it as typed and let the server
  // decide when the run starts — projects on external volumes stay usable.
  const applyManualWs = async () => {
    const raw = $main.querySelector("#wf-ws-input").value.trim();
    if (!raw) { setWorkspace(""); closeWsMenu(); return; }
    try {
      const d = await api("/workspaces/browse", { method: "POST", body: { path: raw } });
      setWorkspace(d.path === wsDefault ? "" : d.path); // the server-normalized form, not the raw text
      closeWsMenu();
    } catch (e) {
      if (String(e.message).includes("outside the home directory")) {
        setWorkspace(raw);
        closeWsMenu();
        toast("该路径不在主目录内,无法预校验;发送时由服务端最终裁决");
        return;
      }
      toast("这个路径用不了:" + e.message);
    }
  };

  // ---- folder browser (a panel inside the dropdown, not a separate modal) ----

  const showWsBrowser = (on) => {
    const panel = $main.querySelector("#wf-ws-browser");
    const main = $main.querySelector("#wf-ws-main");
    if (!panel || !main) return;
    panel.hidden = !on;
    main.hidden = !!on;
  };

  const loadWsDirs = async (path) => {
    let d;
    try { d = await api("/workspaces/browse", { method: "POST", body: { path } }); }
    catch (e) { toast("打不开这个目录:" + e.message); return; }
    wsBrowsePath = d.path;
    wsBrowseDirs = d.dirs || [];
    wsHome = d.home || wsHome;
    $main.querySelector("#wf-ws-crumb").textContent = wsShort(wsBrowsePath);
    const up = $main.querySelector("#wf-ws-up");
    up.disabled = !d.parent;
    up.dataset.parent = d.parent || "";
    renderWsDirs(d.truncated);
    $main.querySelector("#wf-ws-dirs").scrollTop = 0;
  };

  // renderWsDirs paints the current listing. An entry that is the selected
  // workspace is highlighted; an EMPTY entry (the kind "新建文件夹" makes) gets
  // a ✎ that turns its name into an inline rename field — the server refuses
  // to rename anything with content, so the affordance only appears where it
  // will work.
  const renderWsDirs = (truncated) => {
    const list = $main.querySelector("#wf-ws-dirs");
    const cur = wsCurrent();
    list.innerHTML = wsBrowseDirs.map((x) => `
      <div class="ws-dir-entry ${x.path === cur ? "selected" : ""}" data-go="${esc(x.path)}" title="${esc(x.path)}">
        ${ICON_FOLDER}<span class="ws-dir-name">${esc(x.name)}</span>
        ${x.empty ? `<span class="ws-dir-edit" data-rename="${esc(x.path)}" title="重命名(仅空文件夹)">✎</span>` : ""}
      </div>`).join("") || '<div class="ws-empty">这里没有子目录——可以直接「选择此目录」,或「新建文件夹」</div>';
    if (truncated) {
      list.insertAdjacentHTML("beforeend", '<div class="ws-empty">子目录过多,只显示前 500 个</div>');
    }
    list.querySelectorAll("[data-go]").forEach((el) =>
      el.addEventListener("click", (e) => {
        if (e.target.closest("[data-rename]") || e.target.closest("input")) return;
        loadWsDirs(el.dataset.go);
      }));
    list.querySelectorAll("[data-rename]").forEach((el) =>
      el.addEventListener("click", (e) => { e.stopPropagation(); startWsRename(el.dataset.rename); }));
  };

  // startWsRename swaps one entry's name for an input. Enter commits, Escape
  // or blur cancels; the listing is repainted either way.
  const startWsRename = (path) => {
    const entry = $main.querySelector(`.ws-dir-entry[data-go="${CSS.escape(path)}"]`);
    if (!entry) return;
    const nameEl = entry.querySelector(".ws-dir-name");
    const old = wsBase(path);
    nameEl.innerHTML = `<input type="text" class="ws-inline" value="${esc(old)}" spellcheck="false">`;
    entry.querySelector("[data-rename]")?.remove();
    const inp = nameEl.querySelector("input");
    inp.focus(); inp.select();
    let done = false;
    const finish = async (commit) => {
      if (done) return;
      done = true;
      const name = inp.value.trim();
      if (!commit || !name || name === old) { renderWsDirs(false); return; }
      try {
        const d = await api("/workspaces/rename", { method: "POST", body: { path, name } });
        if (selectedWorkspace === path) setWorkspace(d.path);
        toast(`已重命名为 ${d.path}`);
      } catch (e) { toast("重命名失败:" + e.message); }
      await loadWsDirs(wsBrowsePath);
    };
    inp.addEventListener("keydown", (e) => {
      if (e.isComposing || e.keyCode === 229) return;
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(false); }
    });
    inp.addEventListener("blur", () => finish(false));
    inp.addEventListener("click", (e) => e.stopPropagation());
  };

  // startWsCreate opens an inline "new folder" row at the top of the listing.
  // A created folder is immediately selected as the workspace (that is why
  // one creates it) and stays highlighted in the parent, ✎ at hand.
  const startWsCreate = () => {
    const list = $main.querySelector("#wf-ws-dirs");
    if (list.querySelector(".ws-dir-new")) { list.querySelector(".ws-dir-new input")?.focus(); return; }
    list.insertAdjacentHTML("afterbegin", `
      <div class="ws-dir-entry ws-dir-new">${ICON_FOLDER}
        <span class="ws-dir-name"><input type="text" class="ws-inline" placeholder="新文件夹名称" spellcheck="false"></span>
      </div>`);
    const row = list.querySelector(".ws-dir-new");
    const inp = row.querySelector("input");
    inp.focus();
    let done = false;
    const finish = async (commit) => {
      if (done) return;
      done = true;
      const name = inp.value.trim();
      if (!commit || !name) { row.remove(); return; }
      try {
        const d = await api("/workspaces/mkdir", { method: "POST", body: { parent: wsBrowsePath, name } });
        setWorkspace(d.path);
        toast(`已创建并选为工作区:${wsShort(d.path)}(可点 ✎ 重命名)`);
      } catch (e) { toast("创建失败:" + e.message); }
      await loadWsDirs(wsBrowsePath);
    };
    inp.addEventListener("keydown", (e) => {
      if (e.isComposing || e.keyCode === 229) return;
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(false); }
    });
    inp.addEventListener("blur", () => finish(false));
    inp.addEventListener("click", (e) => e.stopPropagation());
  };

  const openWsBrowser = async () => {
    showWsBrowser(true);
    // Start where the draft points; a picked folder opens at its parent so it
    // shows up highlighted (and renamable) instead of as an empty listing.
    const start = selectedWorkspace ? selectedWorkspace.replace(/\/[^/]+$/, "") : (wsDefault || wsHome || "");
    await loadWsDirs(start || wsHome || "");
  };

  const send = async () => {
    const box = $main.querySelector("#wf-input");
    const text = box.value.trim();
    if (!text && !pendingImgs.length) return;
    const wf = selWf();
    if (!wf) return;
    if (wf.mode !== "dynamic" && pendingImgs.length) {
      toast("静态工作流没有会话,不支持图片——图片仅动态编排可用");
      return;
    }
    const images = pendingImgs.map((p) => ({ mime: p.mime, data: p.data }));
    box.value = "";
    box.dispatchEvent(new Event("input")); // collapse the auto-grown composer
    try {
      const dry = $main.querySelector("#wf-dry")?.checked || false;
      // Only a NEW run carries a workspace; continuing a session keeps the one
      // it was born with (the server ignores the field there anyway).
      const ws = wsLocked() ? "" : selectedWorkspace;
      let r;
      if (wf.mode === "dynamic") {
        // The message goes to the selected session; with none selected it
        // opens a new one. Finished sessions are reopened server-side.
        r = await api(`/workflows/${wf.id}/chat`, {
          method: "POST",
          body: { text, images, dry_run: dry, workspace: ws, run_id: sesId || "", new_session: sesId === null },
        });
      } else {
        r = await api(`/workflows/${wf.id}/runs`, {
          method: "POST",
          body: { goal: text, dry_run: dry, workspace: ws },
        });
      }
      pendingImgs.forEach((p) => URL.revokeObjectURL(p.url));
      pendingImgs = [];
      renderAttach();
      const isNew = !run || r.id !== run.id;
      run = r;
      sesId = r.id;
      sessionStorage.setItem(sesKey(), sesId);
      // The run owns the workspace now and the bar switches to its locked form
      // showing r.workspace (the server's normalized, authoritative value), so
      // the draft is spent. Static mode keeps its draft: every send there opens
      // a new run and the bar stays editable — blanking it under the user's
      // cursor would be nothing but a surprise.
      if (wsLocked()) {
        selectedWorkspace = "";
        sessionStorage.removeItem(wsKey());
      }
      if (ws) loadWsList(); // the server just promoted it in the MRU
      if (isNew || !sessions.some((s) => s.id === r.id)) {
        try { sessions = await fetchSessions(); } catch {}
      }
      renderSessions();
      resub();
      renderRight();
    } catch (e) { toast("发送失败:" + e.message); }
  };

  $main.innerHTML = `
    <div class="wf-split">
      <div class="wf-left">
        <div class="wf-left-head">
          <h1 style="font-size:16px;margin:0">会话</h1>
          <button class="small primary" id="wf-new-left" title="开始一个全新会话">+ 新会话</button>
        </div>
        <div id="wf-list" class="ses-list"></div>
      </div>
      <div class="wf-right">
        <div class="wf-head" id="wf-head"></div>
        <div class="wf-sessions" id="wf-sessions"></div>
        <div class="wf-status" id="wf-status"></div>
        <div class="wf-chat-log" id="wf-chat-log"></div>
        <div class="thread-panel" id="wf-thread" style="display:none"></div>
        <div class="wf-chat-input">
          <label class="check" id="wf-dry-wrap" style="font-size:11.5px;padding:2px 4px 6px">
            <input type="checkbox" id="wf-dry" ${meta.default_dry_run ? "checked" : ""}>
            <span>演示模式(dry run,零成本)— 仅对新发起的运行生效</span></label>
          <div class="ws-pick" id="wf-ws-wrap">
            <div class="ws-selector" id="wf-ws-btn" role="button" tabindex="0"
                 title="选择本次运行的工作区(项目与产物所在目录)">
              <span class="ws-ico">${ICON_FOLDER}</span>
              <span class="ws-label" id="wf-ws-label"><span class="ws-none">选择工作区</span></span>
              <span class="ws-x" id="wf-ws-x" title="改回默认工作区" hidden>✕</span>
              <span class="ws-caret">▾</span>
            </div>
            <div class="ws-dropdown" id="wf-ws-menu" hidden>
              <div id="wf-ws-main">
                <div class="ws-manual">
                  <input type="text" id="wf-ws-input" spellcheck="false" placeholder="输入项目路径…">
                  <button class="small" id="wf-ws-apply">使用</button>
                </div>
                <div class="ws-sec-head">最近使用</div>
                <div class="ws-recent" id="wf-ws-recent"></div>
                <div class="ws-foot">
                  <button class="small" id="wf-ws-browse">浏览文件夹 / 新建</button>
                  <span style="flex:1"></span>
                  <button class="small" id="wf-ws-clear">使用默认工作区</button>
                </div>
              </div>
              <div class="ws-browse-panel" id="wf-ws-browser" hidden>
                <div class="ws-crumb" id="wf-ws-crumb"></div>
                <div class="ws-dirs" id="wf-ws-dirs"></div>
                <div class="ws-foot">
                  <button class="small" id="wf-ws-up">↑ 上级目录</button>
                  <button class="small" id="wf-ws-new">+ 新建文件夹</button>
                  <span style="flex:1"></span>
                  <button class="small" id="wf-ws-back">返回</button>
                  <button class="small primary" id="wf-ws-pick">选择此目录</button>
                </div>
              </div>
            </div>
          </div>
          <div class="attach-strip" id="wf-attach" style="display:none"></div>
          <textarea id="wf-input" rows="1" placeholder="对 main agent 说出目标或追加要求…(Enter 发送,Shift+Enter 换行,可粘贴图片)"></textarea>
          <div class="composer-bar">
            <button class="icon-btn" id="wf-attachbtn" title="添加图片(也可直接粘贴)">${ICON_IMG}</button>
            <input type="file" id="wf-file" accept="image/png,image/jpeg,image/webp,image/gif" multiple style="display:none">
            <span style="flex:1"></span>
            <button class="primary small" id="wf-send">发送</button>
          </div>
        </div>
      </div>
    </div>`;
  renderLeft();
  await loadRun();
  selectedWorkspace = sessionStorage.getItem(wsKey()) || "";
  renderHead(); renderSessions(); renderRight(); resub();
  // Non-blocking: the bar renders immediately and re-renders once home arrives,
  // which is what turns absolute paths into their ~ form.
  loadWsList().then(() => { renderWsBar(); renderWsRecent(); });
  $main.querySelector("#wf-send").addEventListener("click", send);
  $main.querySelector("#wf-new-left").addEventListener("click", () => $main.querySelector("#wf-new-session")?.click());
  autoGrow($main.querySelector("#wf-input"));
  $main.querySelector("#wf-input").addEventListener("keydown", (e) => {
    // An Enter that is confirming an IME composition (pinyin etc.) belongs to
    // the IME, not to us: isComposing covers the standard case, keyCode 229
    // covers engines that fire the key event before composition ends.
    if (e.isComposing || e.keyCode === 229) return;
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  });
  // Image intake: paste into the box, or pick via the attach button.
  $main.querySelector("#wf-input").addEventListener("paste", (e) => {
    const files = [...(e.clipboardData?.items || [])]
      .filter((it) => it.kind === "file" && it.type.startsWith("image/"))
      .map((it) => it.getAsFile()).filter(Boolean);
    if (files.length) { e.preventDefault(); addImages(files); }
  });
  const fileInput = $main.querySelector("#wf-file");
  $main.querySelector("#wf-attachbtn").addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => { addImages([...fileInput.files]); fileInput.value = ""; });

  // Workspace selector — bound once. The composer is built a single time and
  // never replaced by a re-render, so these listeners survive every SSE push.
  $main.querySelector("#wf-ws-btn").addEventListener("click", (e) => {
    if (e.target.closest("#wf-ws-x")) return; // the ✕ is handled below
    e.stopPropagation();
    wsOpen ? closeWsMenu() : openWsMenu();
  });
  $main.querySelector("#wf-ws-btn").addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); wsOpen ? closeWsMenu() : openWsMenu(); }
  });
  $main.querySelector("#wf-ws-x").addEventListener("click", (e) => {
    e.stopPropagation();
    setWorkspace("");
    closeWsMenu();
  });
  $main.querySelector("#wf-ws-apply").addEventListener("click", applyManualWs);
  $main.querySelector("#wf-ws-input").addEventListener("keydown", (e) => {
    // An Enter confirming an IME composition belongs to the IME, not to us.
    if (e.isComposing || e.keyCode === 229) return;
    if (e.key === "Enter") { e.preventDefault(); applyManualWs(); }
    if (e.key === "Escape") closeWsMenu();
  });
  $main.querySelector("#wf-ws-browse").addEventListener("click", openWsBrowser);
  $main.querySelector("#wf-ws-clear").addEventListener("click", () => { setWorkspace(""); closeWsMenu(); });
  $main.querySelector("#wf-ws-up").addEventListener("click", (e) => {
    const p = e.currentTarget.dataset.parent;
    if (p) loadWsDirs(p);
  });
  $main.querySelector("#wf-ws-back").addEventListener("click", () => showWsBrowser(false));
  $main.querySelector("#wf-ws-new").addEventListener("click", startWsCreate);
  $main.querySelector("#wf-ws-pick").addEventListener("click", () => {
    if (wsBrowsePath) setWorkspace(wsBrowsePath === wsDefault ? "" : wsBrowsePath);
    closeWsMenu();
  });
  document.addEventListener("click", onDocClick);
}

// ---------- workflow editor ----------

// wfEditPage edits one workflow record. Since the pilot UI there are only two
// kinds — THE main-agent configuration (dynamic, exactly one, under 设置 › 主
// agent) and static TEMPLATES (under 设置 › 静态模板) — the mode is a fact of
// the record, never a choice on this form. opts.prefix is HTML rendered
// above the form (the settings tabs); opts.back is where 保存/删除 returns.
async function wfEditPage(id, opts = {}) {
  const [agents, wf] = await Promise.all([
    api("/agents"),
    id ? api("/workflows/" + id) : Promise.resolve({
      name: "", description: "", mode: "static",
      planner: { model: "sonnet", max_nodes: 8, system_prompt: "" },
      agent_pool: [], require_approval: true, replan_enabled: true, max_replans: 2,
      max_retries: 1, parallelism: 3, node_timeout_sec: 0,
    }),
  ]);
  const pool = new Set(wf.agent_pool || []);
  const b = wf.budget || {};
  let mode = wf.mode === "dynamic" ? "dynamic" : "static";
  const back = opts.back || (mode === "dynamic" ? "#/settings" : "#/settings/templates");
  const title = mode === "dynamic" ? "主 agent 设置" : (id ? "编辑模板" : "新建模板");

  const render = () => {
    const dyn = mode === "dynamic";
    $main.innerHTML = (opts.prefix || "") + `
    <div class="page-head">
      <h1>${title}</h1>
      ${id && !dyn ? '<button class="danger" id="wf-del">删除</button>' : ""}
      <button class="primary" id="wf-save">保存</button>
    </div>
    <div class="panel" style="max-width:760px">
      ${dyn ? `<div class="muted" style="font-size:12.5px;margin-bottom:12px">每个会话都由这个 main agent 驱动:它住在会话的工作区里、有自己的工具,按 level(solo / pair / orchestrate)决定何时动手、何时派单;静态模板在「静态模板」页维护,它可以把模板当一个任务来跑。</div>` :
        `<div class="muted" style="font-size:12.5px;margin-bottom:12px">静态模板:planner 先出完整 DAG,审批后确定性执行。main agent 可用 run_template 把它当一个任务跑;也可以在模板列表直接运行。</div>`}
      <label class="field"><span>名称</span><input id="f-name" value="${esc(wf.name)}"></label>
      <label class="field"><span>描述</span><input id="f-desc" value="${esc(wf.description || "")}"></label>

      ${dyn ? `
      <label class="field"><span>Coordinator 附加指导 — 统筹风格 / 领域偏好</span>
        <textarea id="f-coord-sp" rows="3">${esc(wf.coordinator?.system_prompt || "")}</textarea></label>
      <div class="row">
        <label class="field" style="flex:1"><span>Coordinator 模型</span>
          ${modelSelect("f-coord-model", wf.coordinator?.model)}</label>
        <label class="field" style="flex:1"><span>审批策略</span>
          <select id="f-approval-policy">
            <option value="initial" ${b.approval_policy !== "none" ? "selected" : ""}>首批委派需审批</option>
            <option value="none" ${b.approval_policy === "none" ? "selected" : ""}>无审批(仅靠预算兜底)</option>
          </select></label>
      </div>
      <div class="row">
        <label class="field" style="flex:1"><span>main agent 的手 — 它自己在工作区里可用的工具(留空 = 全部)。什么时候允许动手由 level 决定,引擎在工具调用处强制</span>
          <input id="f-coord-tools" value="${esc(wf.coordinator?.tools || "")}" placeholder="read,grep,glob,edit,write,bash,webfetch,websearch"></label>
        <label class="field" style="flex:1"><span>起始 level(钉死)— 空 = 引擎默认(solo)</span>
          <select id="f-coord-level">
            <option value="" ${!wf.coordinator?.level ? "selected" : ""}>自动(默认 solo)</option>
            <option value="solo" ${wf.coordinator?.level === "solo" ? "selected" : ""}>solo — 自己动手,需要时派单</option>
            <option value="pair" ${wf.coordinator?.level === "pair" ? "selected" : ""}>pair — 自己动手 + 常驻伙伴</option>
            <option value="orchestrate" ${wf.coordinator?.level === "orchestrate" ? "selected" : ""}>orchestrate — 只派单,不动手(写操作被拒)</option>
          </select></label>
      </div>
      <label class="field"><span>常驻伙伴(pair)— 勾选的 agent 各有一个持久会话住在工作区,派给它的任务在该会话上串行执行,项目理解跨任务累积。implementer 与 reviewer 可同时常驻</span></label>
      <div class="row" style="flex-wrap:wrap;gap:6px 14px;margin-bottom:12px" id="f-pair-agents">
        ${agents.map((a) => `<label class="check"><input type="checkbox" value="${esc(a.name)}" ${(wf.pair_agents || []).includes(a.name) || wf.pair_agent === a.name ? "checked" : ""}> ${esc(a.name)}${a.independent ? ' <span class="badge">independent</span>' : ""}</label>`).join("") || '<span class="muted">池里还没有 agent</span>'}
      </div>
      <label class="field"><span>Triage 阈值 — main agent 每个任务前提交结构化评估,引擎按这些阈值定 level(任一达到 → orchestrate);改代码默认 ≥ pair。留空用默认。</span></label>
      <div class="row">
        <label class="field" style="flex:1"><span>步骤数 ≥</span><input id="f-tr-steps" type="number" placeholder="6" value="${wf.triage?.orchestrate_steps || ""}"></label>
        <label class="field" style="flex:1"><span>独立分支 ≥</span><input id="f-tr-branches" type="number" placeholder="2" value="${wf.triage?.orchestrate_branches || ""}"></label>
        <label class="field" style="flex:1"><span>角色种类 ≥</span><input id="f-tr-roles" type="number" placeholder="2" value="${wf.triage?.orchestrate_roles || ""}"></label>
        <label class="field" style="flex:1"><span>预计文件 ≥</span><input id="f-tr-files" type="number" placeholder="8" value="${wf.triage?.orchestrate_files || ""}"></label>
      </div>
      <div class="row" style="margin-bottom:12px">
        <label class="field" style="flex:1"><span>中途重判:自己改了 N 个文件</span><input id="f-tr-refiles" type="number" placeholder="8" value="${wf.triage?.reassess_files || ""}"></label>
        <label class="field" style="flex:1"><span>中途重判:验收失败 K 次</span><input id="f-tr-refails" type="number" placeholder="3" value="${wf.triage?.reassess_test_failures || ""}"></label>
        <label class="check" style="align-self:flex-end"><input type="checkbox" id="f-tr-pairoff" ${wf.triage?.pair_off_for_code ? "checked" : ""}> 改代码不自动升到 pair</label>
      </div>
      <label class="field"><span>预算护栏 — 由引擎硬执行,不依赖 coordinator 自觉。这是 dynamic 模式唯一的终止保证。</span></label>
      <div class="row">
        <label class="field" style="flex:1"><span>最大任务数</span>
          <input id="f-max-tasks" type="number" value="${b.max_tasks || 30}"></label>
        <label class="field" style="flex:1"><span>最大委派深度</span>
          <input id="f-max-depth" type="number" value="${b.max_delegation_depth || 3}"></label>
        <label class="field" style="flex:1"><span>并行任务数</span>
          <input id="f-max-par" type="number" value="${b.max_parallel || 3}"></label>
      </div>
      <div class="row">
        <label class="field" style="flex:1"><span>单任务超时(秒)</span>
          <input id="f-task-timeout" type="number" value="${b.task_timeout_sec || 1800}"></label>
        <label class="field" style="flex:1"><span>整体墙钟(秒)</span>
          <input id="f-run-timeout" type="number" value="${b.run_timeout_sec || 7200}"></label>
        <label class="field" style="flex:1"><span>单任务最大轮次</span>
          <input id="f-max-turns" type="number" value="${b.max_turns_per_task || 6}"></label>
        <label class="field" style="flex:1"><span>单任务返工上限</span>
          <input id="f-max-reworks" type="number" value="${b.max_reworks_per_task || 2}"></label>
        <label class="field" style="flex:1"><span>停滞告警(秒)</span>
          <input id="f-stall" type="number" value="${b.stall_sec || 600}"></label>
      </div>
      <div class="row" style="margin-bottom:14px">
        <label class="check"><input type="checkbox" id="f-handoff" ${b.allow_peer_handoff ? "checked" : ""}> 允许 worker 之间直接交接(peer handoff)</label>
        <label class="check"><input type="checkbox" id="f-dyn-create" ${b.allow_agent_creation ? "checked" : ""}> 允许 coordinator 自建 agent</label>
      </div>
      ` : `
      <label class="field"><span>Planner(main agent)附加指导 — 组装 DAG 时的策略偏好</span>
        <textarea id="f-planner-sp" rows="3">${esc(wf.planner?.system_prompt || "")}</textarea></label>
      <div class="row">
        <label class="field" style="flex:1"><span>Planner 模型</span>
          ${modelSelect("f-planner-model", wf.planner?.model)}</label>
        <label class="field" style="flex:1"><span>最大节点数</span>
          <input id="f-max-nodes" type="number" value="${wf.planner?.max_nodes || 8}"></label>
        <label class="field" style="flex:1"><span>并行度</span>
          <input id="f-par" type="number" value="${wf.parallelism || 3}"></label>
      </div>
      <div class="row">
        <label class="field" style="flex:1"><span>节点重试次数</span>
          <input id="f-retries" type="number" value="${wf.max_retries ?? 1}"></label>
        <label class="field" style="flex:1"><span>最大 replan 次数</span>
          <input id="f-replans" type="number" value="${wf.max_replans ?? 2}"></label>
        <label class="field" style="flex:1"><span>节点超时(秒,0=默认1800)</span>
          <input id="f-timeout" type="number" value="${wf.node_timeout_sec || 0}"></label>
      </div>
      <div class="row" style="margin-bottom:14px">
        <label class="check"><input type="checkbox" id="f-approval" ${wf.require_approval ? "checked" : ""}> 计划需人工审批后才执行</label>
        <label class="check"><input type="checkbox" id="f-replan" ${wf.replan_enabled ? "checked" : ""}> 节点失败后自动 replan</label>
        <label class="check"><input type="checkbox" id="f-create" ${wf.allow_agent_creation ? "checked" : ""}> 允许 planner 自建 agent(缺人时创建并入池)</label>
      </div>
      `}

      <label class="field"><span>Agent 池 · <span id="pool-count"></span>(不选 = 使用全部)—
        每个 agent 的 system prompt(即其 home 的 AGENTS.md)与私有 skills 在
        <a href="#/agents">Agent 池</a> 页查看与编辑</span></label>
      <div class="pool-picker">
        ${agents.map((a) => `
          <label class="check"><input type="checkbox" data-agent="${esc(a.name)}" ${pool.has(a.name) ? "checked" : ""}>
            <span><b>${esc(a.name)}</b> <span class="muted">${esc(a.description || "")}</span></span></label>`).join("")}
      </div>

      <div class="prompt-preview">
        <div class="pp-head">
          <div style="flex:1;min-width:0">
            <b>main agent 完整配置 — ${dyn ? "coordinator" : "planner"} 系统提示词</b>
            <span class="muted">${dyn
              ? "coordinator 每次激活实际收到的完整系统提示词:loom 编排规则 + 预算护栏 + 审批策略 + Agent 池 + 上方附加指导,实时组装。只读;要改可编辑的部分,用上面的「附加指导」。"
              : "planner 组装 DAG 时实际收到的完整提示词:组装规则 + Agent 注册表 + 上方附加指导,实时组装。只读;要改可编辑的部分,用上面的「附加指导」。"}</span>
          </div>
          <button class="small" id="pp-refresh" style="display:none" title="按当前表单值重新组装">刷新</button>
          <button class="small" id="pp-btn">查看</button>
        </div>
        <pre id="pp-body" class="pp-body" style="display:none"></pre>
      </div>
    </div>`;
    wire();
  };

  const val = (sel, fallback) => {
    const el = $main.querySelector(sel);
    return el ? el.value : fallback;
  };
  const checked = (sel) => {
    const el = $main.querySelector(sel);
    return el ? el.checked : false;
  };

  const wire = () => {
    const count = () => {
      const el = $main.querySelector("#pool-count");
      if (el) el.textContent = `已选 ${$main.querySelectorAll("[data-agent]:checked").length} / ${agents.length}`;
    };
    count();
    $main.querySelectorAll("[data-agent]").forEach((c) => c.addEventListener("change", count));
    // Switching mode preserves whatever the other mode's fields already held:
    // the form is a view over one workflow object, not two.
    $main.querySelectorAll('input[name="mode"]').forEach((r) =>
      r.addEventListener("change", () => {
        collect();
        mode = r.value;
        render();
      }));
    // Effective-prompt preview: posts the CURRENT form state (unsaved edits
    // included), so what the user reads is what the main agent would get.
    const ppBtn = $main.querySelector("#pp-btn");
    const ppRefresh = $main.querySelector("#pp-refresh");
    const ppBody = $main.querySelector("#pp-body");
    const loadPreview = async () => {
      collect();
      try {
        const res = await api("/workflows/prompt-preview", { method: "POST", body: wf });
        ppBody.textContent = res.prompt;
        ppBody.style.display = "";
        ppRefresh.style.display = "";
        ppBtn.textContent = "收起";
      } catch (e) { toast("预览失败:" + e.message); }
    };
    ppBtn.onclick = () => {
      if (ppBody.style.display === "none") return loadPreview();
      ppBody.style.display = "none";
      ppRefresh.style.display = "none";
      ppBtn.textContent = "查看";
    };
    ppRefresh.onclick = loadPreview;
    $main.querySelector("#wf-save").onclick = async () => {
      collect();
      if (!wf.name) return toast("请填写名称");
      try {
        await api("/workflows", { method: "POST", body: wf });
        location.hash = back;
      } catch (e) { toast("保存失败:" + e.message); }
    };
    const del = $main.querySelector("#wf-del");
    if (del) del.onclick = async () => {
      if (!(await confirmModal("删除工作流", "运行记录会保留,仅删除工作流定义。", "删除"))) return;
      await api("/workflows/" + id, { method: "DELETE" });
      location.hash = back;
    };
  };

  const collect = () => {
    wf.name = val("#f-name", wf.name).trim();
    wf.description = val("#f-desc", wf.description || "").trim();
    wf.mode = mode;
    wf.agent_pool = [...$main.querySelectorAll("[data-agent]:checked")].map((c) => c.dataset.agent);
    if (mode === "dynamic") {
      wf.coordinator = {
        model: val("#f-coord-model", wf.coordinator?.model || "").trim(),
        system_prompt: val("#f-coord-sp", wf.coordinator?.system_prompt || "").trim(),
      };
      wf.coordinator.tools = val("#f-coord-tools", wf.coordinator?.tools || "").trim();
      wf.coordinator.level = val("#f-coord-level", wf.coordinator?.level || "");
      wf.pair_agent = ""; // legacy single field, folded into the list
      wf.pair_agents = [...$main.querySelectorAll("#f-pair-agents input:checked")].map((i) => i.value);
      wf.triage = {
        orchestrate_steps: +val("#f-tr-steps", 0) || 0,
        orchestrate_branches: +val("#f-tr-branches", 0) || 0,
        orchestrate_roles: +val("#f-tr-roles", 0) || 0,
        orchestrate_files: +val("#f-tr-files", 0) || 0,
        reassess_files: +val("#f-tr-refiles", 0) || 0,
        reassess_test_failures: +val("#f-tr-refails", 0) || 0,
        pair_off_for_code: checked("#f-tr-pairoff"),
      };
      wf.budget = {
        max_tasks: +val("#f-max-tasks", 30),
        max_delegation_depth: +val("#f-max-depth", 3),
        max_parallel: +val("#f-max-par", 3),
        task_timeout_sec: +val("#f-task-timeout", 1800),
        run_timeout_sec: +val("#f-run-timeout", 7200),
        max_turns_per_task: +val("#f-max-turns", 6),
        max_reworks_per_task: +val("#f-max-reworks", 2),
        stall_sec: +val("#f-stall", 600),
        allow_peer_handoff: checked("#f-handoff"),
        allow_agent_creation: checked("#f-dyn-create"),
        approval_policy: val("#f-approval-policy", "initial"),
      };
    } else {
      wf.planner = {
        model: val("#f-planner-model", wf.planner?.model || "").trim(),
        max_nodes: +val("#f-max-nodes", 8),
        system_prompt: val("#f-planner-sp", wf.planner?.system_prompt || "").trim(),
      };
      wf.require_approval = checked("#f-approval");
      wf.replan_enabled = checked("#f-replan");
      wf.allow_agent_creation = checked("#f-create");
      wf.max_replans = +val("#f-replans", 2);
      wf.max_retries = +val("#f-retries", 1);
      wf.parallelism = +val("#f-par", 3);
      wf.node_timeout_sec = +val("#f-timeout", 0);
    }
  };

  render();
}

// ---------- runs list ----------

async function runsListPage() {
  const wfFilter = sessionStorage.getItem("wfFilter") || "";
  sessionStorage.removeItem("wfFilter");
  const load = async () => {
    const runs = await api("/runs" + (wfFilter ? "?workflow_id=" + wfFilter : ""));
    const rows = runs.map((r) => {
      // Progress reads differently per mode: static has a known denominator,
      // dynamic does not — the tree is still growing.
      let progress = "—";
      if (r.mode === "dynamic") {
        const tasks = Object.values(r.tasks || {});
        const done = tasks.filter((t) => t.status === "completed").length;
        if (tasks.length) progress = `${done}/${tasks.length}+`;
      } else {
        const total = r.plan?.nodes?.length || 0;
        const done = Object.values(r.nodes || {}).filter((n) => n.status === "succeeded").length;
        if (total) progress = done + "/" + total;
      }
      return `
      <tr class="click" data-run="${esc(r.id)}">
        <td class="mono">${esc(r.id.slice(0, 24))}</td>
        <td>${esc(r.workflow_name)} <span class="badge">${r.mode === "dynamic" ? "会话" : "模板"}</span>${r.level ? `<span class="badge lvl ${esc(r.level)}">${esc(r.level)}</span>` : ""}${r.parent_run_id ? `<a class="badge" href="#/runs/${esc(r.parent_run_id)}" title="由会话中的模板任务发起">⧉ 子运行</a>` : ""}</td>
        <td style="max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(r.goal)}</td>
        <td>${chip(r.status)}</td>
        <td class="mono">${progress}</td>
        <td class="mono">${r.dry_run || r.backend === "mock" ? "dry-run" : esc(r.backend)}</td>
        <td class="mono">${fmtCost(r.cost_usd)}</td>
        <td class="mono">${fmtTime(r.created_at)}</td>
        <td>${r.mode === "dynamic"
          ? `<button class="small" data-open-ses="${esc(r.workflow_id)}|${esc(r.id)}" title="回到该会话继续对话">打开会话</button>`
          : ""}</td>
      </tr>`;
    }).join("");
    $main.innerHTML = `
      <div class="page-head"><h1>运行记录${wfFilter ? '<span class="muted">(已过滤)</span>' : ""}</h1></div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Run</th><th>工作流</th><th>目标</th><th>状态</th><th>进度</th><th>运行时</th><th>est. 成本</th><th>发起时间</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
        ${runs.length ? "" : '<div class="empty">还没有运行记录</div>'}
      </div>`;
    $main.querySelectorAll("[data-run]").forEach((tr) =>
      tr.addEventListener("click", () => (location.hash = "#/runs/" + tr.dataset.run)));
    $main.querySelectorAll("[data-open-ses]").forEach((btn) =>
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        const [wfId, runId] = btn.dataset.openSes.split("|");
        openSession(wfId, runId);
      }));
  };
  await load();
  const timer = setInterval(load, 5000);
  cleanup = () => clearInterval(timer);
}

// ---------- run detail ----------

// Detail and topology are two views of one snapshot. The strip is a plain
// anchor set so the hash stays the single source of truth — a reload, a
// back button or a pasted link all land on the same view.
function runTabs(id, which) {
  const tab = (key, suffix, label) =>
    `<a class="rtab${which === key ? " active" : ""}" href="#/runs/${esc(id)}${suffix}">${label}</a>`;
  return `<div class="rtabs">
      ${tab("detail", "", "📋 详情")}
      ${tab("topology", "/topology", "🕸 拓扑")}
    </div>`;
}

async function runPage(id) {
  let run;
  try { run = await api("/runs/" + id); }
  catch (e) { $main.innerHTML = `<div class="empty">运行不存在:${esc(e.message)}</div>`; return; }

  let selectedNode = null;

  const render = () => {
    const dyn = run.mode === "dynamic";
    const terminal = ["succeeded", "failed", "canceled", "interrupted"].includes(run.status);
    const controls = [];
    if (run.status === "awaiting_approval") {
      controls.push('<button class="primary" data-act="approve">✓ 批准执行</button>');
      // A dynamic rejection is a decision the coordinator must be told about,
      // not a process kill — it gets to record why the run failed.
      controls.push(dyn
        ? '<button class="danger" data-act="reject">✗ 拒绝该计划</button>'
        : '<button class="danger" data-act="cancel">拒绝并取消</button>');
    } else if (!terminal) {
      controls.push('<button class="danger" data-act="cancel">取消运行</button>');
    }
    if (dyn && run.status === "interrupted") {
      controls.push('<button class="primary" data-act="resume">▶ 从台账恢复</button>');
    }
    if (dyn) {
      controls.push(`<button onclick="openSession('${esc(run.workflow_id)}','${esc(run.id)}')" title="回到工作流页,在此会话继续与 main agent 对话">💬 打开会话</button>`);
    }
    $main.innerHTML = `
      <div class="page-head">
        <h1>${esc(run.workflow_name)}</h1>
        <span class="badge">${run.mode === "dynamic" ? "会话" : "模板"}</span>
        ${run.parent_run_id ? `<a class="badge" href="#/runs/${esc(run.parent_run_id)}" title="回到发起它的会话运行">⧉ 父运行 ${esc(run.parent_run_id)}</a>` : ""}
        ${chip(run.status)}
        ${controls.join("")}
      </div>
      ${runTabs(id, "detail")}
      <div class="panel">
        <div class="run-goal">${esc(run.goal)}</div>
        <div class="run-meta">
          <span class="mono">${esc(run.id)}</span>
          <span>${run.dry_run || run.backend === "mock" ? '<b style="color:var(--warn)">dry-run(演示)</b>' : `运行时: <b class="mono">${esc(run.backend)}</b>`}</span>
          <span title="按 Claude API 牌价折算,非实际账单">est. 成本: <b class="mono">${fmtCost(run.cost_usd)}</b></span>
          <span class="mono">${fmtTokens(run.usage)}</span>
          ${dyn ? `<span>任务: <b class="mono">${Object.keys(run.tasks || {}).length}</b></span>`
                : `<span>replan: <b class="mono">${run.replans}</b></span>`}
          ${run.workspace || run.output_dir ? `<span title="工作区">📁 <b class="mono">${esc(run.workspace || run.output_dir)}</b></span>` : ""}
          <span>耗时: <b class="mono">${runDuration(run)}</b></span>
          <span>${fmtTime(run.created_at)}</span>
        </div>
        ${run.error ? `<div style="color:var(--bad);font-size:13px;margin-top:8px">${esc(run.error)}</div>` : ""}
      </div>
      ${terminal ? (dyn && !(run.dry_run || run.backend === "mock") ? `
      <div class="panel" style="margin-top:14px">
        ${run.feedback ? `<div style="font-size:12.5px;margin-bottom:6px">📮 复盘记录(仅存档,不注入):<i>${esc(run.feedback)}</i></div>` : ""}
        <div class="muted" style="font-size:12px;margin-bottom:6px">📮 复盘反馈:提交后唤醒 main agent 消化——指代不清会反问,值得沉淀的进项目记忆/修订提案;结论只存档,提炼的行为规范在 workflow「复盘」面板确认后才注入之后的 run</div>
        <textarea id="fb-input" rows="2" placeholder="例:报告把结论埋在最后——下次先给结论" style="width:100%;resize:vertical"></textarea>
        <div class="row" style="margin-top:6px"><button class="small primary" data-act="feedback">发起复盘</button></div>
      </div>` : `
      <div class="panel" style="margin-top:14px">
        <div class="muted" style="font-size:12px;margin-bottom:6px">📮 复盘反馈${run.feedback ? "(已记录 ✓)" : ""} — ${dyn ? "演示模式没有真的 main agent," : "static 模式没有对话角色,"}原文保存为复盘记录(不注入);要注入之后 run 的行为规范,在 workflow「复盘」面板手动添加</div>
        <textarea id="fb-input" rows="2" placeholder="例:报告把结论埋在最后——下次先给结论" style="width:100%;resize:vertical">${esc(run.feedback || "")}</textarea>
        <div class="row" style="margin-top:6px"><button class="small primary" data-act="feedback">保存反馈</button></div>
      </div>`) : ""}
      ${dyn && run.status === "awaiting_approval" && run.proposal ? renderProposal(run.proposal) : ""}
      ${!dyn && run.plan?.agents?.length ? `
      <div class="panel" style="margin-top:14px">
        <div class="muted" style="font-size:12px;margin-bottom:8px">🧬 planner 为此计划定义的新 agent${run.status === "awaiting_approval" ? "(批准后创建并永久入池)" : ""}</div>
        <div class="grid">
          ${run.plan.agents.map((a) => `
            <div style="border:1px solid var(--border);border-radius:10px;padding:10px 12px">
              <b>${esc(a.name)}</b> <span class="badge">${esc(a.model)}</span>
              ${a.tools ? a.tools.split(",").map((t) => `<span class="badge">${esc(t.trim())}</span>`).join("") : '<span class="badge">无工具</span>'}
              <div class="muted" style="font-size:12px;margin-top:6px">${esc(a.description)}</div>
            </div>`).join("")}
        </div>
      </div>` : ""}
      <div class="run-layout">
        <div class="panel ${dyn ? "" : "dag-wrap"}" id="graph">
          ${dyn ? renderTaskTree(run, selectedNode)
                : (run.plan ? renderDag(run, selectedNode) : '<div class="empty">planner 正在组装 DAG…</div>')}
        </div>
        <div class="panel drawer" id="drawer">
          ${dyn ? renderTaskDrawer(run, selectedNode) : renderDrawer(run, selectedNode)}
        </div>
      </div>
      <div class="panel events">
        ${(run.events || []).slice().reverse().map((ev) => `
          <div class="ev ${esc(ev.type)}">
            <span class="ts mono">${fmtTime(ev.ts)}</span>
            <span class="tag">${esc(ev.type)}${ev.node ? " · " + esc(ev.node.slice(-8)) : ""}</span>
            <span>${esc(ev.msg)}</span>
          </div>`).join("")}
      </div>`;

    $main.querySelectorAll("[data-act]").forEach((btn) =>
      btn.addEventListener("click", async () => {
        const act = btn.dataset.act;
        let body = {};
        if (act === "feedback") {
          const text = $main.querySelector("#fb-input")?.value ?? "";
          const conversational = dyn && !(run.dry_run || run.backend === "mock");
          if (conversational && !text.trim()) return toast("请先写下反馈内容");
          body = { text };
        }
        if (act === "reject") {
          const reason = await modalDialog({
            title: "拒绝该计划", inputPlaceholder: "拒绝理由(会告知 coordinator)…",
            confirmText: "拒绝", danger: true,
          });
          if (reason === null) return;
          body = { reason };
        }
        try {
          await api(`/runs/${run.id}/${act}`, { method: "POST", body });
          if (act === "feedback") {
            toast(dyn && !(run.dry_run || run.backend === "mock")
              ? "已发起复盘,main agent 正在消化;提炼出的规范会等你在「复盘」面板确认" : "反馈已保存为复盘记录(不注入)");
          }
        } catch (e) { toast(e.message); }
      }));
    $main.querySelectorAll(".dag-node, .tnode").forEach((g) =>
      g.addEventListener("click", () => { selectedNode = g.dataset.node; render(); }));
    wireDrawer(terminal, dyn);
  };

  const wireDrawer = (terminal, dyn) => {
    $main.querySelectorAll("[data-output]").forEach((out) => out.addEventListener("click", async () => {
      const target = out.dataset.output || selectedNode;
      try {
        const text = await fetch(`/api/runs/${run.id}/nodes/${target}/output`).then((r) => {
          if (!r.ok) throw new Error("暂无输出文件");
          return r.text();
        });
        const box = $main.querySelector("#node-output") || $main.querySelector("#coord-output");
        if (box) box.innerHTML = `<pre>${esc(text)}</pre>`;
      } catch (e) { toast(e.message); }
    }));
    const retry = $main.querySelector("[data-retry]");
    if (retry) retry.addEventListener("click", async () => {
      try { await api(`/runs/${run.id}/retry/${selectedNode}`, { method: "POST", body: {} }); resub(); }
      catch (e) { toast(e.message); }
    });
    const send = $main.querySelector("[data-send]");
    if (send) send.addEventListener("click", async () => {
      const box = $main.querySelector("#task-msg");
      const text = box.value.trim();
      if (!text) return toast("请填写内容");
      try {
        await api(`/runs/${run.id}/tasks/${selectedNode}/message`, { method: "POST", body: { text } });
        box.value = "";
        toast("已发送");
      } catch (e) { toast(e.message); }
    });
  };

  // SSE subscription; server pushes a full snapshot per engine event.
  let es;
  const resub = () => {
    if (es) es.close();
    es = new EventSource(`/api/runs/${id}/events`);
    es.onmessage = (m) => { run = JSON.parse(m.data); render(); };
  };
  // Only follow a run that can still move. A terminal run's stream closes
  // right after the snapshot, and EventSource would just reconnect in a loop.
  // resub() stays ungated so a retry can pick the stream back up.
  if (!["succeeded", "failed", "canceled", "interrupted"].includes(run.status)) resub();
  cleanup = () => es && es.close();
  render();
}

function renderDrawer(run, nodeID) {
  if (!nodeID || !run.plan) return '<div class="muted" style="font-size:13px">点击 DAG 节点查看详情</div>';
  const node = run.plan.nodes.find((n) => n.id === nodeID);
  const ns = run.nodes[nodeID];
  if (!node || !ns) return "";
  const terminal = ["succeeded", "failed", "canceled", "interrupted"].includes(run.status);
  return `
    <h3>${esc(node.title || node.id)} ${ns.superseded ? '<span class="muted">(已被 replan 取代)</span>' : ""}</h3>
    <div class="muted mono" style="font-size:11px">${esc(node.id)} · agent: ${esc(node.agent)}</div>
    ${ns.status === "running" && ns.activity ? `<div class="summary" style="color:var(--run)">⚙ ${esc(ns.activity)}</div>` : ""}
    <div class="kv">
      <dt>状态</dt><dd>${chip(ns.status, NODE_LABEL)}</dd>
      <dt>尝试</dt><dd class="mono">${ns.attempts || 0}</dd>
      <dt>耗时</dt><dd class="mono">${fmtDur(ns.duration_ms)}</dd>
      <dt>成本</dt><dd class="mono">${fmtCost(ns.cost_usd)}</dd>
      <dt>依赖</dt><dd class="mono">${esc((node.depends_on || []).join(", ") || "—")}</dd>
    </div>
    <div class="muted" style="font-size:12px">任务指令</div>
    <div class="summary">${esc(node.instruction)}</div>
    ${ns.summary ? `<div class="muted" style="font-size:12px">结果摘要</div><div class="summary">${esc(ns.summary)}</div>` : ""}
    ${ns.error ? `<div class="muted" style="font-size:12px">错误</div><div class="summary" style="color:var(--bad)">${esc(ns.error)}</div>` : ""}
    ${ns.artifacts?.length ? `<div class="muted" style="font-size:12px">产物</div><div class="summary mono">${ns.artifacts.map(esc).join("<br>")}</div>` : ""}
    <div class="row" style="margin-top:10px">
      <button class="small" data-output>查看完整输出</button>
      ${terminal ? '<button class="small" data-retry>从此节点重试</button>' : ""}
    </div>
    <div id="node-output"></div>`;
}

// ---------- dynamic mode: task tree ----------

// The proposal is what a human actually signs off on at the initial gate: the
// coordinator's intent, before any agent has been paid to run.
function renderProposal(p) {
  return `
    <div class="panel proposal">
      <h3>⏸ coordinator 提交了首批计划,等待批准</h3>
      <div class="muted" style="font-size:12.5px;white-space:pre-wrap">${esc(p.summary || "")}</div>
      <ol>
        ${(p.tasks || []).map((t) => `
          <li><b>${esc(t.title || "(未命名)")}</b> → <span class="mono">${esc(t.agent)}</span>${modelBadge(t.model)}
            ${t.why ? `<span class="muted"> — ${esc(t.why)}</span>` : ""}</li>`).join("")}
      </ol>
      ${p.agents?.length ? `
        <div class="muted" style="font-size:12px">🧬 计划新建的 agent(批准后永久入池):</div>
        <div class="badges" style="margin-top:6px">
          ${p.agents.map((a) => `<span class="badge"><b>${esc(a.name)}</b> · ${esc(a.model)}</span>`).join("")}
        </div>` : ""}
      <div class="muted" style="font-size:11.5px">批准后 coordinator 可自由追加委派,不再逐条审批——之后由预算护栏兜底。</div>
    </div>`;
}

// Tasks are laid out by lineage indentation rather than as a graph: the shape
// emerges at runtime, so there is no stable geometry worth drawing, and what a
// reader actually needs is "who asked for this".
function renderTaskTree(run, selected) {
  const c = run.coordinator;
  const tasks = (run.task_order || []).map((id) => run.tasks[id]).filter(Boolean);

  const coordCard = c ? `
    <div class="coord">
      <h3>🧭 coordinator ${c.status === "awaiting_user"
        ? '<span class="chip input-required">等待用户回答</span>'
        : chip(c.status === "done" ? "succeeded" : c.status === "failed" ? "failed" : "running")}
        <span class="badge">${esc(c.model)}</span>
        ${c.rounds ? `<span class="badge" title="每轮上下文由任务台账重建,不跨轮累积">第 ${c.rounds} 轮</span>` : ""}
        <span class="badge" title="按 API 牌价折算">est. ${fmtCost(c.cost_usd)}</span></h3>
      ${c.activity ? `<div style="color:var(--run);font-size:12px">⚙ ${esc(c.activity)}</div>` : ""}
      ${c.decision ? `<div class="decision">${esc(c.decision)}</div>` : ""}
      <div class="row" style="margin-top:8px">
        <button class="small" data-output="coordinator">查看 coordinator 完整 transcript</button>
      </div>
      <div id="coord-output"></div>
    </div>` : "";

  if (!tasks.length) {
    return coordCard + '<div class="empty">coordinator 还没有委派任务…</div>';
  }

  const children = {};
  tasks.forEach((t) => (children[t.created_by] = children[t.created_by] || []).push(t));

  const row = (t) => {
    const cls = ["tnode", t.status, selected === t.id ? "selected" : ""].join(" ");
    const sub = t.status === "working" && t.activity
      ? `<span class="tact">⚙ ${esc(t.activity)}</span>`
      : `<span>${esc(TASK_LABEL[t.status] || t.status)}</span>`;
    return `
      <div class="${cls}" data-node="${esc(t.id)}">
        <span class="tdot"></span>
        <span class="ttitle">${esc(t.title || t.id)}</span>
        <span class="tmeta">
          <span class="mono">${esc(t.agent)}</span>
          ${modelBadge(t.model)}
          ${sub}
          ${t.turns > 1 ? `<span>${t.turns} 轮</span>` : ""}
          ${t.duration_ms ? `<span>${fmtDur(t.duration_ms)}</span>` : ""}
        </span>
      </div>`;
  };

  // Roots are whatever the coordinator (or an external A2A caller) created;
  // handoff children nest under the task that asked for them.
  const branch = (parentKey) => {
    const kids = children[parentKey] || [];
    if (!kids.length) return "";
    return kids.map((t) => row(t) + (children[t.id] ? `<div class="tlineage">${branch(t.id)}</div>` : "")).join("");
  };
  const roots = tasks.filter((t) => !run.tasks[t.created_by]);
  const body = roots.map((t) =>
    row(t) + (children[t.id] ? `<div class="tlineage">${branch(t.id)}</div>` : "")).join("");

  return coordCard + `<div class="tree">${body}</div>`;
}

function renderTaskDrawer(run, taskID) {
  const t = taskID && run.tasks ? run.tasks[taskID] : null;
  if (!t) return '<div class="muted" style="font-size:13px">点击任务查看消息往来与产物</div>';
  const live = !["completed", "failed", "canceled"].includes(t.status);
  return `
    <h3>${esc(t.title || t.id)}</h3>
    <div class="muted mono" style="font-size:11px">${esc(t.id)} · agent: ${esc(t.agent)}</div>
    ${t.status === "working" && t.activity ? `<div class="summary" style="color:var(--run)">⚙ ${esc(t.activity)}</div>` : ""}
    <div class="kv">
      <dt>状态</dt><dd>${chip(t.status, TASK_LABEL)}</dd>
      ${t.model ? `<dt>模型</dt><dd class="mono">${esc(t.model)}</dd>` : ""}
      <dt>创建者</dt><dd class="mono">${esc(t.created_by)}</dd>
      <dt>深度</dt><dd class="mono">${t.depth}</dd>
      <dt>轮次</dt><dd class="mono">${t.turns || 0}</dd>
      <dt>耗时</dt><dd class="mono">${fmtDur(t.duration_ms)}</dd>
      <dt>est.成本</dt><dd class="mono">${fmtCost(t.cost_usd)}</dd>
      <dt>tokens</dt><dd class="mono">${fmtTokens(t.usage)}</dd>
    </div>
    <div class="muted" style="font-size:12px">消息往来(完整审计)</div>
    <div class="thread">
      ${(t.messages || []).map((m) => `
        <div class="msg ${esc(m.role)}">
          <div class="who"><b>${esc(MSG_LABEL[m.role] || m.role)}</b>
            <span>${esc(m.from === "coordinator" ? "coordinator" : m.from === "user" ? "你" : m.from.slice(-8))}</span>
            <span>${fmtTime(m.ts)}</span></div>
          <div class="body">${esc(m.text)}</div>
        </div>`).join("") || '<span class="muted" style="font-size:12px">还没有消息</span>'}
    </div>
    ${t.constraints && t.constraints !== "none" ? `<div class="muted" style="font-size:12px">跨域约束(派单时固定)</div><div class="summary">${esc(t.constraints)}</div>` : ""}
    ${t.acceptance?.length ? `<div class="muted" style="font-size:12px">验收契约(引擎执行,worker 自述不作数)</div>
      <div class="summary mono" style="font-size:11.5px">${t.acceptance.map((c) =>
        esc(c.kind === "command" ? `$ ${c.command}` : c.kind === "artifact_contains" ? `${c.path} ~ /${c.pattern}/` : `存在: ${c.path}`)).join("<br>")}</div>` : ""}
    ${t.acceptance_results?.length ? `<div class="muted" style="font-size:12px">验收结果</div>
      <div class="summary mono" style="font-size:11.5px">${t.acceptance_results.map((r) =>
        `${r.passed ? "✓" : "✗"} ${esc(r.detail || r.check.kind)}`).join("<br>")}</div>` : ""}
    ${t.error ? `<div class="muted" style="font-size:12px">错误${t.failure_kind ? `(<span class="mono">${esc(t.failure_kind)}</span>${t.failure_kind === "blocked" ? " · 可返工" : " · 需回规划层"})` : ""}</div><div class="summary" style="color:var(--bad)">${esc(t.error)}</div>` : ""}
    ${t.artifacts?.length ? `<div class="muted" style="font-size:12px">产物</div><div class="summary mono">${t.artifacts.map(esc).join("<br>")}</div>` : ""}
    ${live ? `
      <label class="field" style="margin-top:10px"><span>插话(与 coordinator 的 send_message 走同一条台账)</span>
        <textarea id="task-msg" rows="2" placeholder="回答反问,或调整方向…"></textarea></label>
      <button class="small" data-send>发送给该任务</button>` : ""}
    <div class="row" style="margin-top:10px">
      <button class="small" data-output="${esc(t.id)}">查看完整输出</button>
    </div>
    <div id="node-output"></div>`;
}

// ---------- topology ----------

// A page of its own rather than a panel inside runPage: the canvas engine is
// stateful across snapshots, and runPage replaces its whole innerHTML on every
// SSE frame — that would tear the engine down several times a second. Here only
// the header, the HUD and the side panel are re-rendered; the canvas is handed
// each snapshot and diffs it itself.
async function topologyPage(id) {
  let run;
  try { run = await api("/runs/" + id); }
  catch (e) { $main.innerHTML = `<div class="empty">运行不存在:${esc(e.message)}</div>`; return; }

  const dyn = run.mode === "dynamic";
  const legend = ["working", "input-required", "submitted", "completed", "failed"]
    .map((st) => `<span class="chip ${st}">${esc(TASK_LABEL[st] || st)}</span>`).join("");

  $main.innerHTML = `
    <div class="page-head" id="topo-head"></div>
    ${runTabs(id, "topology")}
    ${dyn ? `
    <div class="topo-panel">
      <div class="topo-stage" id="topo-stage">
        <canvas id="topo-canvas"></canvas>
        <div class="topo-tip" id="topo-tip" hidden></div>
        <div class="topo-legend" id="topo-legend">${legend}</div>
        <div class="topo-hud" id="topo-hud"></div>
      </div>
      <div class="panel topo-side" id="topo-side"></div>
    </div>`
    : '<div class="empty">拓扑画的是 coordinator 在运行时委派出来的网络。static 模式的形状在批准那一刻就固定了,详情页的 DAG 画得更准。</div>'}`;

  const $head = document.getElementById("topo-head");
  const updateTopoHeader = () => {
    $head.innerHTML = `
      <h1>${esc(run.workflow_name)}</h1>
      <span class="badge">${esc(run.mode || "static")}</span>
      ${chip(run.status)}
      <span class="topo-stat" title="按 Claude API 牌价折算,非实际账单">est. <b class="mono">${fmtCost(run.cost_usd)}</b></span>
      <span class="topo-stat">耗时 <b class="mono">${runDuration(run)}</b></span>`;
  };
  updateTopoHeader();
  if (!dyn) return;

  const $hud = document.getElementById("topo-hud");
  const $side = document.getElementById("topo-side");
  let sel = null;      // {kind,label,taskIds} — whatever node the canvas selected
  let selTask = null;  // a task drilled into from that node's list

  const updateHud = () => {
    const tasks = Object.values(run.tasks || {});
    const n = (st) => tasks.filter((t) => t.status === st).length;
    const c = run.coordinator || {};
    $hud.innerHTML = `
      <div>🧭 ${c.rounds ? `第 ${c.rounds} 轮` : "未启动"}${c.activity ? " · " + esc(c.activity) : ""}</div>
      <div class="mono">${tasks.length} 任务 · 执行中 ${n("working")} · 待答复 ${n("input-required")} · 完成 ${n("completed")} · 失败 ${n("failed")}</div>`;
  };

  const coordSide = () => {
    const c = run.coordinator || {};
    return `
      <h3>🧭 coordinator</h3>
      <div class="muted mono" style="font-size:11px">${esc(c.model || "—")}</div>
      ${c.activity ? `<div class="summary" style="color:var(--run)">⚙ ${esc(c.activity)}</div>` : ""}
      <div class="kv">
        <dt>状态</dt><dd>${c.status === "awaiting_user"
          ? '<span class="chip input-required">等待用户回答</span>'
          : chip(c.status === "done" ? "succeeded" : c.status === "failed" ? "failed" : "running")}</dd>
        <dt>轮次</dt><dd class="mono" title="每轮上下文由任务台账重建,不跨轮累积">${c.rounds || 0}</dd>
        <dt>已委派</dt><dd class="mono">${Object.keys(run.tasks || {}).length}</dd>
        <dt>est.成本</dt><dd class="mono">${fmtCost(c.cost_usd)}</dd>
      </div>
      ${c.decision ? `<div class="muted" style="font-size:12px">最近决策</div><div class="summary">${esc(c.decision)}</div>` : ""}`;
  };

  const userSide = () => {
    const chat = run.chat || [];
    return `
      <h3>你</h3>
      <div class="muted" style="font-size:11.5px">与 main agent 的会话 · ${chat.length} 条</div>
      <div class="thread">
        ${chat.slice(-12).map((m) => `
          <div class="msg">
            <div class="who"><b>${m.from === "user" ? "你" : "main agent"}</b><span>${fmtTime(m.ts)}</span></div>
            <div class="body">${esc(m.text)}</div>
          </div>`).join("") || '<span class="muted" style="font-size:12px">还没有对话</span>'}
      </div>
      <div class="row" style="margin-top:10px">
        <button class="small" onclick="openSession('${esc(run.workflow_id)}','${esc(run.id)}')">💬 打开会话</button>
      </div>`;
  };

  const agentSide = () => {
    const tasks = (sel.taskIds || []).map((tid) => (run.tasks || {})[tid]).filter(Boolean);
    return `
      <h3>${esc(sel.label)}</h3>
      <div class="muted" style="font-size:11.5px">${tasks.length} 个任务 · 点一条看完整往来</div>
      <div class="tree" style="margin-top:8px">
        ${tasks.map((t) => `
          <div class="tnode ${esc(t.status)}" data-task="${esc(t.id)}">
            <span class="tdot"></span>
            <span class="ttitle">${esc(t.title || t.id)}</span>
            <span class="tmeta">
              ${modelBadge(t.model)}
              <span>${esc(TASK_LABEL[t.status] || t.status)}</span>
              ${t.duration_ms ? `<span>${fmtDur(t.duration_ms)}</span>` : ""}
            </span>
          </div>`).join("") || '<div class="muted" style="font-size:12px">还没有任务</div>'}
      </div>`;
  };

  const sideHTML = () => {
    if (selTask && (run.tasks || {})[selTask]) {
      return `<button class="small" data-back>‹ 返回 ${esc(sel ? sel.label : "节点")}</button>`
        + renderTaskDrawer(run, selTask);
    }
    if (!sel) return '<div class="muted" style="font-size:13px">点击节点:看它承担的任务、消息往来与产物</div>';
    if (sel.kind === "coordinator") return coordSide();
    if (sel.kind === "user") return userSide();
    return agentSide();
  };

  // Re-rendered on every SSE push, so a half-typed reply has to survive it.
  const renderSide = () => {
    const draft = $side.querySelector("#task-msg")?.value;
    $side.innerHTML = sideHTML();
    if (draft) {
      const box = $side.querySelector("#task-msg");
      if (box) box.value = draft;
    }
    $side.querySelectorAll("[data-task]").forEach((el) =>
      el.addEventListener("click", () => { selTask = el.dataset.task; renderSide(); }));
    const back = $side.querySelector("[data-back]");
    if (back) back.addEventListener("click", () => { selTask = null; renderSide(); });
    const out = $side.querySelector("[data-output]");
    if (out) out.addEventListener("click", async () => {
      try {
        const text = await fetch(`/api/runs/${run.id}/nodes/${out.dataset.output}/output`).then((r) => {
          if (!r.ok) throw new Error("暂无输出文件");
          return r.text();
        });
        const box = $side.querySelector("#node-output");
        if (box) box.innerHTML = `<pre>${esc(text)}</pre>`;
      } catch (e) { toast(e.message); }
    });
    const send = $side.querySelector("[data-send]");
    if (send) send.addEventListener("click", async () => {
      const box = $side.querySelector("#task-msg");
      const text = box.value.trim();
      if (!text) return toast("请填写内容");
      try {
        await api(`/runs/${run.id}/tasks/${selTask}/message`, { method: "POST", body: { text } });
        box.value = "";
        toast("已发送");
      } catch (e) { toast(e.message); }
    });
  };

  const topo = LoomTopology.create(document.getElementById("topo-canvas"), {
    tip: document.getElementById("topo-tip"),
    // The canvas hands back its own node id ("agent:x"), never a task id — the
    // task list has to come from info.taskIds.
    onSelect: (nodeId, info) => { sel = info; selTask = null; renderSide(); },
  });
  topo.ingest(run, { initial: true });
  updateHud();
  renderSide();

  // A finished run has nothing left to push; skip the stream entirely.
  const terminal = ["succeeded", "failed", "canceled", "interrupted"].includes(run.status);
  if (!terminal) {
    const es = new EventSource(`/api/runs/${id}/events`);
    es.onmessage = (m) => {
      run = JSON.parse(m.data);
      topo.ingest(run);
      updateTopoHeader();
      updateHud();
      renderSide();
    };
    cleanup = () => { es.close(); topo.destroy(); };
  } else {
    cleanup = () => { topo.destroy(); };
  }
}

// ---------- DAG rendering ----------

function renderDag(run, selected) {
  const nodes = run.plan.nodes;
  const byId = Object.fromEntries(nodes.map((n) => [n.id, n]));
  // depth = longest dependency chain
  const depth = {};
  const calcDepth = (id, seen = new Set()) => {
    if (id in depth) return depth[id];
    if (seen.has(id)) return 0;
    seen.add(id);
    const deps = byId[id]?.depends_on || [];
    depth[id] = deps.length ? Math.max(...deps.map((d) => calcDepth(d, seen))) + 1 : 0;
    return depth[id];
  };
  nodes.forEach((n) => calcDepth(n.id));

  const cols = {};
  nodes.forEach((n) => (cols[depth[n.id]] = cols[depth[n.id]] || []).push(n));
  const W = 190, H = 62, HG = 72, VG = 22, PAD = 16;
  const pos = {};
  const maxRows = Math.max(...Object.values(cols).map((c) => c.length));
  Object.entries(cols).forEach(([d, colNodes]) => {
    const offset = ((maxRows - colNodes.length) * (H + VG)) / 2;
    colNodes.forEach((n, i) => {
      pos[n.id] = { x: PAD + d * (W + HG), y: PAD + offset + i * (H + VG) };
    });
  });
  const width = PAD * 2 + (Object.keys(cols).length - 1) * (W + HG) + W;
  const height = PAD * 2 + maxRows * (H + VG) - VG;

  const edges = [];
  nodes.forEach((n) =>
    (n.depends_on || []).forEach((d) => {
      const a = pos[d], b = pos[n.id];
      if (!a || !b) return;
      const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2;
      const mx = (x1 + x2) / 2;
      edges.push(`<path class="dag-edge" d="M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}"/>
        <circle class="dag-dot" cx="${x2}" cy="${y2}" r="2.5"/>`);
    }));

  const boxes = nodes.map((n) => {
    const ns = run.nodes[n.id] || { status: "pending" };
    const p = pos[n.id];
    const cls = ["dag-node", ns.status, ns.superseded ? "superseded" : "", selected === n.id ? "selected" : ""].join(" ");
    const title = (n.title || n.id).length > 20 ? (n.title || n.id).slice(0, 19) + "…" : (n.title || n.id);
    let sub = `${n.agent} · ${NODE_LABEL[ns.status] || ns.status}`;
    if (ns.status === "running" && ns.activity) {
      sub = `⚙ ${ns.activity.length > 24 ? ns.activity.slice(0, 23) + "…" : ns.activity}`;
    } else if (ns.duration_ms) {
      sub += ` · ${fmtDur(ns.duration_ms)}`;
    }
    return `
      <g class="${cls}" data-node="${esc(n.id)}" transform="translate(${p.x},${p.y})">
        <rect width="${W}" height="${H}" rx="10"/>
        <circle class="statusdot ${ns.status}" cx="16" cy="21" r="4.5"/>
        <text x="30" y="25" font-weight="600">${esc(title)}</text>
        <text x="30" y="44" class="sub">${esc(sub)}</text>
      </g>`;
  });

  return `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" xmlns="http://www.w3.org/2000/svg">
    ${edges.join("")}${boxes.join("")}</svg>`;
}

// ---------- agents ----------

async function agentsPage() {
  const [agents, costs, amendments] = await Promise.all([
    api("/agents"),
    api("/costs/summary?by=agent").catch(() => ({ agents: [] })),
    api("/amendments").catch(() => []),
  ]);
  // Cumulative spend per agent across every run it has ever served — the
  // cross-workflow view the run pages structurally cannot give.
  const spend = Object.fromEntries((costs.agents || []).map((b) => [b.key, b]));
  // Pending amendments: a coordinator's proposed revision of an agent's
  // definition. Nothing applies without the approve button below — that click
  // IS the security boundary between "agent surfaced evidence" and "identity
  // changed".
  const pending = (amendments || []).filter((am) => am.status === "pending");
  const amendCard = (am) => `
    <div class="panel" style="border-left:3px solid var(--warn)">
      <div class="row" style="align-items:baseline;gap:8px">
        <b>${esc(am.agent)}</b>
        <span class="muted" style="font-size:12px">main agent 提议修订该 agent 的 system prompt · ${fmtTime(am.created_at)}${am.run_id ? ` · 来自 <a href="#/runs/${esc(am.run_id)}" class="mono" style="font-size:11px">${esc(am.run_id)}</a>` : ""}</span>
      </div>
      <div style="font-size:13px;margin:8px 0"><b>理由:</b>${esc(am.rationale)}</div>
      <details style="margin-bottom:8px"><summary style="cursor:pointer;font-size:12.5px">当前 prompt(修订将整体替换它)</summary>
        <pre style="white-space:pre-wrap;font-size:12px;max-height:220px;overflow:auto">${esc(am.current)}</pre></details>
      <details open style="margin-bottom:10px"><summary style="cursor:pointer;font-size:12.5px">提议的新 prompt</summary>
        <pre style="white-space:pre-wrap;font-size:12px;max-height:220px;overflow:auto">${esc(am.proposed)}</pre></details>
      <div class="row">
        <button class="small primary" data-am-approve="${esc(am.id)}">✓ 批准并应用</button>
        <button class="small danger" data-am-reject="${esc(am.id)}">✗ 拒绝</button>
      </div>
    </div>`;
  $main.innerHTML = `
    <div class="page-head">
      <h1>Agent 池 <span class="muted" style="font-size:13px">(${agents.length} 个可复用 executor)</span></h1>
      <button class="primary" id="ag-new">+ 新建 Agent</button>
    </div>
    ${pending.length ? `
    <div style="margin-bottom:14px">
      <div class="muted" style="font-size:12.5px;margin-bottom:8px">📝 待审修订提案(${pending.length})— agent 永远不能自改定义;批准才会生效,并同步重生成 AGENTS.md</div>
      ${pending.map(amendCard).join("")}
    </div>` : ""}
    <div class="grid">
      ${agents.map((a) => `
        <div class="panel agent-card">
          <h3>${esc(a.name)} <span class="badge">${esc(a.model || "sonnet")}</span></h3>
          <div class="desc">${esc(a.description || "")}</div>
          <div class="badges">
            ${a.tools ? a.tools.split(",").map((t) => `<span class="badge">${esc(t.trim())}</span>`).join("") : '<span class="badge">无工具</span>'}
            ${a.skills?.length ? `<span class="badge" style="color:var(--accent)">skills × ${a.skills.length}</span>` : ""}
            ${spend[a.name] ? `<span class="badge" title="历史全部执行的 API 等价成本">累计 est. ${fmtCost(spend[a.name].cost_usd)} · ${spend[a.name].units} 次</span>` : ""}
          </div>
          <div class="row">
            <button class="small" data-edit="${esc(a.name)}">编辑</button>
            <button class="small danger" data-del="${esc(a.name)}">删除</button>
          </div>
        </div>`).join("")}
    </div>`;
  $main.querySelectorAll("[data-am-approve]").forEach((b) =>
    b.addEventListener("click", async () => {
      if (!(await confirmModal("批准修订", `agent「${pending.find((x) => x.id === b.dataset.amApprove)?.agent}」的 system prompt 将被提议版本整体替换。`, "批准并应用"))) return;
      try { await api(`/amendments/${b.dataset.amApprove}/approve`, { method: "POST", body: {} }); toast("已应用"); }
      catch (e) { toast(e.message); }
      agentsPage();
    }));
  $main.querySelectorAll("[data-am-reject]").forEach((b) =>
    b.addEventListener("click", async () => {
      try { await api(`/amendments/${b.dataset.amReject}/reject`, { method: "POST", body: {} }); toast("已拒绝"); }
      catch (e) { toast(e.message); }
      agentsPage();
    }));
  $main.querySelector("#ag-new").onclick = () => agentModal(null, agents);
  $main.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => agentModal(agents.find((a) => a.name === b.dataset.edit), agents)));
  $main.querySelectorAll("[data-del]").forEach((b) =>
    b.addEventListener("click", async () => {
      if (!(await confirmModal("删除 Agent", `agent「${b.dataset.del}」将从池中移除。`, "删除"))) return;
      await api("/agents/" + b.dataset.del, { method: "DELETE" });
      agentsPage();
    }));
}

// The workflow's retrospective panel, two tiers with different fates:
// behavior rules (lessons) — proposed by the coordinator from postmortems,
// injected into future runs ONLY once the user confirms each — and per-run
// retrospective records (run.feedback), which are archives: readable,
// editable, never injected. Confirmation is the only path from a
// retrospective to the next run's prompt.
async function feedbackModal(wf) {
  let runs = [], lessons = [];
  try { runs = await api("/runs?workflow_id=" + wf.id); } catch {}
  try { lessons = await api("/lessons?workflow_id=" + wf.id); } catch {}
  const records = runs.filter((r) => r.feedback); // newest first
  const pending = lessons.filter((l) => l.status === "pending");
  const approved = lessons.filter((l) => l.status === "approved");
  const injectN = meta.max_lessons || 20;
  const fmtDay = (t) => new Date(t).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
  const runLink = (id) => id ? `<a href="#/runs/${esc(id)}" class="mono" style="font-size:11px" onclick="document.getElementById('overlay').innerHTML=''">来源 ${esc(id.slice(-8))}</a>` : "";

  // A pending proposal comes in three shapes: a plain new rule, a replacement
  // (old rules shown struck through above the new text), and a pure
  // retirement (old rules go, nothing added).
  const pendingItem = (l) => {
    const isRepl = (l.replaces || []).length > 0;
    const retire = isRepl && !l.text;
    return `
    <div style="border:1px solid var(--accent);border-radius:10px;padding:10px 12px;margin-bottom:10px" data-ls="${esc(l.id)}">
      <div class="row" style="align-items:baseline;gap:8px;margin-bottom:6px">
        <span class="badge" style="color:var(--accent)">${retire ? "提议退役" : isRepl ? "提议替换" : "待确认"}</span>
        ${runLink(l.run_id)}
        <span class="muted" style="font-size:12px">${fmtDay(l.created_at)}</span>
      </div>
      ${isRepl ? `<div style="font-size:12.5px;margin-bottom:4px">${(l.replaced_texts || []).map((t) =>
        `<div class="muted" style="text-decoration:line-through;white-space:pre-wrap">${esc(t)}</div>`).join("")}</div>` : ""}
      ${retire
        ? '<div class="muted" style="font-size:12.5px">退役后这些规范不再注入,也没有替代条目。</div>'
        : `${isRepl ? '<div class="muted" style="font-size:11px;margin-bottom:2px">↓ 替换为</div>' : ""}
      <div class="ls-text" style="font-size:13px;white-space:pre-wrap">${esc(l.text)}</div>
      <textarea class="ls-edit" rows="2" style="width:100%;resize:vertical;display:none">${esc(l.text)}</textarea>`}
      <div class="row" style="margin-top:8px">
        <button class="small primary" data-ls-approve>✓ ${retire ? "确认退役" : isRepl ? "替换并采纳" : "采纳(注入之后的 run)"}</button>
        ${retire ? "" : '<button class="small" data-ls-edit>改后采纳</button>'}
        <button class="small danger" data-ls-reject>✗ 不采纳</button>
      </div>
    </div>`;
  };
  const approvedItem = (l, i) => `
    <div style="border:1px solid var(--border);border-radius:10px;padding:10px 12px;margin-bottom:10px" data-ls="${esc(l.id)}">
      <div class="row" style="align-items:baseline;gap:8px;margin-bottom:6px">
        ${i < injectN
          ? '<span class="badge" style="color:var(--accent)" title="注入之后每次 run 的开局(main agent 可见,worker 不可见)">注入中</span>'
          : `<span class="badge" title="超出注入上限 ${injectN} 条,最新的优先">超限存档</span>`}
        ${l.run_id ? runLink(l.run_id) : '<span class="muted" style="font-size:11px">手动添加</span>'}
        <span class="muted" style="font-size:12px">${fmtDay(l.decided_at || l.created_at)}</span>
      </div>
      <div class="ls-text" style="font-size:13px;white-space:pre-wrap">${esc(l.text)}</div>
      <textarea class="ls-edit" rows="2" style="width:100%;resize:vertical;display:none">${esc(l.text)}</textarea>
      <div class="row" style="margin-top:8px">
        <button class="small" data-ls-edit>编辑</button>
        <button class="small primary" data-ls-save style="display:none">保存</button>
        <button class="small danger" data-ls-del>移除</button>
      </div>
    </div>`;
  const recordItem = (r) => `
    <div style="border:1px solid var(--border);border-radius:10px;padding:10px 12px;margin-bottom:10px" data-fb="${esc(r.id)}">
      <div class="row" style="align-items:baseline;gap:8px;margin-bottom:6px">
        <a href="#/runs/${esc(r.id)}" class="mono" style="font-size:11px" onclick="document.getElementById('overlay').innerHTML=''">${esc(r.id)}</a>
        <span class="muted" style="font-size:12px">${fmtDay(r.feedback_at || r.created_at)} · ${esc((r.goal || "").replace(/\s+/g, " ").slice(0, 24))}</span>
      </div>
      <div class="fb-text" style="font-size:13px;white-space:pre-wrap">${esc(r.feedback)}</div>
      <textarea class="fb-edit" rows="2" style="width:100%;resize:vertical;display:none">${esc(r.feedback)}</textarea>
      <div class="row" style="margin-top:8px">
        <button class="small" data-fb-edit>编辑</button>
        <button class="small primary" data-fb-save style="display:none">保存</button>
        <button class="small danger" data-fb-clear>清除</button>
      </div>
    </div>`;

  $overlay.innerHTML = `
    <div class="modal-bg">
      <div class="modal">
        <h2>复盘 · ${esc(wf.name)}</h2>
        <div class="muted" style="font-size:12.5px;margin-bottom:12px">复盘产出分两层:<b>行为规范</b>是从复盘里提炼的具体做法,经你确认后注入之后每次 run 的开局;<b>复盘记录</b>是每次 run 的结论存档,只供查阅,永不注入。</div>
        ${pending.length ? `<h3 style="margin:14px 0 8px">待确认的行为规范</h3>${pending.map(pendingItem).join("")}` : ""}
        <h3 style="margin:14px 0 8px">生效中的规范(注入每次 run)</h3>
        ${approved.length >= (meta.lessons_nudge || 12) ? `
        <div style="border:1px solid var(--accent);border-radius:10px;padding:8px 12px;margin-bottom:10px;font-size:12.5px">
          生效规范已有 ${approved.length} 条,重复或矛盾的条目会拖累每一个 run 的开局 —— 建议发起一次整理:
          main agent 通读全部规范,提出合并/改写/退役方案,仍然逐条经你确认。
          <div class="row" style="margin-top:6px"><button class="small primary" id="ls-consolidate">发起整理</button></div>
        </div>` : ""}
        ${approved.map(approvedItem).join("") || '<div class="empty" style="padding:10px">还没有生效的规范 — 复盘后 main agent 会提炼并送来待确认,也可以在下面手动添加。</div>'}
        <div class="row" style="margin-top:8px">
          <input id="ls-new" placeholder="手动添加一条规范(立即生效),例:报告先给结论再给论证" style="flex:1">
          <button class="small primary" id="ls-add">添加</button>
        </div>
        <h3 style="margin:18px 0 8px">复盘记录(存档,不注入)</h3>
        ${records.map(recordItem).join("") || '<div class="empty" style="padding:10px">还没有复盘记录 — 会话结束后在会话尾部「发起复盘」。</div>'}
        <div class="row modal-foot"><button id="fb-close">关闭</button></div>
      </div>
    </div>`;
  $overlay.querySelector("#fb-close").onclick = () => ($overlay.innerHTML = "");
  $overlay.querySelector(".modal-bg").addEventListener("click", (e) => {
    if (e.target.classList.contains("modal-bg")) $overlay.innerHTML = "";
  });

  const bCons = $overlay.querySelector("#ls-consolidate");
  if (bCons) bCons.onclick = async () => {
    try {
      await api(`/workflows/${wf.id}/lessons/consolidate`, { method: "POST", body: {} });
      $overlay.innerHTML = "";
      toast("已唤醒 main agent 整理规范;提案会回到「复盘」面板,逐条经你确认");
    } catch (e) { toast(e.message); }
  };

  $overlay.querySelector("#ls-add").onclick = async () => {
    const v = $overlay.querySelector("#ls-new").value.trim();
    if (!v) return toast("先写下规范内容");
    try {
      await api(`/workflows/${wf.id}/lessons`, { method: "POST", body: { text: v } });
      toast("已添加并生效");
      feedbackModal(wf);
    } catch (e) { toast(e.message); }
  };

  $overlay.querySelectorAll("[data-ls]").forEach((box) => {
    const id = box.dataset.ls;
    const text = box.querySelector(".ls-text"), edit = box.querySelector(".ls-edit");
    const bE = box.querySelector("[data-ls-edit]"), bS = box.querySelector("[data-ls-save]");
    const toggleEdit = () => {
      const on = edit.style.display === "none";
      edit.style.display = on ? "" : "none";
      text.style.display = on ? "none" : "";
      if (bS) bS.style.display = on ? "" : "none";
      return on;
    };
    const decide = async (approve, textOverride) => {
      try {
        if (textOverride !== undefined) await api(`/lessons/${id}`, { method: "PUT", body: { text: textOverride } });
        await api(`/lessons/${id}/${approve ? "approve" : "reject"}`, { method: "POST", body: {} });
        toast(approve ? "已确认" : "已拒绝");
        feedbackModal(wf);
      } catch (e) { toast(e.message); }
    };
    const bApprove = box.querySelector("[data-ls-approve]");
    if (bApprove) {
      // Pending proposal: approve as-is, approve edited, or reject. A
      // retirement has no text/edit elements — approve deletes its targets.
      bApprove.onclick = () => {
        const editing = edit && edit.style.display !== "none";
        const v = edit ? edit.value.trim() : "";
        if (editing && !v) return toast("内容为空");
        decide(true, editing && v !== text.textContent ? v : undefined);
      };
      if (bE) bE.onclick = () => { bE.textContent = toggleEdit() ? "收起" : "改后采纳"; };
      box.querySelector("[data-ls-reject]").onclick = () => decide(false);
      return;
    }
    // Approved rule: edit in place or remove from the injection set.
    bE.onclick = () => { bE.textContent = toggleEdit() ? "取消" : "编辑"; };
    bS.onclick = async () => {
      const v = edit.value.trim();
      if (!v) return toast("内容为空 — 要移除请用「移除」");
      try {
        await api(`/lessons/${id}`, { method: "PUT", body: { text: v } });
        toast("已更新");
        feedbackModal(wf);
      } catch (e) { toast(e.message); }
    };
    box.querySelector("[data-ls-del]").onclick = async () => {
      if (!(await confirmModal("移除规范", "这条规范将不再注入之后的 run。", "移除"))) return;
      try {
        await api(`/lessons/${id}`, { method: "DELETE" });
        toast("已移除");
        feedbackModal(wf);
      } catch (e) { toast(e.message); }
    };
  });

  $overlay.querySelectorAll("[data-fb]").forEach((box) => {
    const id = box.dataset.fb;
    const text = box.querySelector(".fb-text"), edit = box.querySelector(".fb-edit");
    const bE = box.querySelector("[data-fb-edit]"), bS = box.querySelector("[data-fb-save]");
    bE.onclick = () => {
      const on = edit.style.display === "none";
      edit.style.display = on ? "" : "none";
      text.style.display = on ? "none" : "";
      bS.style.display = on ? "" : "none";
      bE.textContent = on ? "取消" : "编辑";
    };
    bS.onclick = async () => {
      const v = edit.value.trim();
      if (!v) return toast("内容为空 — 要移除请用「清除」");
      try {
        await api(`/runs/${id}/feedback`, { method: "POST", body: { text: v, direct: true } });
        toast("已更新");
        feedbackModal(wf);
      } catch (e) { toast(e.message); }
    };
    box.querySelector("[data-fb-clear]").onclick = async () => {
      if (!(await confirmModal("清除复盘记录", "仅删除该 run 的复盘记录存档(不删除 run 本身,也不影响已确认的规范)。", "清除"))) return;
      try {
        await api(`/runs/${id}/feedback`, { method: "POST", body: { text: "", direct: true } });
        toast("已清除");
        feedbackModal(wf);
      } catch (e) { toast(e.message); }
    };
  });
}

function agentModal(agent, _all) {
  const a = agent || { name: "", description: "", runtime: "", model: "sonnet", tools: "", max_turns: 0, system_prompt: "" };
  $overlay.innerHTML = `
    <div class="modal-bg">
      <div class="modal">
        <h2>${agent ? "编辑" : "新建"} Agent</h2>
        <label class="field"><span>名称(唯一,作为池注册名)</span>
          <input id="am-name" value="${esc(a.name)}" ${agent ? "disabled" : ""}></label>
        <label class="field"><span>描述(planner 依据它选择该 agent,写清能力边界)</span>
          <input id="am-desc" value="${esc(a.description)}"></label>
        <div class="row">
          <label class="field" style="flex:1"><span title="该 agent 的会话由谁托管">运行时</span>
            <select id="am-runtime" title="该 agent 的会话由谁托管">${meta.runtimes.map((r) =>
              `<option value="${esc(r.id)}" ${(a.runtime || meta.default_runtime) === r.id ? "selected" : ""}>${esc(r.label)}</option>`).join("")}
            </select></label>
          <label class="field" style="flex:1"><span>模型(worker 上限 opus;Fable 仅 main agent)</span>${modelSelect("am-model", a.model, { worker: true })}</label>
          <label class="field" style="flex:1"><span>max turns(0=默认)</span><input id="am-turns" type="number" value="${a.max_turns || 0}"></label>
        </div>
        <label class="field"><span>允许的工具(逗号分隔,空=纯文本推理)</span>
          <input id="am-tools" value="${esc(a.tools)}" placeholder="Read,Write,Edit,Bash"></label>
        <label class="check" style="margin-bottom:10px" title="独立校验者只收需求、验收标准与产物路径;派单时禁止 context_hint,static 模式不注入上游自述摘要">
          <input type="checkbox" id="am-independent" ${a.independent ? "checked" : ""}> 独立校验者(fresh context:不接收作者叙述,只凭产物评审)</label>
        <label class="field"><span>System prompt(保存后生成到该 agent home 的 AGENTS.md)</span>
          <textarea id="am-sp" rows="7">${esc(a.system_prompt)}</textarea></label>
        ${agent ? `
        <label class="field"><span>私有 skills(home/.claude/skills/,ACP 会话自动加载)</span></label>
        <div class="badges" style="margin-bottom:8px">
          ${(a.skills || []).map((sk) => `
            <span class="badge">${esc(sk)}
              <a href="javascript:void 0" data-skill-edit="${esc(sk)}">编辑</a>
              <a href="javascript:void 0" data-skill-del="${esc(sk)}" style="color:var(--bad)">×</a>
            </span>`).join("") || '<span class="muted" style="font-size:12px">还没有 skill</span>'}
        </div>
        <div class="row" style="margin-bottom:14px">
          <input id="am-skill-name" placeholder="skill 名称(kebab-case)" style="flex:1">
          <button class="small" id="am-skill-add">+ 新增 skill</button>
        </div>` : ""}
        <div class="row modal-foot">
          <button id="am-cancel">取消</button>
          <button class="primary" id="am-save">保存</button>
        </div>
      </div>
    </div>`;
  if (agent) {
    $overlay.querySelectorAll("[data-skill-edit]").forEach((el) =>
      el.addEventListener("click", () => skillEditor(a.name, el.dataset.skillEdit)));
    $overlay.querySelectorAll("[data-skill-del]").forEach((el) =>
      el.addEventListener("click", async () => {
        if (!(await confirmModal("删除 Skill", `skill「${el.dataset.skillDel}」将从该 agent 的私有目录移除。`, "删除"))) return;
        await api(`/agents/${a.name}/skills/${el.dataset.skillDel}`, { method: "DELETE" });
        reopenAgent(a.name);
      }));
    $overlay.querySelector("#am-skill-add").onclick = () => {
      const name = $overlay.querySelector("#am-skill-name").value.trim();
      if (!name) return toast("请填写 skill 名称");
      skillEditor(a.name, name, true);
    };
  }
  $overlay.querySelector("#am-cancel").onclick = () => ($overlay.innerHTML = "");
  $overlay.querySelector("#am-save").onclick = async () => {
    try {
      await api("/agents", {
        method: "POST",
        body: {
          name: $overlay.querySelector("#am-name").value.trim(),
          description: $overlay.querySelector("#am-desc").value.trim(),
          runtime: $overlay.querySelector("#am-runtime").value,
          model: $overlay.querySelector("#am-model").value.trim(),
          tools: $overlay.querySelector("#am-tools").value.trim(),
          max_turns: +$overlay.querySelector("#am-turns").value,
          independent: $overlay.querySelector("#am-independent").checked,
          system_prompt: $overlay.querySelector("#am-sp").value,
        },
      });
      $overlay.innerHTML = "";
      agentsPage();
    } catch (e) { toast("保存失败:" + e.message); }
  };
}

async function reopenAgent(name) {
  try {
    const fresh = await api("/agents/" + name);
    agentModal(fresh);
    agentsPage();
  } catch { $overlay.innerHTML = ""; }
}

// skillEditor edits one skill's SKILL.md; on save returns to the agent modal.
async function skillEditor(agentName, skillName, isNew) {
  let content = `---\nname: ${skillName}\ndescription: 何时使用这个 skill\n---\n\n# ${skillName}\n\n步骤:\n1. …\n`;
  if (!isNew) {
    try { content = (await api(`/agents/${agentName}/skills/${skillName}`)).content; }
    catch (e) { return toast(e.message); }
  }
  $overlay.innerHTML = `
    <div class="modal-bg">
      <div class="modal" style="width:640px">
        <h2>${esc(agentName)} / skills / ${esc(skillName)}</h2>
        <label class="field"><span>SKILL.md(frontmatter 的 description 决定何时被触发)</span>
          <textarea id="sk-content" rows="16" class="mono">${esc(content)}</textarea></label>
        <div class="row modal-foot">
          <button id="sk-back">返回</button>
          <button class="primary" id="sk-save">保存</button>
        </div>
      </div>
    </div>`;
  $overlay.querySelector("#sk-back").onclick = () => reopenAgent(agentName);
  $overlay.querySelector("#sk-save").onclick = async () => {
    try {
      await api(`/agents/${agentName}/skills/${skillName}`, {
        method: "POST",
        body: { content: $overlay.querySelector("#sk-content").value },
      });
      reopenAgent(agentName);
    } catch (e) { toast("保存失败:" + e.message); }
  };
}

router();
