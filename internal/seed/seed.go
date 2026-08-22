// Package seed populates an empty data dir with a starter agent pool and two
// example workflows so the UI is useful on first launch.
package seed

import (
	"time"

	"loom/internal/model"
	"loom/internal/store"
)

// The pool is built so any two agents differ substantively in their
// (model, tool allowlist) pair — not just in prompt. A pair that differs only
// in prompt is a nominal split: it buys a handoff cost without buying any
// isolation, so such roles are merged (implement+test is one agent here).
var agents = []*model.Agent{
	{
		Name:        "researcher",
		Description: "调研与分析:梳理需求、调查现状、产出结论与建议。纯推理,不改代码;报告经 write_artifact 交付为 md 文档。",
		Model:       "claude-sonnet-5",
		SystemPrompt: `You are a research and analysis specialist. Investigate the given question thoroughly,
structure your findings, and end with clear, actionable conclusions. Be concrete and cite the
reasoning behind each conclusion. You do not modify code. Deliver your findings as a Markdown
document in the workspace using the write_artifact tool; keep your reply text to a short
summary pointing at the file.`,
	},
	{
		Name:        "architect",
		Description: "方案设计:把目标拆成清晰的技术方案与文件结构,输出设计文档。可写设计文件,不可执行。",
		Model:       "claude-opus-5",
		Tools:       "Read,Write",
		MaxTurns:    15,
		SystemPrompt: `You are a software architect. Turn the given goal into a concrete technical design:
components, file layout, interfaces, and tradeoffs considered. Write the design as DESIGN.md
in the working directory unless instructed otherwise. Keep it implementable, not aspirational.`,
	},
	{
		Name:        "implementer",
		Description: "编码与自测:按设计实现代码,并编写、运行配套测试,保证任务的验收检查真实通过。",
		Model:       "claude-sonnet-5",
		Tools:       "Read,Write,Edit,Bash",
		MaxTurns:    40,
		SystemPrompt: `You are a senior software engineer. Implement exactly what the task asks, in the working
directory, and write and run the tests that prove it works — implementation and its tests are one
job, not two. Prefer simple, readable code. Run the task's acceptance commands yourself before
finishing; never claim success you have not seen pass. List every file you created or changed.`,
	},
	{
		Name:        "reviewer",
		Description: "独立评审:仅凭产物与需求评审,产出按严重度排序的问题清单。只读(可自行发现文件),不受作者叙述影响。",
		Model:       "claude-opus-5",
		Tools:       "Read,Grep,Glob",
		MaxTurns:    20,
		Independent: true,
		SystemPrompt: `You are an independent reviewer with fresh eyes. You receive ONLY the requirement, the
acceptance criteria, and artifact paths — never the author's own account, by design: your value is
seeing what the author cannot. Read the artifacts and report defects ranked by severity:
correctness first, then robustness, then clarity. For each issue give file, location, what breaks,
and a suggested fix. Do not modify anything, and do not assume anything you cannot see.`,
	},
	{
		Name:        "doc-writer",
		Description: "文档撰写:为 workspace 的成果撰写 README/使用说明。",
		Model:       "claude-haiku-4-5",
		Tools:       "Read,Write",
		MaxTurns:    15,
		SystemPrompt: `You are a technical writer. Document what exists in the working directory: what it is,
how to run it, and how it is structured. Write for a reader seeing the project for the first
time. Output README.md unless instructed otherwise.`,
	},
}

var workflows = []*model.Workflow{
	{
		ID:          "wf-feature-dev",
		Name:        "功能开发",
		Description: "设计 → 实现 → 测试 → 评审 → 文档的完整开发流水线。计划需人工审批,失败自动 replan。",
		Planner: model.PlannerConfig{
			Model:    "claude-sonnet-5",
			MaxNodes: 8,
			SystemPrompt: "Prefer this shape: architect designs first; implementation may split into parallel " +
				"independent parts (the implementer writes AND runs its own tests); reviewer runs after " +
				"implementation with only the requirement and artifact paths; doc-writer last. Skip stages " +
				"that the goal clearly doesn't need.",
		},
		AgentPool:       []string{"architect", "implementer", "reviewer", "doc-writer"},
		RequireApproval: true,
		ReplanEnabled:   true,
		MaxReplans:      2,
		MaxRetries:      1,
		Parallelism:     3,
	},
	{
		ID:          "wf-auto-compose",
		Name:        "自由编排",
		Description: "planner 全自主:按目标从全池选人,缺少的专家自己定义并入池。计划(含新 agent)需审批。",
		Planner: model.PlannerConfig{
			Model:    "claude-opus-5",
			MaxNodes: 8,
			SystemPrompt: "Prefer existing registry agents; define a new specialist only when the goal needs " +
				"expertise none of them cover. New agents must be reusable specialists, not one-off task holders.",
		},
		AgentPool:          nil, // whole pool
		AllowAgentCreation: true,
		RequireApproval:    true,
		ReplanEnabled:      true,
		MaxReplans:         2,
		MaxRetries:         1,
		Parallelism:        3,
	},
	{
		ID:   "wf-dynamic",
		Name: "动态编排",
		Description: "coordinator 运行时分解任务、委派、跟进、收敛。任务树涌现而非预先声明;" +
			"终止靠预算硬限而非 DAG 结构。首批委派需审批。",
		Mode: model.ModeDynamic,
		Coordinator: &model.CoordinatorConfig{
			Model: "claude-opus-5",
			SystemPrompt: "Bias toward two or three substantial tasks over many tiny ones — each round trip " +
				"costs a turn. Verify deliverables in the workspace before declaring success.",
		},
		Budget: &model.BudgetConfig{
			MaxTasks:           30,
			MaxDelegationDepth: 3,
			MaxParallel:        3,
			TaskTimeoutSec:     1800,
			RunTimeoutSec:      36000,
			MaxTurnsPerTask:    6,
			StallSec:           600,
			ApprovalPolicy:     model.ApprovalInitial,
		},
		AgentPool: nil, // whole pool
	},
	{
		ID:          "wf-quick-analysis",
		Name:        "快速分析",
		Description: "轻量调研/分析任务:无审批门,直接执行,适合演示和小问题。",
		Planner: model.PlannerConfig{
			Model:    "claude-sonnet-5",
			MaxNodes: 5,
		},
		AgentPool:     []string{"researcher", "reviewer", "doc-writer"},
		ReplanEnabled: true,
		MaxReplans:    1,
		MaxRetries:    1,
		Parallelism:   3,
	},
}

// EnsureDefaults writes seed data for whichever collection is empty.
func EnsureDefaults(st *store.Store) error {
	existing, err := st.ListAgents()
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		for _, a := range agents {
			if err := st.SaveAgent(a); err != nil {
				return err
			}
		}
	}
	wfs, err := st.ListWorkflows()
	if err != nil {
		return err
	}
	if len(wfs) == 0 {
		for _, w := range workflows {
			w.CreatedAt = time.Now()
			if err := st.SaveWorkflow(w); err != nil {
				return err
			}
		}
	}
	return nil
}
