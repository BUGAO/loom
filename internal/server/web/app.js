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

function toast(msg) {
  const el = document.createElement("div");
  el.className = "toast";
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 4200);
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
function modelSelect(id, current) {
  const models = [...meta.models];
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
  const hash = location.hash || "#/workflows";
  const parts = hash.slice(2).split("/").filter(Boolean); // e.g. ["runs","run_x"]
  const section = parts[0] || "workflows";
  document.querySelectorAll("[data-nav]").forEach((a) =>
    a.classList.toggle("active", a.dataset.nav === section));
  // The conversation page is the one full-bleed surface; everything else
  // keeps the readable 1280px column.
  $main.classList.toggle("wide", section === "workflows" && !parts[1]);
  if (section === "workflows" && parts[1] === "new") return wfEditPage(null);
  if (section === "workflows" && parts[2] === "edit") return wfEditPage(parts[1]);
  if (section === "workflows") return wfListPage();
  if (section === "runs" && parts[1]) return runPage(parts[1]);
  if (section === "runs") return runsListPage();
  if (section === "agents") return agentsPage();
  wfListPage();
}
window.addEventListener("hashchange", router);

// ---------- workflows: list + conversation ----------

// The workflow page is a conversation surface: pick a workflow on the left,
// talk to its main agent on the right. The main agent decomposes, delegates,
// and reports back; the runtime status above the chat is the live task tree.
async function wfListPage() {
  const wfs = await api("/workflows");
  let selId = sessionStorage.getItem("wfSel");
  if (!wfs.some((w) => w.id === selId)) selId = wfs[0]?.id || null;
  let run = null; // the selected workflow's active (or latest) run
  let es = null;

  const terminalRun = (r) => ["succeeded", "failed", "canceled", "interrupted"].includes(r.status);
  const selWf = () => wfs.find((w) => w.id === selId);

  const loadRun = async () => {
    run = null;
    if (!selId) return;
    try {
      const runs = await api("/runs?workflow_id=" + selId); // newest first
      const pick = runs.find((r) => !terminalRun(r)) || runs[0];
      if (pick) run = await api("/runs/" + pick.id);
    } catch { /* no runs yet */ }
  };

  const resub = () => {
    if (es) { es.close(); es = null; }
    if (!run) return;
    es = new EventSource(`/api/runs/${run.id}/events`);
    es.onmessage = (m) => { run = JSON.parse(m.data); renderRight(); };
  };
  cleanup = () => es && es.close();

  const renderLeft = () => {
    const list = $main.querySelector("#wf-list");
    if (!list) return;
    list.innerHTML = wfs.map((w) => `
      <div class="wf-item ${w.id === selId ? "selected" : ""}" data-wf="${esc(w.id)}">
        <div class="wf-item-head">
          <b>${esc(w.name)}</b>
          <span class="badge" style="color:${w.mode === "dynamic" ? "var(--accent)" : "var(--muted)"}">${w.mode === "dynamic" ? "dynamic" : "static"}</span>
        </div>
        <div class="muted" style="font-size:11.5px">${esc(w.description || "")}</div>
      </div>`).join("") || '<div class="empty">还没有工作流</div>';
    list.querySelectorAll("[data-wf]").forEach((el) =>
      el.addEventListener("click", async () => {
        selId = el.dataset.wf;
        sessionStorage.setItem("wfSel", selId);
        await loadRun();
        renderLeft(); renderHead(); renderRight(); resub();
      }));
  };

  const renderHead = () => {
    const head = $main.querySelector("#wf-head");
    const wf = selWf();
    if (!head) return;
    if (!wf) { head.innerHTML = ""; return; }
    head.innerHTML = `
      <h2 style="margin:0">${esc(wf.name)}</h2>
      <span class="badge">${wf.mode === "dynamic" ? "dynamic · main agent 对话式编排" : "static · planner 组装 DAG"}</span>
      <span style="flex:1"></span>
      <button class="small" onclick="location.hash='#/workflows/${esc(wf.id)}/edit'">设置</button>
      <a class="btn small" href="#/runs" onclick="sessionStorage.setItem('wfFilter','${esc(wf.id)}')">历史</a>`;
  };

  // Status zone: the selected run's live shape, compact. Deep inspection stays
  // on the run page — every element here links into it.
  const renderStatus = () => {
    if (!run) return '<div class="empty" style="padding:18px">还没有运行 — 在下面对 main agent 说出你的目标,它会拆解并派发 agent 开始干活。</div>';
    const dyn = run.mode === "dynamic";
    const header = `
      <div class="wf-run-head">
        <a href="#/runs/${esc(run.id)}" class="mono" style="font-size:11px">${esc(run.id)}</a>
        ${chip(run.status)}
        ${run.coordinator?.rounds ? `<span class="badge">第 ${run.coordinator.rounds} 轮</span>` : ""}
        <span class="badge" title="按 API 牌价折算">est. ${fmtCost(run.cost_usd)}</span>
        <span class="badge">${runDuration(run)}</span>
        ${terminalRun(run) ? "" : '<button class="small danger" data-act="cancel">取消</button>'}
        ${run.status === "interrupted" && dyn ? '<button class="small primary" data-act="resume">▶ 恢复</button>' : ""}
      </div>`;
    if (dyn) {
      const tasks = (run.task_order || []).map((id) => run.tasks[id]).filter(Boolean);
      const rows = tasks.map((t) => `
        <a class="wf-task ${esc(t.status)}" href="#/runs/${esc(run.id)}">
          <span class="tdot"></span>
          <span class="ttitle">${esc(t.title || t.id)}</span>
          <span class="tmeta"><span class="mono">${esc(t.agent)}</span>
            <span>${t.status === "working" && t.activity ? "⚙ " + esc(t.activity) : esc(TASK_LABEL[t.status] || t.status)}</span>
            ${t.failure_kind ? `<span class="mono" style="color:var(--bad)">${esc(t.failure_kind)}</span>` : ""}
          </span>
        </a>`).join("");
      return header + (rows ? `<div class="wf-tasks">${rows}</div>` : '<div class="muted" style="font-size:12px;padding:6px 2px">main agent 还没有派发任务…</div>');
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

  // Chat zone: the conversation with the main agent, plus decision moments
  // (approval, verdict) inline where they happen.
  const renderChat = () => {
    if (!run) return "";
    const msgs = (run.chat || []).map((m) => `
      <div class="cmsg ${m.from === "user" ? "me" : "agent"}">
        <div class="cwho">${m.from === "user" ? "你" : "main agent"} · ${fmtTime(m.ts)}</div>
        <div class="cbody">${esc(m.text)}</div>
      </div>`).join("");
    let tail = "";
    if (run.status === "awaiting_approval" && run.proposal) {
      tail = `
        <div class="cmsg agent">
          <div class="cwho">main agent · 计划待批准</div>
          <div class="cbody">
            ${esc(run.proposal.summary || "")}
            <ol style="margin:8px 0 0 18px">${(run.proposal.tasks || []).map((t) =>
              `<li>${esc(t.title || "")} → <span class="mono">${esc(t.agent)}</span></li>`).join("")}</ol>
            <div class="row" style="margin-top:10px">
              <button class="small primary" data-act="approve">✓ 批准</button>
              <button class="small danger" data-act="reject">✗ 拒绝</button>
            </div>
          </div>
        </div>`;
    } else if (!terminalRun(run) && run.mode === "dynamic") {
      tail = `<div class="cmsg agent typing"><div class="cbody">⋯ main agent 工作中(${esc(run.coordinator?.activity || "等待任务推进")})</div></div>`;
    } else if (run.status === "failed" && run.error) {
      tail = `<div class="cmsg agent"><div class="cwho">系统</div><div class="cbody" style="color:var(--bad)">${esc(run.error)}</div></div>`;
    }
    return msgs + tail;
  };

  const renderRight = () => {
    const st = $main.querySelector("#wf-status");
    const log = $main.querySelector("#wf-chat-log");
    if (!st || !log) return;
    st.innerHTML = renderStatus();
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
    log.innerHTML = renderChat();
    if (atBottom) log.scrollTop = log.scrollHeight;
    const dryWrap = $main.querySelector("#wf-dry-wrap");
    if (dryWrap) dryWrap.style.display = !run || terminalRun(run) ? "" : "none";
    st.querySelectorAll("[data-act]").forEach(wireAct);
    log.querySelectorAll("[data-act]").forEach(wireAct);
  };

  const wireAct = (btn) => btn.addEventListener("click", async () => {
    const act = btn.dataset.act;
    const body = act === "reject" ? { reason: prompt("拒绝理由(会告知 main agent):") || "" } : {};
    try {
      await api(`/runs/${run.id}/${act}`, { method: "POST", body });
      if (act === "resume") { await loadRun(); resub(); }
    } catch (e) { toast(e.message); }
  });

  const send = async () => {
    const box = $main.querySelector("#wf-input");
    const text = box.value.trim();
    if (!text) return;
    const wf = selWf();
    if (!wf) return;
    box.value = "";
    try {
      if (wf.mode === "dynamic") {
        const dry = $main.querySelector("#wf-dry")?.checked || false;
        const r = await api(`/workflows/${wf.id}/chat`, { method: "POST", body: { text, dry_run: dry } });
        if (!run || r.id !== run.id) { run = r; resub(); }
      } else {
        const dry = $main.querySelector("#wf-dry")?.checked || false;
        run = await api(`/workflows/${wf.id}/runs`, { method: "POST", body: { goal: text, dry_run: dry } });
        resub();
      }
      renderRight();
    } catch (e) { toast("发送失败:" + e.message); }
  };

  $main.innerHTML = `
    <div class="wf-split">
      <div class="wf-left">
        <div class="wf-left-head">
          <h1 style="font-size:16px;margin:0">工作流</h1>
          <button class="small primary" onclick="location.hash='#/workflows/new'">+ 新建</button>
        </div>
        <div id="wf-list"></div>
      </div>
      <div class="wf-right">
        <div class="wf-head" id="wf-head"></div>
        <div class="wf-status" id="wf-status"></div>
        <div class="wf-chat-log" id="wf-chat-log"></div>
        <div class="wf-chat-input">
          <label class="check" id="wf-dry-wrap" style="font-size:11.5px">
            <input type="checkbox" id="wf-dry" ${meta.default_dry_run ? "checked" : ""}>
            <span>演示模式(dry run,零成本)— 仅对新发起的运行生效</span></label>
          <div class="row" style="align-items:flex-end">
            <textarea id="wf-input" rows="2" placeholder="对 main agent 说出目标或追加要求…(Enter 发送,Shift+Enter 换行)"></textarea>
            <button class="primary" id="wf-send">发送</button>
          </div>
        </div>
      </div>
    </div>`;
  renderLeft(); renderHead();
  await loadRun();
  renderRight(); resub();
  $main.querySelector("#wf-send").addEventListener("click", send);
  $main.querySelector("#wf-input").addEventListener("keydown", (e) => {
    // An Enter that is confirming an IME composition (pinyin etc.) belongs to
    // the IME, not to us: isComposing covers the standard case, keyCode 229
    // covers engines that fire the key event before composition ends.
    if (e.isComposing || e.keyCode === 229) return;
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  });
}

// ---------- workflow editor ----------

async function wfEditPage(id) {
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

  const render = () => {
    const dyn = mode === "dynamic";
    $main.innerHTML = `
    <div class="page-head">
      <h1>${id ? "编辑" : "新建"}工作流</h1>
      ${id ? '<button class="danger" id="wf-del">删除</button>' : ""}
      <button class="primary" id="wf-save">保存</button>
    </div>
    <div class="panel" style="max-width:760px">
      <label class="field"><span>名称</span><input id="f-name" value="${esc(wf.name)}"></label>
      <label class="field"><span>描述</span><input id="f-desc" value="${esc(wf.description || "")}"></label>

      <label class="field"><span>模式</span></label>
      <div class="mode-pick">
        <label class="${dyn ? "" : "on"}"><input type="radio" name="mode" value="static" ${dyn ? "" : "checked"}>
          <b>static</b>
          <span class="why">planner 先出完整 DAG,审批后确定性执行。形状可预判、要复现审计的任务。终止由无环结构保证。</span></label>
        <label class="${dyn ? "on" : ""}"><input type="radio" name="mode" value="dynamic" ${dyn ? "checked" : ""}>
          <b>dynamic</b>
          <span class="why">coordinator 边做边分解、委派、收敛,任务树运行时涌现。需要循环、反问的任务。终止由预算硬限保证。</span></label>
      </div>

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

      <label class="field"><span>Agent 池 · <span id="pool-count"></span>(不选 = 使用全部)</span></label>
      <div class="pool-picker">
        ${agents.map((a) => `
          <label class="check"><input type="checkbox" data-agent="${esc(a.name)}" ${pool.has(a.name) ? "checked" : ""}>
            <span><b>${esc(a.name)}</b> <span class="muted">${esc(a.description || "")}</span></span></label>`).join("")}
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
    $main.querySelector("#wf-save").onclick = async () => {
      collect();
      if (!wf.name) return toast("请填写名称");
      try {
        await api("/workflows", { method: "POST", body: wf });
        location.hash = "#/workflows";
      } catch (e) { toast("保存失败:" + e.message); }
    };
    const del = $main.querySelector("#wf-del");
    if (del) del.onclick = async () => {
      if (!confirm("删除该工作流?运行记录会保留。")) return;
      await api("/workflows/" + id, { method: "DELETE" });
      location.hash = "#/workflows";
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
        <td>${esc(r.workflow_name)} <span class="badge">${esc(r.mode || "static")}</span></td>
        <td style="max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(r.goal)}</td>
        <td>${chip(r.status)}</td>
        <td class="mono">${progress}</td>
        <td class="mono">${r.dry_run || r.backend === "mock" ? "dry-run" : esc(r.backend)}</td>
        <td class="mono">${fmtCost(r.cost_usd)}</td>
        <td class="mono">${fmtTime(r.created_at)}</td>
      </tr>`;
    }).join("");
    $main.innerHTML = `
      <div class="page-head"><h1>运行记录${wfFilter ? '<span class="muted">(已过滤)</span>' : ""}</h1></div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Run</th><th>工作流</th><th>目标</th><th>状态</th><th>进度</th><th>运行时</th><th>est. 成本</th><th>发起时间</th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
        ${runs.length ? "" : '<div class="empty">还没有运行记录</div>'}
      </div>`;
    $main.querySelectorAll("[data-run]").forEach((tr) =>
      tr.addEventListener("click", () => (location.hash = "#/runs/" + tr.dataset.run)));
  };
  await load();
  const timer = setInterval(load, 5000);
  cleanup = () => clearInterval(timer);
}

// ---------- run detail ----------

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
    $main.innerHTML = `
      <div class="page-head">
        <h1>${esc(run.workflow_name)}</h1>
        <span class="badge">${esc(run.mode || "static")}</span>
        ${chip(run.status)}
        ${controls.join("")}
      </div>
      <div class="panel">
        <div class="run-goal">${esc(run.goal)}</div>
        <div class="run-meta">
          <span class="mono">${esc(run.id)}</span>
          <span>${run.dry_run || run.backend === "mock" ? '<b style="color:var(--warn)">dry-run(演示)</b>' : `运行时: <b class="mono">${esc(run.backend)}</b>`}</span>
          <span title="按 Claude API 牌价折算,非实际账单">est. 成本: <b class="mono">${fmtCost(run.cost_usd)}</b></span>
          <span class="mono">${fmtTokens(run.usage)}</span>
          ${dyn ? `<span>任务: <b class="mono">${Object.keys(run.tasks || {}).length}</b></span>`
                : `<span>replan: <b class="mono">${run.replans}</b></span>`}
          <span>耗时: <b class="mono">${runDuration(run)}</b></span>
          <span>${fmtTime(run.created_at)}</span>
        </div>
        ${run.error ? `<div style="color:var(--bad);font-size:13px;margin-top:8px">${esc(run.error)}</div>` : ""}
      </div>
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
        const body = act === "reject" ? { reason: prompt("拒绝理由(会告知 coordinator):") || "" } : {};
        try { await api(`/runs/${run.id}/${act}`, { method: "POST", body }); }
        catch (e) { toast(e.message); }
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
  resub();
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
          <li><b>${esc(t.title || "(未命名)")}</b> → <span class="mono">${esc(t.agent)}</span>
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
      <h3>🧭 coordinator ${chip(c.status === "done" ? "succeeded" : c.status === "failed" ? "failed" : "running")}
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
  const [agents, costs] = await Promise.all([
    api("/agents"),
    api("/costs/summary?by=agent").catch(() => ({ agents: [] })),
  ]);
  // Cumulative spend per agent across every run it has ever served — the
  // cross-workflow view the run pages structurally cannot give.
  const spend = Object.fromEntries((costs.agents || []).map((b) => [b.key, b]));
  $main.innerHTML = `
    <div class="page-head">
      <h1>Agent 池 <span class="muted" style="font-size:13px">(${agents.length} 个可复用 executor)</span></h1>
      <button class="primary" id="ag-new">+ 新建 Agent</button>
    </div>
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
  $main.querySelector("#ag-new").onclick = () => agentModal(null, agents);
  $main.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => agentModal(agents.find((a) => a.name === b.dataset.edit), agents)));
  $main.querySelectorAll("[data-del]").forEach((b) =>
    b.addEventListener("click", async () => {
      if (!confirm(`删除 agent「${b.dataset.del}」?`)) return;
      await api("/agents/" + b.dataset.del, { method: "DELETE" });
      agentsPage();
    }));
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
          <label class="field" style="flex:1"><span>模型</span>${modelSelect("am-model", a.model)}</label>
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
        if (!confirm(`删除 skill「${el.dataset.skillDel}」?`)) return;
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
