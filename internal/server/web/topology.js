/* loom topology — the run's live agent star, painted on a canvas.

   The SSE stream pushes FULL run snapshots, not deltas, and the run page
   rebuilds its DOM on every one of them. A CSS/SVG animation would therefore be
   destroyed several times a second. This module instead owns a stateful canvas:
   the page hands it a snapshot via ingest(), it diffs against what it already
   saw, and turns the difference into effects that outlive the next snapshot.

   Colours are read from the same custom properties style.css defines, so a
   "working" node and a "working" chip can never drift apart.

   Public surface (one global, matching the app's no-module, no-bundler style):
     const topo = LoomTopology.create(canvasEl, { tip, onSelect, mode });
     topo.ingest(run, { initial: true });   // prime, no burst of animations
     topo.ingest(run);                      // diff → effects
     topo.setMode("agent" | "task"); topo.resize(); topo.selected(); topo.destroy();
*/
"use strict";

window.LoomTopology = (function () {
  const TAU = Math.PI * 2;
  const MAX_PARTICLES = 400;
  const MAX_BURST = 12;        // effects scheduled from one snapshot
  const STAGGER = 80;          // ms between them
  const PULSE_MS = 1200;       // same beat as @keyframes pulse
  const TERMINAL = ["succeeded", "failed", "canceled", "interrupted"];
  const FONT = '-apple-system, "PingFang SC", "Helvetica Neue", sans-serif';

  // Aggregated node status, most-alive-first: the view exists to show what is
  // happening now, so one live task outranks three finished ones.
  const STATUS_RANK = ["input-required", "working", "failed", "submitted", "canceled", "completed"];

  const clamp = (v, a, b) => (v < a ? a : v > b ? b : v);
  const easeOutCubic = (t) => 1 - Math.pow(1 - t, 3);
  const easeOutBack = (t) => 1 + 2.7 * Math.pow(t - 1, 3) + 1.7 * Math.pow(t - 1, 2);
  const now = () => performance.now();
  const esc = (s) =>
    String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  // Normalise to (-π, π] — without this a re-slotted ring takes the long way round.
  function shortAngle(from, to) {
    let d = (to - from) % TAU;
    if (d > Math.PI) d -= TAU;
    if (d <= -Math.PI) d += TAU;
    return d;
  }

  // hex + alpha, e.g. hexa("#8b5cf6", .4) → "#8b5cf666"
  function hexa(hex, a) {
    const v = clamp(Math.round(a * 255), 0, 255).toString(16).padStart(2, "0");
    return hex + v;
  }

  // ---------- theme ----------

  function readTheme() {
    const cs = getComputedStyle(document.documentElement);
    const v = (name, fallback) => {
      const raw = (cs.getPropertyValue(name) || "").trim();
      return /^#[0-9a-f]{6}$/i.test(raw) ? raw : fallback;
    };
    const C = {
      bg: v("--bg", "#0a0d1c"),
      panel: v("--panel-solid", "#10162a"),
      text: v("--text", "#e8eaf2"),
      muted: v("--muted", "#8b93ad"),
      accent: v("--accent", "#8b5cf6"),
      accent2: v("--accent2", "#22d3ee"),
      ok: v("--ok", "#34d399"),
      bad: v("--bad", "#fb7185"),
      warn: v("--warn", "#fbbf24"),
      idle: v("--idle", "#4b5573"),
      // the chip/tdot palette, verbatim
      working: "#67e8f9",
      completed: "#86efac",
      failed: "#fda4af",
      link: "#a5b4fc",
    };
    C.status = {
      idle: C.idle,
      submitted: C.idle,
      pending: C.idle,
      canceled: C.idle,
      working: C.working,
      running: C.working,
      completed: C.completed,
      succeeded: C.completed,
      failed: C.failed,
      "input-required": C.warn,
      awaiting_user: C.warn,
      done: C.completed,
    };
    return C;
  }

  const statusColor = (C, st) => C.status[st] || C.idle;

  // ---------- instance ----------

  function create(canvas, options) {
    const opts = options || {};
    const ctx = canvas.getContext("2d");
    const tipEl = opts.tip || null;
    const onSelect = typeof opts.onSelect === "function" ? opts.onSelect : null;

    let C = readTheme();
    let mode = opts.mode === "task" ? "task" : "agent";

    let W = 0, H = 0, cx = 0, cy = 0, R1 = 160;
    let raf = 0, last = 0, destroyed = false, dead = false;

    const nodes = new Map();     // id → node, insertion order = first-seen order
    const edges = new Map();     // id → edge
    let particles = [];
    let ripples = [];
    let sparks = [];
    let glyphs = [];
    let sweep = null;
    let pending = [];            // staggered effects waiting for their moment

    let snapshot = null;         // last run seen, for setMode/relayout
    let runTerminal = false;
    let hover = null, selectedId = null;

    const seen = {
      taskStatus: new Map(),
      taskMsgCount: new Map(),
      taskActivity: new Map(),
      eventCount: 0,
      chatCount: 0,
      rounds: 0,
      runStatus: "",
      primed: false,
    };

    const mm = window.matchMedia ? matchMedia("(prefers-reduced-motion: reduce)") : null;
    let reduced = !!(mm && mm.matches);
    const onMotionChange = () => { reduced = mm.matches; if (reduced) { particles = []; sparks = []; } };
    if (mm && mm.addEventListener) mm.addEventListener("change", onMotionChange);

    // ---------- graph ----------

    function makeNode(id, kind, label, ring) {
      const n = {
        id, kind, label, ring,
        angle: -Math.PI / 2, tAngle: -Math.PI / 2,
        r: 0, tr: 0, x: cx, y: cy,
        radius: kind === "coordinator" ? 34 : kind === "user" ? 14 : 20,
        status: "idle",
        taskIds: [],
        counts: { total: 0, working: 0, completed: 0, failed: 0 },
        model: "", activity: "", sub: "",
        parent: null,
        inEdge: null,
        pop: 0, shake: 0, glow: 0, emit: 0,
        phase: (nodes.size * 1.7) % TAU,
        seenAt: now(),
      };
      nodes.set(id, n);
      return n;
    }

    function upsertNode(id, kind, label, ring) {
      const n = nodes.get(id);
      if (!n) return makeNode(id, kind, label, ring);
      n.label = label;
      if (ring < n.ring) n.ring = ring;   // an agent seen first via handoff can be promoted
      return n;
    }

    function upsertEdge(id, from, to, kind) {
      let e = edges.get(id);
      if (!e) {
        e = { id, from, to, kind, weight: 0, activity: 0, status: "idle", flash: 0, dead: false, g: null };
        edges.set(id, e);
      }
      return e;
    }

    const nodeIdForTask = (t) => (mode === "task" ? "task:" + t.id : "agent:" + (t.agent || "agent"));

    function nodeForTask(t) {
      return t ? nodes.get(nodeIdForTask(t)) || null : null;
    }

    // nodeById maps whatever an event carries (usually a task id) onto a graph node.
    function nodeById(ref) {
      if (!ref) return null;
      if (nodes.has(ref)) return nodes.get(ref);
      const t = snapshot && snapshot.tasks ? snapshot.tasks[ref] : null;
      return t ? nodeForTask(t) : null;
    }

    function tasksOf(run) {
      const out = [];
      const order = run.task_order || Object.keys(run.tasks || {});
      for (const id of order) {
        const t = (run.tasks || {})[id];
        if (t) out.push(t);
      }
      return out;
    }

    // rebuildGraph upserts; nodes are never removed mid-run, because a worker
    // that finished is still part of the story of the run.
    function rebuildGraph(run) {
      const coord = upsertNode("coordinator", "coordinator", "main agent", 0);
      coord.status = coordStatus(run);
      coord.model = (run.coordinator && run.coordinator.model) || "";
      coord.activity = (run.coordinator && run.coordinator.activity) || "";
      coord.sub = run.coordinator && run.coordinator.rounds ? "第 " + run.coordinator.rounds + " 轮" : "";

      const user = upsertNode("user", "user", "你", 0);
      user.status = "idle";
      if ((run.chat || []).length) upsertEdge("user->coordinator", "user", "coordinator", "chat");

      // weights are recomputed from scratch: rebuildGraph runs per snapshot and
      // an accumulating counter would inflate every spoke over time
      for (const e of edges.values()) e.weight = 0;

      const tasks = tasksOf(run);
      const agg = new Map();     // node id → accumulator

      for (const t of tasks) {
        const id = nodeIdForTask(t);
        const parentTask = (run.tasks || {})[t.created_by];
        // a handoff to the agent that is already on the wire is not a new spoke
        const pid = parentTask ? nodeIdForTask(parentTask) : null;
        const handoff = !!pid && pid !== id;
        const label = mode === "task" ? (t.title || t.id) : (t.agent || "agent");
        const n = upsertNode(id, "agent", label, handoff ? 2 : 1);

        if (handoff) upsertEdge(pid + "->" + id, pid, id, "handoff").weight += 1;
        if (handoff && n.ring >= 2) {
          n.parent = pid;
          n.inEdge = pid + "->" + id;
        } else {
          n.parent = "coordinator";
          const e = upsertEdge("coordinator->" + id, "coordinator", id, "delegate");
          e.weight += 1;
          n.inEdge = e.id;
        }

        let a = agg.get(id);
        if (!a) { a = { ids: [], total: 0, working: 0, completed: 0, failed: 0, status: "idle", model: "", activity: "" }; agg.set(id, a); }
        a.ids.push(t.id);
        a.total += 1;
        if (t.status === "working") a.working += 1;
        if (t.status === "completed") a.completed += 1;
        if (t.status === "failed") a.failed += 1;
        if (rank(t.status) < rank(a.status)) a.status = t.status;
        if (t.model) a.model = t.model;
        if (t.status === "working" && t.activity) a.activity = t.activity;
      }

      for (const [id, a] of agg) {
        const n = nodes.get(id);
        if (!n) continue;
        n.taskIds = a.ids;
        n.counts = { total: a.total, working: a.working, completed: a.completed, failed: a.failed };
        n.status = a.status === "idle" ? "submitted" : a.status;
        n.model = a.model;
        n.activity = a.activity;
        n.radius = mode === "task"
          ? 17
          : clamp(18 + 4 * Math.log2(1 + a.total), 18, 30);
      }

      // a spoke whose only tasks failed reads as a dead wire
      for (const e of edges.values()) {
        const to = nodes.get(e.to);
        if (!to || e.kind === "chat") continue;
        e.status = to.status;
        e.dead = to.status === "failed" || to.status === "canceled";
      }
    }

    const rank = (st) => {
      const i = STATUS_RANK.indexOf(st);
      return i < 0 ? STATUS_RANK.length : i;
    };

    function coordStatus(run) {
      const c = run.coordinator;
      if (!c) return TERMINAL.indexOf(run.status) >= 0 ? "completed" : "working";
      if (c.status === "awaiting_user") return "input-required";
      if (c.status === "done") return "completed";
      if (c.status === "failed") return "failed";
      return "working";
    }

    // ---------- layout ----------

    function relayout() {
      R1 = Math.max(90, Math.min(W, H) * 0.30);
      const ring1 = [];
      for (const n of nodes.values()) if (n.kind === "agent" && n.ring === 1) ring1.push(n);
      const step = TAU / Math.max(ring1.length, 1);
      ring1.forEach((n, i) => {
        n.tAngle = -Math.PI / 2 + i * step;   // 12 o'clock, clockwise
        n.tr = R1;
      });

      // ring 2+ clusters inside its parent's angular sector, so lineage reads
      // without drawing a tree
      const kidsOf = new Map();
      for (const n of nodes.values()) {
        if (n.kind !== "agent" || n.ring < 2 || !n.parent) continue;
        if (!kidsOf.has(n.parent)) kidsOf.set(n.parent, []);
        kidsOf.get(n.parent).push(n);
      }
      for (const [pid, kids] of kidsOf) {
        const p = nodes.get(pid);
        const base = p ? p.tAngle : -Math.PI / 2;
        const sector = Math.min(step * 0.72, 0.9);
        kids.forEach((n, i) => {
          const f = kids.length === 1 ? 0 : i / (kids.length - 1) - 0.5;
          n.tAngle = base + f * sector;
          n.tr = R1 * Math.pow(1.7, n.ring - 1);
        });
      }

      const coord = nodes.get("coordinator");
      if (coord) { coord.tAngle = -Math.PI / 2; coord.tr = 0; }
      const user = nodes.get("user");
      if (user) { user.tAngle = -Math.PI / 2; user.tr = R1 * 0.46; }
    }

    // ---------- effects ----------

    function alive() {
      return !runTerminal || particles.length > 0 || ripples.length > 0 ||
        sparks.length > 0 || glyphs.length > 0 || pending.length > 0 || sweep !== null || settling();
    }

    function settling() {
      for (const n of nodes.values()) {
        if (n.pop > 0.01 || n.shake > 0.2 || n.glow > 0.01) return true;
        if (Math.abs(n.tr - n.r) > 0.5) return true;
        if (Math.abs(shortAngle(n.angle, n.tAngle)) > 0.004) return true;
        if (now() - n.seenAt < 500) return true;
      }
      for (const e of edges.values()) if (e.activity > 0.02 || e.flash > 0.01) return true;
      return false;
    }

    function spawn(edgeId, dir, color, cfg) {
      if (reduced || !edgeId) return;
      if (!edges.has(edgeId)) return;
      const c = cfg || {};
      if (particles.length >= MAX_PARTICLES) particles.splice(0, particles.length - MAX_PARTICLES + 1);
      particles.push({
        edgeId, dir,
        t: dir > 0 ? 0 : 1,
        speed: c.speed || 1.1,
        color: color || C.accent,
        size: c.size || 2.2,
        trail: c.trail || 0.16,
        ping: !!c.ping,
        owner: c.owner || null,
        onArrive: c.onArrive || null,
      });
    }

    function ripple(node, color, cfg) {
      if (!node) return;
      const c = cfg || {};
      ripples.push({
        node: node.id,
        r0: c.r0 != null ? c.r0 : node.radius,
        r1: c.r1 != null ? c.r1 : node.radius + 26,
        t: 0,
        dur: reduced ? 0.12 : (c.dur || 0.52),
        color: color || C.accent,
        width: c.width || 2,
      });
    }

    function spark(node) {
      if (reduced || !node) return;
      const a = Math.random() * TAU;
      sparks.push({ x: node.x, y: node.y, vx: Math.cos(a) * 20, vy: Math.sin(a) * 20, t: 0, dur: 0.5, color: C.working });
    }

    function glyph(node, text, color) {
      if (!node) return;
      glyphs.push({ node: node.id, text, color: color || C.warn, t: 0, dur: 0.9 });
    }

    function bump(node, amount) {
      if (!node) return;
      node.glow = Math.min(1, node.glow + (amount || 0.6));
    }

    function heat(edgeId, v) {
      const e = edges.get(edgeId);
      if (e) e.activity = Math.min(1, Math.max(e.activity, v == null ? 1 : v));
    }

    // emit staggers a snapshot's worth of changes: the broker drops frames for
    // slow subscribers (engine/broker.go), so one snapshot routinely carries
    // several things that "happened at once". A wave reads; a flash does not.
    function emit(fx) {
      if (!fx.length) return;
      let list = fx;
      if (list.length > MAX_BURST) {
        const keep = list.filter((f) => f.k !== "tick");
        list = (keep.length > MAX_BURST ? keep.slice(0, MAX_BURST) : keep.length ? keep : list.slice(0, MAX_BURST));
      }
      const t0 = now();
      list.forEach((f, i) => pending.push({ at: t0 + i * (reduced ? 0 : STAGGER), fx: f }));
    }

    function drainPending(t) {
      if (!pending.length) return;
      const still = [];
      for (const p of pending) {
        if (p.at <= t) applyEffect(p.fx);
        else still.push(p);
      }
      pending = still;
    }

    function applyEffect(f) {
      const n = f.node || null;
      switch (f.k) {
        case "delegate": {                                    // A1
          if (n) {
            const e = n.inEdge;
            heat(e, 1);
            for (let i = 0; i < 3; i++) {
              spawn(e, 1, C.accent, { speed: 1.1, size: 2.4, trail: 0.2 - i * 0.04 });
            }
            n.pop = 1;
            ripple(n, C.accent, { r1: n.radius + 26 });
          }
          break;
        }
        case "status": {                                      // A2 / A5 / A6 / A7
          if (!n) break;
          if (f.to === "working") {
            heat(n.inEdge, 0.8);
            bump(n, 0.5);
          } else if (f.to === "completed") {                  // A5
            heat(n.inEdge, 1);
            for (let i = 0; i < 5; i++) spawn(n.inEdge, -1, C.completed, { speed: 1.4, size: 2, trail: 0.14, onArrive: "coord" });
            ripple(n, C.completed, { r1: n.radius + 30, dur: 0.6 });
          } else if (f.to === "failed") {                     // A6
            heat(n.inEdge, 1);
            n.shake = reduced ? 0 : 6;
            ripple(n, C.bad, { r1: n.radius + 24, dur: 0.62, width: 2.4 });
            ripple(n, C.bad, { r0: n.radius + 6, r1: n.radius + 44, dur: 0.62, width: 1.2 });
            glyph(n, "✕", C.bad);
            const e = edges.get(n.inEdge);
            if (e) e.flash = 1.5;
          } else if (f.to === "input-required") {             // A7
            heat(n.inEdge, 0.6);
            spawn(n.inEdge, -1, C.warn, { speed: 0.42, size: 2.2, trail: 0.2, ping: true, owner: n.id });
            glyph(n, "?", C.warn);
          }
          // a node leaving input-required must lose its oscillating particle
          if (f.from === "input-required") {
            particles = particles.filter((p) => !(p.ping && p.owner === n.id));
          }
          break;
        }
        case "msg": {                                         // A3
          if (!n) break;
          const role = (f.m && f.m.role) || "progress";
          const e = n.inEdge;
          heat(e, 0.7);
          if (role === "instruction" || role === "followup") {
            spawn(e, 1, C.accent, { speed: 1.2, size: 2, trail: 0.15 });
          } else if (role === "question") {
            spawn(e, -1, C.warn, { speed: 1.2, size: 2.2, trail: 0.18 });
            const ed = edges.get(e);
            if (ed) ed.flash = 0.4;
            glyph(n, "?", C.warn);
          } else if (role === "peer") {
            spawn(e, 1, C.muted, { speed: 0.9, size: 1.8, trail: 0.12 });
          } else {
            spawn(e, -1, C.working, { speed: 1.2, size: 2, trail: 0.15 });
          }
          break;
        }
        case "tick": spark(n); break;                          // A4
        case "round": {                                        // A8
          const coord = nodes.get("coordinator");
          if (coord) ripple(coord, C.accent, { r0: coord.radius, r1: R1 * 0.8, dur: 0.9, width: 1.2 });
          break;
        }
        case "chat": {                                         // A9
          const from = (f.m && f.m.from) || "user";
          if (from === "user") spawn("user->coordinator", 1, C.accent, { speed: 1.5, size: 2.2 });
          else spawn("user->coordinator", -1, C.working, { speed: 1.5, size: 2.2 });
          heat("user->coordinator", 0.9);
          break;
        }
        case "handoff": {                                      // A10
          if (n) { n.pop = 1; heat(n.inEdge, 1); spawn(n.inEdge, 1, C.muted, { speed: 1, size: 2.2 }); }
          break;
        }
        case "artifact": if (n) { bump(n, 0.5); ripple(n, C.completed, { r1: n.radius + 18, dur: 0.4, width: 1.2 }); } break;
        case "warn": if (n) { glyph(n, "!", C.warn); ripple(n, C.warn, { r1: n.radius + 20, dur: 0.5 }); } break;
        case "error": if (n) { n.shake = reduced ? 0 : 5; ripple(n, C.bad, { r1: n.radius + 22, dur: 0.5 }); } break;
        case "run": {                                          // A11
          const ok = f.status === "succeeded";
          sweep = { t: 0, dur: 1.4, color: ok ? C.ok : f.status === "failed" ? C.bad : C.muted };
          const coord = nodes.get("coordinator");
          if (coord) { coord.pop = 0.8; bump(coord, 1); }
          break;
        }
        default: break;                                        // unknown kinds are ignored
      }
    }

    // ---------- ingest (snapshot diffing) ----------

    function primeSeen(run) {
      seen.taskStatus.clear();
      seen.taskMsgCount.clear();
      seen.taskActivity.clear();
      for (const t of tasksOf(run)) {
        seen.taskStatus.set(t.id, t.status);
        seen.taskMsgCount.set(t.id, (t.messages || []).length);
        seen.taskActivity.set(t.id, t.activity || "");
      }
      seen.eventCount = (run.events || []).length;
      seen.chatCount = (run.chat || []).length;
      seen.rounds = (run.coordinator && run.coordinator.rounds) || 0;
      seen.runStatus = run.status;
      seen.primed = true;
    }

    // assembleAnimation: the star flies out of the center once, staggered, so
    // opening a finished run feels constructed rather than pasted.
    function assembleAnimation() {
      const t0 = now();
      let i = 0;
      for (const n of nodes.values()) {
        n.seenAt = reduced ? t0 - 1000 : t0 + i * 40;
        i++;
      }
    }

    function ingest(run, o) {
      if (destroyed || !run) return;
      const initial = !!(o && o.initial) || !seen.primed;
      snapshot = run;
      runTerminal = TERMINAL.indexOf(run.status) >= 0;

      const before = new Set(nodes.keys());
      rebuildGraph(run);
      relayout();

      if (initial) {
        primeSeen(run);
        assembleAnimation();
        start();
        return;
      }

      // brand-new nodes are born at the center and fly out along their spoke
      for (const [id, n] of nodes) {
        if (!before.has(id)) { n.seenAt = now(); n.r = 0; n.angle = n.tAngle; }
      }

      const fx = [];

      for (const t of tasksOf(run)) {
        const node = nodeForTask(t);
        if (!seen.taskStatus.has(t.id)) {
          fx.push({ k: "delegate", node, task: t });
        } else {
          const was = seen.taskStatus.get(t.id);
          if (was !== t.status) fx.push({ k: "status", node, task: t, from: was, to: t.status });
        }
        seen.taskStatus.set(t.id, t.status);

        const msgs = t.messages || [];
        const had = seen.taskMsgCount.has(t.id) ? seen.taskMsgCount.get(t.id) : msgs.length;
        if (msgs.length > had) for (const m of msgs.slice(had)) fx.push({ k: "msg", node, m });
        seen.taskMsgCount.set(t.id, msgs.length);

        const act = t.activity || "";
        if (act && act !== seen.taskActivity.get(t.id)) fx.push({ k: "tick", node });
        seen.taskActivity.set(t.id, act);
      }

      const rounds = (run.coordinator && run.coordinator.rounds) || 0;
      if (rounds > seen.rounds) fx.push({ k: "round" });
      seen.rounds = rounds;

      const chat = run.chat || [];
      if (chat.length > seen.chatCount) for (const m of chat.slice(seen.chatCount)) fx.push({ k: "chat", m });
      seen.chatCount = chat.length;

      if (run.status !== seen.runStatus) {
        if (runTerminal) fx.push({ k: "run", status: run.status });
        seen.runStatus = run.status;
      }

      // The event log is the residue only: task status and messages are read
      // from the task objects, which are authoritative and typed.
      const evs = run.events || [];
      for (const ev of evs.slice(seen.eventCount)) {
        switch (ev.type) {
          case "handoff": fx.push({ k: "handoff", node: nodeById(ev.node) }); break;
          case "artifact_written": fx.push({ k: "artifact", node: nodeById(ev.node) }); break;
          case "stall_warning":
          case "budget_hit": fx.push({ k: "warn", node: nodeById(ev.node) || nodes.get("coordinator") }); break;
          case "error": fx.push({ k: "error", node: nodeById(ev.node) || nodes.get("coordinator") }); break;
          default: break;                 // load-bearing: new event types must never break the view
        }
      }
      seen.eventCount = evs.length;

      emit(fx);
      start();
    }

    // ---------- geometry ----------

    function edgeGeom(e) {
      const a = nodes.get(e.from), b = nodes.get(e.to);
      if (!a || !b) return null;
      const dx = b.x - a.x, dy = b.y - a.y;
      const len = Math.hypot(dx, dy) || 1;
      const ux = dx / len, uy = dy / len;
      const ax = a.x + ux * (a.radius + 3), ay = a.y + uy * (a.radius + 3);
      const bx = b.x - ux * (b.radius + 3), by = b.y - uy * (b.radius + 3);
      const bow = (e.kind === "handoff" ? 0.16 : 0.07) * len;
      return { ax, ay, bx, by, qx: (ax + bx) / 2 - uy * bow, qy: (ay + by) / 2 + ux * bow };
    }

    function pointOn(g, t) {
      const u = 1 - t;
      return {
        x: u * u * g.ax + 2 * u * t * g.qx + t * t * g.bx,
        y: u * u * g.ay + 2 * u * t * g.qy + t * t * g.by,
      };
    }

    // ---------- step ----------

    function step(dt, t) {
      const K = 1 - Math.pow(0.001, dt);   // frame-rate independent easing

      for (const n of nodes.values()) {
        if (reduced) { n.angle = n.tAngle; n.r = n.tr; }
        else {
          n.angle += shortAngle(n.angle, n.tAngle) * K;
          n.r += (n.tr - n.r) * K;
        }
        const age = (t - n.seenAt) / 420;
        const emerge = age >= 1 ? 1 : easeOutCubic(clamp(age, 0, 1));
        const drift = reduced || n.kind === "coordinator" ? 0 : Math.sin(t / 2600 + n.phase) * 2.5;
        n.x = cx + Math.cos(n.angle) * (n.r * emerge + drift) + (n.shake ? Math.sin(t / 28) * n.shake : 0);
        n.y = cy + Math.sin(n.angle) * (n.r * emerge + drift);
        n.pop *= Math.pow(0.02, dt);
        n.glow *= Math.pow(0.15, dt);
        n.shake *= Math.pow(0.0008, dt);
        if (n.shake < 0.2) n.shake = 0;

        // A2: a working node keeps a slow outbound stream alive
        if (n.status === "working" && n.kind === "agent" && !reduced) {
          n.emit += dt;
          if (n.emit > 0.7) { n.emit = 0; spawn(n.inEdge, 1, C.working, { speed: 1.0, size: 1.8, trail: 0.12 }); heat(n.inEdge, 0.55); }
        } else n.emit = 0;
      }

      for (const e of edges.values()) {
        e.activity *= Math.pow(0.35, dt);     // ~2s half-life
        if (e.activity < 0.002) e.activity = 0;
        e.flash = Math.max(0, e.flash - dt);
        e.g = edgeGeom(e);
      }

      for (let i = particles.length - 1; i >= 0; i--) {
        const p = particles[i];
        p.t += p.speed * dt * p.dir;
        if (p.t >= 1 || p.t <= 0) {
          if (p.ping) { p.dir *= -1; p.t = clamp(p.t, 0, 1); continue; }
          if (p.onArrive === "coord") bump(nodes.get("coordinator"), 0.35);
          particles.splice(i, 1);
        }
      }

      for (let i = ripples.length - 1; i >= 0; i--) {
        ripples[i].t += dt;
        if (ripples[i].t >= ripples[i].dur) ripples.splice(i, 1);
      }
      for (let i = sparks.length - 1; i >= 0; i--) {
        const s = sparks[i];
        s.t += dt;
        s.x += s.vx * dt; s.y += s.vy * dt;
        if (s.t >= s.dur) sparks.splice(i, 1);
      }
      for (let i = glyphs.length - 1; i >= 0; i--) {
        glyphs[i].t += dt;
        if (glyphs[i].t >= glyphs[i].dur) glyphs.splice(i, 1);
      }
      if (sweep) { sweep.t += dt; if (sweep.t >= sweep.dur) sweep = null; }
    }

    // ---------- draw ----------

    function draw(t) {
      ctx.clearRect(0, 0, W, H);
      drawBackdrop();
      for (const e of edges.values()) drawEdge(e, t);
      drawRipples();
      drawParticles();
      drawSparks();
      for (const n of nodes.values()) drawNode(n, t);
      drawGlyphs();
      if (sweep) drawSweep();
    }

    function drawBackdrop() {
      const coord = nodes.get("coordinator");
      if (!coord) return;
      const g = ctx.createRadialGradient(coord.x, coord.y, 0, coord.x, coord.y, coord.radius * 2.6 + 30);
      g.addColorStop(0, hexa(C.accent, 0.10));
      g.addColorStop(1, hexa(C.accent, 0));
      ctx.fillStyle = g;
      ctx.beginPath();
      ctx.arc(coord.x, coord.y, coord.radius * 2.6 + 30, 0, TAU);
      ctx.fill();
    }

    function drawEdge(e, t) {
      const g = e.g;
      if (!g) return;
      const a = e.activity;
      let width = 1.2 + 1.4 * a + Math.min(e.weight, 4) * 0.35;
      ctx.save();
      ctx.lineCap = "round";
      if (e.kind === "chat") {
        ctx.strokeStyle = hexa(C.link, 0.25 + 0.4 * a);
        ctx.setLineDash([3, 5]);
        width = 1 + a;
      } else if (e.dead) {
        ctx.strokeStyle = hexa(C.bad, 0.3 + 0.4 * e.flash);
        ctx.setLineDash([5, 4]);
      } else if (e.kind === "handoff") {
        ctx.strokeStyle = hexa(C.muted, 0.28 + 0.4 * a);
        ctx.setLineDash([5, 4]);
      } else if (a > 0.02) {
        const lg = ctx.createLinearGradient(g.ax, g.ay, g.bx, g.by);
        lg.addColorStop(0, hexa(C.accent, 0.22 + 0.45 * a));
        lg.addColorStop(1, hexa(C.accent2, 0.22 + 0.45 * a));
        ctx.strokeStyle = lg;
        if (a > 0.15 && !reduced) { ctx.setLineDash([2, 10]); ctx.lineDashOffset = -((t / 1000) * 26) % 12; }
      } else {
        ctx.strokeStyle = "rgba(139,147,173,.22)";
      }
      ctx.lineWidth = width;
      ctx.beginPath();
      ctx.moveTo(g.ax, g.ay);
      ctx.quadraticCurveTo(g.qx, g.qy, g.bx, g.by);
      ctx.stroke();
      ctx.restore();
    }

    function drawParticles() {
      if (!particles.length) return;
      ctx.save();
      ctx.globalCompositeOperation = "lighter";   // aurora: light adds up where flows converge
      ctx.lineCap = "round";
      for (const p of particles) {
        const e = edges.get(p.edgeId);
        if (!e || !e.g) continue;
        const head = pointOn(e.g, clamp(p.t, 0, 1));
        const tail = pointOn(e.g, clamp(p.t - p.dir * p.trail, 0, 1));
        const lg = ctx.createLinearGradient(tail.x, tail.y, head.x, head.y);
        lg.addColorStop(0, p.color + "00");
        lg.addColorStop(1, p.color + "ff");
        ctx.strokeStyle = lg;
        ctx.lineWidth = p.size;
        ctx.beginPath();
        ctx.moveTo(tail.x, tail.y);
        ctx.lineTo(head.x, head.y);
        ctx.stroke();
        ctx.fillStyle = p.color;
        ctx.shadowColor = p.color;
        ctx.shadowBlur = 8;
        ctx.beginPath();
        ctx.arc(head.x, head.y, p.size * 0.7, 0, TAU);
        ctx.fill();
        ctx.shadowBlur = 0;
      }
      ctx.restore();
    }

    function drawSparks() {
      if (!sparks.length) return;
      ctx.save();
      ctx.globalCompositeOperation = "lighter";
      for (const s of sparks) {
        const k = 1 - s.t / s.dur;
        ctx.fillStyle = hexa(s.color, clamp(k, 0, 1));
        ctx.beginPath();
        ctx.arc(s.x, s.y, 2 * k + 0.6, 0, TAU);
        ctx.fill();
      }
      ctx.restore();
    }

    function drawRipples() {
      for (const rp of ripples) {
        const n = nodes.get(rp.node);
        if (!n) continue;
        const k = clamp(rp.t / rp.dur, 0, 1);
        ctx.save();
        ctx.strokeStyle = hexa(rp.color, (1 - k) * 0.7);
        ctx.lineWidth = rp.width;
        ctx.beginPath();
        ctx.arc(n.x, n.y, rp.r0 + (rp.r1 - rp.r0) * easeOutCubic(k), 0, TAU);
        ctx.stroke();
        ctx.restore();
      }
    }

    function drawNode(n, t) {
      const isCoord = n.kind === "coordinator";
      const isUser = n.kind === "user";
      const popK = n.pop > 0.01 ? 1 + 0.22 * easeOutBack(clamp(1 - n.pop, 0, 1)) * n.pop : 1;
      const r = n.radius * popK;
      const col = statusColor(C, n.status);
      const beat = 0.65 + 0.35 * Math.cos((t / PULSE_MS) * TAU);
      const live = n.status === "working" || n.status === "input-required";
      const hovered = hover === n;
      const chosen = selectedId === n.id;

      ctx.save();

      // body
      ctx.fillStyle = hexa(C.panel, isCoord ? 0.92 : 0.88);
      ctx.beginPath();
      ctx.arc(n.x, n.y, r, 0, TAU);
      ctx.fill();
      ctx.strokeStyle = "rgba(255,255,255,.045)";
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.arc(n.x, n.y, r - 1, 0, TAU);
      ctx.stroke();

      // ring
      if (isCoord) {
        let grad;
        if (typeof ctx.createConicGradient === "function") {
          grad = ctx.createConicGradient(reduced ? 0 : (t / 12000) * TAU, n.x, n.y);   // A12
          grad.addColorStop(0, C.accent);
          grad.addColorStop(0.5, C.accent2);
          grad.addColorStop(1, C.accent);
        } else {
          grad = ctx.createLinearGradient(n.x - r, n.y - r, n.x + r, n.y + r);
          grad.addColorStop(0, C.accent);
          grad.addColorStop(1, C.accent2);
        }
        ctx.strokeStyle = n.status === "completed" || n.status === "failed" || n.status === "input-required" ? col : grad;
        ctx.lineWidth = 2.2;
        ctx.shadowColor = hexa(C.accent, 0.45);
        ctx.shadowBlur = 24;
        ctx.globalAlpha = n.status === "working" && !reduced ? 0.55 + 0.45 * beat : 1;
        ctx.beginPath();
        ctx.arc(n.x, n.y, r, 0, TAU);
        ctx.stroke();
        ctx.globalAlpha = 1;
        ctx.shadowBlur = 0;
      } else {
        ctx.strokeStyle = col;
        ctx.lineWidth = live ? 2 : 1.5;
        if (n.status === "canceled") ctx.setLineDash([4, 3]);
        if (live || n.glow > 0.01 || hovered) {
          ctx.shadowColor = col;
          ctx.shadowBlur = (live ? 14 : 6) + n.glow * 10 + (hovered ? 6 : 0);
        }
        ctx.globalAlpha = live && !reduced ? beat : 1;
        if (hovered) ctx.globalAlpha = Math.min(1, ctx.globalAlpha * 1.25);
        ctx.beginPath();
        ctx.arc(n.x, n.y, r, 0, TAU);
        ctx.stroke();
        ctx.globalAlpha = 1;
        ctx.shadowBlur = 0;
        ctx.setLineDash([]);
      }

      // progress arc
      if (!isUser && !isCoord && n.counts.total > 1) {
        const frac = n.counts.completed / n.counts.total;
        if (frac > 0) {
          ctx.strokeStyle = hexa(C.completed, 0.8);
          ctx.lineWidth = 2.5;
          ctx.lineCap = "round";
          ctx.beginPath();
          ctx.arc(n.x, n.y, r + 5, -Math.PI / 2, -Math.PI / 2 + TAU * frac);
          ctx.stroke();
          ctx.lineCap = "butt";
        }
      }

      // A2: a rotating dashed arc says "busy" without any text
      if (n.status === "working" && !isUser && !reduced) {
        ctx.strokeStyle = hexa(C.working, 0.55);
        ctx.lineWidth = 1.4;
        ctx.beginPath();
        ctx.arc(n.x, n.y, r + 8, (t / 1000) * 0.6, (t / 1000) * 0.6 + Math.PI / 2);
        ctx.stroke();
      }

      // selection ring — the canvas translation of "selection is a gradient border"
      if (chosen) {
        const sg = ctx.createLinearGradient(n.x - r, n.y - r, n.x + r, n.y + r);
        sg.addColorStop(0, C.accent);
        sg.addColorStop(1, C.accent2);
        ctx.strokeStyle = sg;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(n.x, n.y, r + 4, 0, TAU);
        ctx.stroke();
      }

      // labels
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      if (isCoord) {
        ctx.font = "18px " + FONT;
        ctx.fillText("🧭", n.x, n.y - 1);
        ctx.font = "600 12.5px " + FONT;
        ctx.fillStyle = C.text;
        ctx.fillText("main agent", n.x, n.y + r + 14);
        if (n.sub) {
          ctx.font = "10.5px " + FONT;
          ctx.fillStyle = C.muted;
          ctx.fillText(n.sub, n.x, n.y + r + 28);
        }
      } else if (isUser) {
        ctx.font = "12px " + FONT;
        ctx.fillStyle = C.muted;
        ctx.fillText("你", n.x, n.y);
      } else {
        ctx.font = "600 11px " + FONT;
        ctx.fillStyle = C.text;
        ctx.fillText(fit(n.label, 2 * r - 8), n.x, n.y);
        let below = n.y + r + 12;
        ctx.font = "10px " + FONT;
        ctx.fillStyle = C.muted;
        const meta = [];
        if (n.counts.total) meta.push(n.counts.completed + "/" + n.counts.total);
        if (n.model) meta.push(shortModel(n.model));
        if (meta.length) { ctx.fillText(meta.join(" · "), n.x, below); below += 13; }
        if (n.status === "working" && n.activity) {
          ctx.font = "10px " + FONT;
          ctx.fillStyle = C.working;
          ctx.fillText(fit(n.activity, 130), n.x, below);
        }
      }
      ctx.restore();
    }

    function drawGlyphs() {
      ctx.save();
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      for (const gl of glyphs) {
        const n = nodes.get(gl.node);
        if (!n) continue;
        const k = gl.t / gl.dur;
        ctx.globalAlpha = clamp(1 - k, 0, 1);
        ctx.fillStyle = gl.color;
        ctx.font = "600 15px " + FONT;
        ctx.fillText(gl.text, n.x, n.y - n.radius - 10 - 14 * k);
      }
      ctx.restore();
    }

    function drawSweep() {                                   // A11
      const k = clamp(sweep.t / sweep.dur, 0, 1);
      const rad = Math.hypot(W, H) * 0.6 * k;
      ctx.save();
      ctx.globalCompositeOperation = "lighter";
      const g = ctx.createRadialGradient(cx, cy, Math.max(0, rad - 60), cx, cy, rad + 10);
      g.addColorStop(0, hexa(sweep.color, 0));
      g.addColorStop(0.7, hexa(sweep.color, 0.10 * (1 - k)));
      g.addColorStop(1, hexa(sweep.color, 0));
      ctx.fillStyle = g;
      ctx.fillRect(0, 0, W, H);
      ctx.restore();
    }

    // measured truncation — canvas has no text-overflow
    function fit(text, maxPx) {
      const s = String(text || "");
      if (!s) return "";
      if (ctx.measureText(s).width <= maxPx) return s;
      let lo = 0, hi = s.length;
      while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if (ctx.measureText(s.slice(0, mid) + "…").width <= maxPx) lo = mid; else hi = mid - 1;
      }
      return lo > 0 ? s.slice(0, lo) + "…" : "…";
    }

    const shortModel = (m) => String(m || "").replace(/^claude-/, "");

    // ---------- loop ----------

    function frame(t) {
      raf = 0;
      if (destroyed) return;
      const dt = Math.min((t - last) / 1000, 0.05);   // clamp: a tab switch must not teleport
      last = t;
      drainPending(t);
      step(dt > 0 ? dt : 0.016, t);
      draw(t);
      if (alive()) raf = requestAnimationFrame(frame);
      else dead = true;                               // last frame stays on screen
    }

    function start() {
      if (destroyed || raf || document.hidden) return;
      dead = false;
      last = now();
      raf = requestAnimationFrame(frame);
    }

    function stop() {
      if (raf) cancelAnimationFrame(raf);
      raf = 0;
    }

    const onVisibility = () => { if (document.hidden) stop(); else if (!dead) start(); };
    document.addEventListener("visibilitychange", onVisibility);

    // ---------- sizing ----------

    function fitCanvas() {
      const rect = canvas.getBoundingClientRect();
      if (!rect.width || !rect.height) return;
      const dpr = Math.min(window.devicePixelRatio || 1, 2);   // 3x buys nothing here
      canvas.width = Math.round(rect.width * dpr);
      canvas.height = Math.round(rect.height * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);                  // draw in CSS pixels
      W = rect.width; H = rect.height;
      cx = W / 2; cy = H / 2;
      C = readTheme();
      relayout();
      // always restart: a resize moved every target, and the eased positions
      // only catch up inside the loop. alive() shuts it down again once settled.
      start();
    }

    let ro = null;
    if (window.ResizeObserver) {
      ro = new ResizeObserver(fitCanvas);
      ro.observe(canvas);
    } else {
      window.addEventListener("resize", fitCanvas);
    }
    fitCanvas();

    // ---------- interaction ----------

    function local(ev) {
      const rect = canvas.getBoundingClientRect();
      return { x: ev.clientX - rect.left, y: ev.clientY - rect.top };
    }

    function hitTest(p) {
      let best = null, bestD = Infinity;
      for (const n of nodes.values()) {
        const d = Math.hypot(p.x - n.x, p.y - n.y);
        if (d <= n.radius + 6 && d < bestD) { best = n; bestD = d; }
      }
      return best;
    }

    function showTip(n) {
      if (!tipEl) return;
      if (!n) { tipEl.hidden = true; return; }
      const rows = [];
      if (n.kind === "coordinator") {
        rows.push('<div class="muted" style="font-size:11px">main agent</div>');
        if (n.model) rows.push('<div class="mono" style="font-size:11px">' + esc(shortModel(n.model)) + "</div>");
        if (n.sub) rows.push("<div>" + esc(n.sub) + "</div>");
        if (n.activity) rows.push('<div style="color:#67e8f9">⚙ ' + esc(n.activity) + "</div>");
      } else if (n.kind === "user") {
        rows.push("<div>你 — 与 main agent 的对话</div>");
      } else {
        rows.push("<b>" + esc(n.label) + "</b>");
        rows.push('<div class="muted" style="font-size:11px">' +
          esc(n.counts.total) + " 个任务 · 完成 " + esc(n.counts.completed) +
          (n.counts.working ? " · 执行中 " + esc(n.counts.working) : "") +
          (n.counts.failed ? " · 失败 " + esc(n.counts.failed) : "") + "</div>");
        rows.push('<div style="margin-top:4px">' + chipHTML(n.status) + "</div>");
        if (n.model) rows.push('<div class="muted mono" style="font-size:11px">' + esc(shortModel(n.model)) + "</div>");
        if (n.status === "working" && n.activity) rows.push('<div style="color:#67e8f9;font-size:11.5px">⚙ ' + esc(n.activity) + "</div>");
      }
      tipEl.innerHTML = rows.join("");
      tipEl.hidden = false;
      const w = tipEl.offsetWidth || 200, h = tipEl.offsetHeight || 60;
      tipEl.style.left = clamp(n.x + n.radius + 12, 6, Math.max(6, W - w - 6)) + "px";
      tipEl.style.top = clamp(n.y - h / 2, 6, Math.max(6, H - h - 6)) + "px";
    }

    const TASK_LABEL = {
      submitted: "排队", working: "执行中", "input-required": "待答复",
      completed: "完成", failed: "失败", canceled: "取消", idle: "空闲",
    };
    const chipHTML = (st) => '<span class="chip ' + esc(st) + '">' + esc(TASK_LABEL[st] || st) + "</span>";

    function onMove(ev) {
      const h = hitTest(local(ev));
      if (h !== hover) {
        hover = h;
        canvas.style.cursor = h ? "pointer" : "default";
        showTip(h);
        if (dead) draw(now());
      } else if (h) showTip(h);
    }

    function onLeave() {
      if (hover) { hover = null; canvas.style.cursor = "default"; showTip(null); if (dead) draw(now()); }
    }

    function onClick(ev) {
      const h = hitTest(local(ev));
      selectedId = h ? h.id : null;
      if (h) { ripple(h, C.accent2, { r1: h.radius + 16, dur: 0.35, width: 1.5 }); start(); }
      if (dead) draw(now());
      if (onSelect) onSelect(selectedId, h ? { kind: h.kind, label: h.label, taskIds: h.taskIds.slice() } : null);
    }

    canvas.addEventListener("pointermove", onMove);
    canvas.addEventListener("pointerleave", onLeave);
    canvas.addEventListener("click", onClick);

    // ---------- public ----------

    return {
      ingest,
      setMode(m) {
        const next = m === "task" ? "task" : "agent";
        if (next === mode) return;
        mode = next;
        nodes.clear();
        edges.clear();
        particles = []; ripples = []; sparks = []; glyphs = []; pending = [];
        selectedId = null; hover = null;
        if (snapshot) { rebuildGraph(snapshot); relayout(); assembleAnimation(); }
        start();
      },
      mode() { return mode; },
      resize: fitCanvas,
      selected() { return selectedId; },
      select(id) { selectedId = id || null; if (dead) draw(now()); },
      destroy() {
        destroyed = true;
        stop();
        if (ro) ro.disconnect(); else window.removeEventListener("resize", fitCanvas);
        document.removeEventListener("visibilitychange", onVisibility);
        if (mm && mm.removeEventListener) mm.removeEventListener("change", onMotionChange);
        canvas.removeEventListener("pointermove", onMove);
        canvas.removeEventListener("pointerleave", onLeave);
        canvas.removeEventListener("click", onClick);
        canvas.style.cursor = "";
        if (tipEl) tipEl.hidden = true;
        nodes.clear(); edges.clear();
        particles = []; ripples = []; sparks = []; glyphs = []; pending = []; sweep = null;
      },
    };
  }

  return { create };
})();
