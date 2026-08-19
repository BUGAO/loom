package hub

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"loom/internal/model"
)

// ---- assessment & triage ----
//
// The main agent decides nothing about its own level. What it does is file a
// structured ASSESSMENT of the task in front of it — steps, modules, parallel
// branches, roles, does it change code, how many files — and the engine turns
// that into a level with deterministic thresholds (TriageConfig). The
// assessment is forced: while one is pending, the gate refuses workspace
// writes and the ledger refuses delegate, both with "call assess_task first".
// An assessment is pending at the start of every run (the goal is a task by
// construction), whenever the listener classifies a new user message as a
// task, and whenever the run's own signals say the work has outgrown its
// last assessment (re-triage).

// RequireAssessment marks the run as needing a fresh assess_task before the
// main agent may change anything. reason is shown in refusals and in the
// notice that wakes the main agent.
func (rs *RunSession) RequireAssessment(reason string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.requireAssessmentLocked(reason, true)
}

func (rs *RunSession) requireAssessmentLocked(reason string, wake bool) {
	if rs.assessPending {
		return
	}
	rs.assessPending = true
	rs.assessReason = reason
	rs.appendEventLocked("triage", "", "assessment required: "+reason)
	if wake {
		rs.pendingNotice = joinNotice(rs.pendingNotice, "SYSTEM: "+reason+" — call assess_task before changing anything "+
			"(workspace writes and delegate are refused until you do).")
		rs.notifyLocked()
	}
}

// AssessmentPending reports whether the main agent owes an assessment.
func (rs *RunSession) AssessmentPending() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.assessPending
}

func (rs *RunSession) assessGateLocked() error {
	if !rs.assessPending {
		return nil
	}
	return fmt.Errorf("assessment pending (%s): call assess_task first — steps, modules, parallel branches, "+
		"roles needed, whether it changes code, estimated files. The engine sets the level from it; "+
		"until then workspace writes and delegate are refused", rs.assessReason)
}

// maxAssessments bounds the run record.
const maxAssessments = 20

// Assess files the main agent's assessment, runs triage, applies the level
// (unless the user pinned one on this run), posts the verdict card into the
// chat, and clears the pending state. It returns the stored assessment.
func (rs *RunSession) Assess(in model.TaskAssessment) (*model.TaskAssessment, error) {
	in.Summary = strings.TrimSpace(in.Summary)
	if in.Summary == "" {
		return nil, fmt.Errorf("summary is required: one line on what the task is")
	}
	if in.Steps < 1 {
		return nil, fmt.Errorf("steps must be ≥ 1 (your honest estimate of distinct steps)")
	}
	if in.EstFiles < 0 || in.ParallelBranches < 0 {
		return nil, fmt.Errorf("est_files and parallel_branches cannot be negative")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	cfg := rs.cfg.Workflow.EffectiveTriage()
	level, reasons := Triage(in, cfg, rs.pairMeaningfulLocked())
	in.Ts = time.Now()
	in.Level, in.Reasons = level, reasons

	cur := rs.levelLocked()
	pinned := rs.cfg.Workflow != nil && rs.cfg.Workflow.Coordinator != nil && model.LevelRank(rs.cfg.Workflow.Coordinator.Level) >= 0
	userSet := rs.run.LevelSource == "user"
	switch {
	case pinned:
		in.Applied = false
		in.Reasons = append(in.Reasons, "workflow pins level "+cur)
	case userSet:
		in.Applied = false
		in.Reasons = append(in.Reasons, "user set level "+cur+" on this run; triage does not override it")
	default:
		in.Applied = true
		if cur != level {
			rs.run.Level, rs.run.LevelSource = level, "triage"
			rs.run.LevelLog = append(rs.run.LevelLog, model.LevelChange{Ts: in.Ts, Level: level, Source: "triage", Reason: strings.Join(reasons, "; ")})
			rs.appendEventLocked("level", "", fmt.Sprintf("level → %s (triage): %s", level, strings.Join(reasons, "; ")))
		}
	}
	rs.run.Assessments = append(rs.run.Assessments, in)
	if n := len(rs.run.Assessments); n > maxAssessments {
		rs.run.Assessments = append([]model.TaskAssessment(nil), rs.run.Assessments[n-maxAssessments:]...)
	}
	rs.assessPending = false
	rs.assessReason = ""
	rs.writesAtAssess = len(rs.run.Writes)
	rs.testFailsSinceAssess = 0
	rs.reassessNoticed = false

	// The verdict card, for the user.
	card := fmt.Sprintf("%s — %s", rs.levelLocked(), in.Summary)
	if len(in.Reasons) > 0 {
		card += "\n" + strings.Join(in.Reasons, " · ")
	}
	if !in.Applied {
		card += fmt.Sprintf("\n(triage 建议 %s,未应用)", level)
	}
	rs.run.Chat = append(rs.run.Chat, model.ChatMessage{Ts: in.Ts, From: "system", Kind: model.ChatTriage, Text: card})
	rs.appendEventLocked("triage", "", fmt.Sprintf("assessed: %s → %s", in.Summary, level))
	stored := rs.run.Assessments[len(rs.run.Assessments)-1]
	return &stored, nil
}

// pairMeaningfulLocked reports whether "pair" would change anything on this
// run: resident partners are configured, or an independent agent exists the
// review gate can lean on. Without either, code changes stay solo.
func (rs *RunSession) pairMeaningfulLocked() bool {
	if rs.cfg.Workflow != nil && len(rs.cfg.Workflow.EffectivePairAgents()) > 0 {
		return true
	}
	for _, a := range rs.pool {
		if a.Independent {
			return true
		}
	}
	return false
}

// Triage is the pure decision: assessment + thresholds → level and the
// reasons that decided it (shown on the card and kept on the record).
func Triage(a model.TaskAssessment, cfg model.TriageConfig, pairMeaningful bool) (string, []string) {
	var why []string
	orchestrate := false
	if a.Steps >= cfg.OrchestrateSteps {
		why = append(why, fmt.Sprintf("%d steps (≥%d)", a.Steps, cfg.OrchestrateSteps))
		orchestrate = true
	}
	if a.ParallelBranches >= cfg.OrchestrateBranches {
		why = append(why, fmt.Sprintf("%d independent branches (≥%d)", a.ParallelBranches, cfg.OrchestrateBranches))
		orchestrate = true
	}
	if n := len(distinctLower(a.Roles)); n >= cfg.OrchestrateRoles {
		why = append(why, fmt.Sprintf("%d roles needed: %s", n, strings.Join(a.Roles, ", ")))
		orchestrate = true
	}
	if a.EstFiles >= cfg.OrchestrateFiles {
		why = append(why, fmt.Sprintf("~%d files (≥%d)", a.EstFiles, cfg.OrchestrateFiles))
		orchestrate = true
	}
	if orchestrate {
		return model.LevelOrchestrate, why
	}
	if a.ChangesCode && !cfg.PairOffForCode && pairMeaningful {
		return model.LevelPair, []string{"changes code — a second pair of eyes rides along"}
	}
	if a.ChangesCode {
		return model.LevelSolo, []string{"changes code; no resident partner configured — solo, review gate still applies"}
	}
	return model.LevelSolo, []string{fmt.Sprintf("%d steps, no code change", a.Steps)}
}

func distinctLower(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// RequestLevel is the main agent's one lever: asking to go UP. Down is the
// user's or the engine's call. A user-set level is never changed by this.
func (rs *RunSession) RequestLevel(level, why string) error {
	if model.LevelRank(level) < 0 {
		return fmt.Errorf("unknown level %q (solo | pair | orchestrate)", level)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	cur := rs.levelLocked()
	if model.LevelRank(level) <= model.LevelRank(cur) {
		return fmt.Errorf("level is %s; you may only request a HIGHER level (more structure), never a lower one — "+
			"lowering is the user's decision", cur)
	}
	if rs.run.LevelSource == "user" {
		return fmt.Errorf("the user set this run's level to %s; ask them (ask_user) if you believe it should be %s, do not request it", cur, level)
	}
	rs.run.Level, rs.run.LevelSource = level, "pilot"
	rs.run.LevelLog = append(rs.run.LevelLog, model.LevelChange{Ts: time.Now(), Level: level, Source: "pilot", Reason: why})
	rs.appendEventLocked("level", "", fmt.Sprintf("level → %s (main agent asked): %s", level, why))
	rs.run.Chat = append(rs.run.Chat, model.ChatMessage{Ts: time.Now(), From: "system", Kind: model.ChatTriage,
		Text: fmt.Sprintf("%s — main agent 申请升档:%s", level, why)})
	return nil
}

// ---- re-triage signals ----

// noteReassessLocked checks the run's own signals after a write or a failed
// acceptance and, when the work has outgrown its assessment, requires a new
// one (once per assessment).
func (rs *RunSession) noteReassessLocked() {
	if rs.assessPending || rs.reassessNoticed || len(rs.run.Assessments) == 0 {
		return
	}
	cfg := rs.cfg.Workflow.EffectiveTriage()
	files := map[string]bool{}
	for _, w := range rs.run.Writes[rs.writesAtAssess:] {
		if w.By == RoleCoordinator && w.Path != "" && writeChangesCode(&w) {
			files[w.Path] = true
		}
	}
	var reason string
	switch {
	case len(files) >= cfg.ReassessFiles:
		reason = fmt.Sprintf("you have changed %d files yourself since the last assessment (threshold %d) — the task has grown", len(files), cfg.ReassessFiles)
	case rs.testFailsSinceAssess >= cfg.ReassessTestFailures:
		reason = fmt.Sprintf("%d acceptance failures since the last assessment (threshold %d) — the plan may be wrong", rs.testFailsSinceAssess, cfg.ReassessTestFailures)
	default:
		return
	}
	rs.reassessNoticed = true
	rs.requireAssessmentLocked(reason, true)
}

// ---- listener ----

// ListenerResult records the outcome of classifying one user message. A
// failure is tolerated (the message went to the main agent regardless); the
// third consecutive failure, and every one after it, is surfaced to the user
// until a classification succeeds again.
func (rs *RunSession) ListenerResult(kind string, err error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if err == nil {
		rs.listenerFails = 0
		rs.appendEventLocked("listener", "", "message classified: "+kind)
		return
	}
	rs.listenerFails++
	rs.appendEventLocked("listener", "", fmt.Sprintf("classification failed (%d in a row): %s", rs.listenerFails, firstLine(err.Error(), 160)))
	if rs.listenerFails >= 3 {
		rs.run.Chat = append(rs.run.Chat, model.ChatMessage{Ts: time.Now(), From: "system", Kind: model.ChatNotice,
			Text: fmt.Sprintf("监听器连续 %d 次分类失败(%s)。消息已照常交给 main agent;新任务不会被自动识别——需要时手动改 level 或让 main agent 重新评估。", rs.listenerFails, firstLine(err.Error(), 160))})
	}
	if rs.cfg.OnChange != nil {
		rs.cfg.OnChange(rs.run)
	}
}
