package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"loom/internal/model"
)

// Claude runs completions via the Claude Code CLI in headless mode
// (`claude -p --output-format json`). Tool-using agents work inside
// req.WorkDir with only the tools their definition allows.
type Claude struct {
	Bin string // path to the claude binary, default "claude"
}

func (c *Claude) Name() string { return "claude" }

// cliResult is the JSON envelope emitted by `claude -p --output-format json`.
// ModelUsage is keyed by model id; its cost is already API-equivalent, so this
// path needs no catalog pricing — but the token breakdown still gets recorded
// so per-model reporting works the same as on the ACP path.
type cliResult struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	TotalCost  float64 `json:"total_cost_usd"`
	DurationMs int64   `json:"duration_ms"`
	NumTurns   int     `json:"num_turns"`
	ModelUsage map[string]struct {
		InputTokens              int64 `json:"inputTokens"`
		OutputTokens             int64 `json:"outputTokens"`
		CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
		CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	} `json:"modelUsage"`
}

// usage flattens modelUsage, also reporting the model that did most of the
// generation (multi-model turns are rare; the majority one is the honest label).
func (cr cliResult) usage() (model.TokenUsage, string) {
	var sum model.TokenUsage
	top, topOut := "", int64(-1)
	for id, u := range cr.ModelUsage {
		sum.Add(model.TokenUsage{
			Input:      u.InputTokens,
			Output:     u.OutputTokens,
			CacheWrite: u.CacheCreationInputTokens,
			CacheRead:  u.CacheReadInputTokens,
		})
		if u.OutputTokens > topOut {
			top, topOut = id, u.OutputTokens
		}
	}
	return sum, top
}

func (c *Claude) Complete(ctx context.Context, req Request) (*Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", "--output-format", "json"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if req.Tools != "" {
		// --tools limits availability; --allowedTools pre-approves them so
		// headless runs don't stall on permission prompts.
		args = append(args, "--tools", req.Tools, "--allowedTools", req.Tools,
			"--permission-mode", "acceptEdits")
	} else {
		args = append(args, "--tools", "")
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(req.MaxTurns))
	}
	for _, d := range req.AddDirs {
		args = append(args, "--add-dir", d)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start).Milliseconds()

	// Even on non-zero exit the CLI usually prints a JSON result; try to parse.
	var cr cliResult
	parseErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &cr)
	if parseErr != nil {
		if err != nil {
			return nil, fmt.Errorf("claude cli: %w: %s", err, firstLine(stderr.String()))
		}
		return nil, fmt.Errorf("claude cli: unparseable output: %w", parseErr)
	}
	usage, topModel := cr.usage()
	if topModel == "" {
		topModel = req.Model
	}
	res := &Result{Text: cr.Result, Model: topModel, Usage: usage,
		CostUSD: cr.TotalCost, DurationMs: cr.DurationMs, NumTurns: cr.NumTurns}
	if res.DurationMs == 0 {
		res.DurationMs = elapsed
	}
	if cr.IsError {
		return res, fmt.Errorf("claude cli error (%s): %s", cr.Subtype, truncate(cr.Result, 500))
	}
	return res, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
