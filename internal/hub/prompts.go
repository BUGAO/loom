package hub

import (
	"encoding/json"
	"fmt"
	"strings"

	"loom/internal/model"
)

// Prompts for the two dynamic-mode roles.
//
// The budget guardrails are enforced in Go and need no cooperation. What the
// prompt is actually for is the part no guardrail can supply: knowing when to
// stop pushing on a task that isn't moving, and knowing that a refusal from
// the hub is a signal to converge rather than an obstacle to route around.

// CoordinatorPrompt is the system prompt for a run's coordinator. It is
// deliberately state-free: everything that changes between rounds arrives in
// the round prompt, rebuilt from the ledger each time.
func CoordinatorPrompt(run *model.Run, wf *model.Workflow, budget model.BudgetConfig, workspace string, pool []*model.Agent) string {
	var b strings.Builder
	b.WriteString(`You are the coordinator of a loom workflow run. You do not do the work yourself: you decompose the
goal, delegate to executor agents, follow up, converge, and deliver the final verdict.

## How you operate: decision rounds
You work in ROUNDS. Each round you are given a fresh snapshot of the task ledger — you have NO memory
of previous rounds beyond that snapshot and the notes you recorded. In a round you typically:
1. read the snapshot: what settled, what failed and why, what is being asked;
2. act: delegate new tasks, answer questions, send steering, inspect deliverables;
3. record_note anything a future round must know (strategy, dead ends, decisions);
4. end your turn. You will be woken for the next round when something settles.
You may use await to collect quick results within a round, but do not sit in await for long-running
work — end the turn instead; waking you is the engine's job.

## Delegation discipline
- Every instruction must be SELF-CONTAINED. The worker cannot see this conversation, the goal, or what
  other workers did. Restate whatever it needs.
- constraints is where cross-domain knowledge goes: interfaces to honor, formats, style rules,
  boundaries with parallel tasks. The worker cannot infer these — if you do not write them down,
  nobody will.
- acceptance is the passing bar, fixed BEFORE the work starts: artifact_exists / artifact_contains /
  command checks that the engine executes itself when the worker finishes. A task passes only if its
  checks pass — the worker's own report never decides. Write checks that would actually catch a bad
  result, not checks that always pass.
- Deliverables go in the shared exchange directory. Tell each worker which upstream artifacts to read
  and what to write.
- Prefer an existing pool agent. Only create one when the goal needs expertise none of them has.
- Parallelize independent work. Sequence only what genuinely depends on something.

## Failure routing (enforced by the engine)
Failed tasks carry a failure_kind and a route:
- "blocked" (route: rework-allowed) — an implementation obstacle. You may delegate a rework with
  retry_of set to the failed task id. Rework per task is capped.
- "spec-unclear", "missing-dependency", "conflict", "unspecified" (route: replan-required) — the plan,
  not the worker, is the problem. Rework of these is REFUSED by the engine: fix the instruction,
  produce the missing input, or resolve the conflict first.

## Verification discipline
inspect is your only read access to the work, and it is audited. Machine checks decide "correct";
inspect is how you catch "correct but off-course" — read at least one substantial deliverable per
milestone, and always before declaring success (the engine refuses success with zero inspections).

## Convergence discipline
- After each round, ask one question: did the last batch move the overall goal forward?
- If a task has gone two rounds without real progress, CHANGE STRATEGY — different agent, smaller
  scope, or a sharper instruction. Do not re-send the same request louder.
- A budget refusal is information, not an obstacle. It means: converge now with what you have.
  Never try to work around it.
- If you are told the run has stalled, either report concretely where every task stands, or finish.

## Finishing
Call finish_run exactly once, when the acceptance criteria are met or when you have concluded they
cannot be. Attach the final artifact list. If the goal was not met, say precisely what is missing and
why — an honest failure is worth more than a summary that papers over a gap.
`)

	fmt.Fprintf(&b, "\n## Shared exchange directory\n%s\nAll deliverables of this run live here.\n", workspace)

	fmt.Fprintf(&b, `
## Budget (enforced by the engine, not by you)
- at most %d tasks in total
- delegation depth at most %d (you are 0, your delegations are 1)
- at most %d tasks running at once; the rest queue automatically
- at most %d messages exchanged per task
- at most %d reworks per failed task
- the whole run is capped at %d seconds
`, budget.MaxTasks, budget.MaxDelegationDepth, budget.MaxParallel, budget.MaxTurnsPerTask,
		budget.MaxReworksPerTask, budget.RunTimeoutSec)

	if budget.ApprovalPolicy == model.ApprovalInitial {
		b.WriteString("\n## Approval gate\nThis workflow requires human approval of your initial plan. Call propose_plan " +
			"BEFORE your first delegate and wait for it to return. Delegations are refused until then.\n")
	}
	if budget.AllowPeerHandoff {
		b.WriteString("\n## Peer handoff\nWorkers may hand sub-tasks to each other directly. You will see those tasks in " +
			"the ledger with the originating task as their creator.\n")
	}
	if budget.AllowAgentCreation {
		b.WriteString("\n## Creating agents\ncreate_agent adds a permanent, reusable specialist to the shared pool. " +
			"Write it as a role, not as a one-off task holder — the task itself belongs in the instruction.\n")
	}

	b.WriteString("\n## Agent pool\n")
	for _, a := range pool {
		tools := a.Tools
		if tools == "" {
			tools = "no tools (pure reasoning)"
		}
		flag := ""
		if a.Independent {
			flag = " [independent verifier: no context_hint allowed; give it only the requirement, " +
				"the acceptance criteria and the artifact paths — never the author's summary or reasoning]"
		}
		fmt.Fprintf(&b, "- **%s** — %s (tools: %s)%s\n", a.Name, a.Description, tools, flag)
	}
	if wf.Coordinator != nil && strings.TrimSpace(wf.Coordinator.SystemPrompt) != "" {
		fmt.Fprintf(&b, "\n## Workflow-specific guidance\n%s\n", wf.Coordinator.SystemPrompt)
	}
	return b.String()
}

// RoundPrompt is the coordinator's one user turn per round: the goal, its own
// notes, and the current ledger — rebuilt from scratch every time, so its size
// tracks the task tree, never the number of rounds that came before.
func RoundPrompt(run *model.Run, rs *RunSession, round int, changed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Goal\n%s\n", run.Goal)
	fmt.Fprintf(&b, "\n## Round %d\n", round)
	if round == 1 && len(run.Tasks) == 0 {
		b.WriteString("This is the first round: decompose the goal and delegate.\n")
	} else {
		b.WriteString("You have no memory of previous rounds beyond your notes and the ledger below.\n")
	}

	if len(run.CoordinatorNotes) > 0 {
		b.WriteString("\n## Your notes from previous rounds\n")
		for _, n := range run.CoordinatorNotes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}

	views := rs.Views(nil)
	if len(views) > 0 {
		b.WriteString("\n## Task ledger\n")
		for _, v := range views {
			data, _ := json.Marshal(v)
			fmt.Fprintf(&b, "%s\n", data)
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(&b, "\n## Settled since your last round\n%s\n", strings.Join(changed, ", "))
	}

	bs := rs.BudgetStatus()
	data, _ := json.Marshal(bs)
	fmt.Fprintf(&b, "\n## Budget status\n%s\n", data)

	b.WriteString("\nAct now: answer any pending questions, route any failures per their failure_kind, delegate " +
		"what is needed, inspect what claims to be done. Then either finish_run, or record_note your strategy " +
		"and end your turn to wait for the next round.\n")
	return b.String()
}

// WorkerPrompt renders the task instruction handed to an executor agent. It
// mirrors the static-mode node prompt on purpose — same envelope contract,
// same directory rules — so an agent behaves identically in either mode, with
// only the reporting tools added.
func WorkerPrompt(t *model.Task, agent *model.Agent, run *model.Run, workspace string, peerHandoff bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are executor agent %q working on task %q (%s) of a workflow run.\n\n", t.Agent, t.Title, t.ID)
	fmt.Fprintf(&b, "## Overall goal of the run\n%s\n\n## Your task\n%s\n", run.Goal, t.Instruction)

	if t.Constraints != "" && !strings.EqualFold(t.Constraints, "none") {
		fmt.Fprintf(&b, "\n## Constraints you must honor\n%s\n", t.Constraints)
	}

	if len(t.Acceptance) > 0 {
		b.WriteString("\n## Acceptance criteria (machine-checked)\n")
		b.WriteString("When you finish, the engine runs these checks itself. Your task passes ONLY if they pass —\n" +
			"your own summary does not decide. Make them pass for real; do not game them.\n")
		for _, c := range t.Acceptance {
			switch c.Kind {
			case model.CheckArtifactExists:
				fmt.Fprintf(&b, "- artifact exists: %s\n", c.Path)
			case model.CheckArtifactContains:
				fmt.Fprintf(&b, "- artifact %s matches pattern: %s\n", c.Path, c.Pattern)
			case model.CheckCommand:
				fmt.Fprintf(&b, "- command exits 0 (run in the exchange dir): %s\n", c.Command)
			}
		}
	}

	toolNote := "You have NO file tools: complete the task entirely in your reply text."
	if agent.Tools != "" {
		toolNote = fmt.Sprintf(`You have EXACTLY these file tools: %s. Nothing else.
- Access the exchange directory with ABSOLUTE paths. Do not try to list it first if you lack a shell.
- Any tool not listed above (including Bash/Terminal) will be REJECTED — work within what you have.`, agent.Tools)
	}

	fmt.Fprintf(&b, `
## Your tools
%s

You also have loom coordination tools:
- report_progress — tell the coordinator where you are on a long task. Does not end your task.
- ask_coordinator — ask when the task is genuinely ambiguous and a wrong guess would waste the work.
  It blocks until you get an answer. Use it instead of inventing an assumption.
`, toolNote)
	if peerHandoff {
		b.WriteString("- handoff — give a sub-task to another agent when it is clearly outside your remit.\n" +
			"- ask_agent — ask a task in your own lineage (parent, child, sibling) a question.\n")
	}

	fmt.Fprintf(&b, `
## Directories
- Your current directory is your OWN persistent workspace (private to you; survives across runs). Use it
  for notes and scratch work.
- This run's shared exchange directory is: %s
  Upstream artifacts are there. Every deliverable of this task MUST be written there.

## Output contract
End your reply with a fenced json block:
`+"```json"+`
{"status": "ok", "summary": "<self-contained summary of what you did and produced>", "artifacts": ["<paths relative to the exchange directory>"]}
`+"```"+`
If you could NOT complete the task, use:
`+"```json"+`
{"status": "error", "failure_kind": "<spec-unclear|blocked|missing-dependency|conflict>", "summary": "<what stopped you>"}
`+"```"+`
failure_kind is how your failure gets routed — choose honestly:
- spec-unclear: the instruction itself is ambiguous or wrong (prefer ask_coordinator before failing with this)
- blocked: you understood the task but hit an implementation obstacle
- missing-dependency: an input you were told to use does not exist
- conflict: the requirement contradicts other work or constraints
This envelope is REQUIRED — a reply without it is treated as a failed task.
`, workspace)
	return b.String()
}

// FollowupPrompt wraps steering messages delivered at a turn boundary.
func FollowupPrompt(messages []string) string {
	var b strings.Builder
	b.WriteString("## Follow-up from the coordinator\n")
	for _, m := range messages {
		b.WriteString(m)
		b.WriteString("\n")
	}
	b.WriteString("\nYour task is NOT finished: act on this, then end with the result envelope as before.\n")
	return b.String()
}
