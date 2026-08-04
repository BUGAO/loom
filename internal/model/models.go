package model

// ModelOption is one entry of the curated Claude model catalog — the single
// source of truth for the UI dropdown, for validating planner-created agents,
// and for costing token usage. IDs are fixed, date-suffix-free identifiers
// accepted by both the claude CLI (--model) and ACP (ANTHROPIC_MODEL).
//
// Prices are official per-million-token API list prices in USD. loom's ACP and
// CLI executions usually ride a subscription and are not billed per token at
// all; the catalog price exists to give every run a single comparable unit of
// "what would this have cost on the API" — see CostOf and note that every
// display of it is labelled as an estimate, not a bill.
//
// Deliberately no time-based price tiers: the ledger's job is comparing agents
// and workflows against each other, and a promotional price that expires
// mid-history would make runs on either side of the date incomparable.
type ModelOption struct {
	ID            string  `json:"id"`
	Label         string  `json:"label"`
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

var ModelCatalog = []ModelOption{
	{ID: "claude-fable-5", Label: "Fable 5(最强)", InputPerMTok: 10, OutputPerMTok: 50},
	{ID: "claude-opus-5", Label: "Opus 5(旗舰)", InputPerMTok: 5, OutputPerMTok: 25},
	{ID: "claude-opus-4-8", Label: "Opus 4.8", InputPerMTok: 5, OutputPerMTok: 25},
	{ID: "claude-sonnet-5", Label: "Sonnet 5(均衡)", InputPerMTok: 3, OutputPerMTok: 15},
	{ID: "claude-sonnet-4-6", Label: "Sonnet 4.6", InputPerMTok: 3, OutputPerMTok: 15},
	{ID: "claude-haiku-4-5", Label: "Haiku 4.5(快/省)", InputPerMTok: 1, OutputPerMTok: 5},
}

const DefaultModel = "claude-opus-5"

// RuntimeOption is one entry of the execution-runtime catalog: the ways an
// agent's sessions can be hosted. Today there is exactly one real runtime
// (Claude Code over ACP); the catalog exists so adding codex etc. later is a
// registry entry, not a schema change. Dry-run is NOT a runtime — it is a
// per-run switch that bypasses whatever runtime the agent declares.
type RuntimeOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var RuntimeCatalog = []RuntimeOption{
	{ID: "claude", Label: "Claude Code · ACP"},
}

const DefaultRuntime = "claude"

func ValidRuntime(id string) bool {
	if id == "" {
		return true // empty = default
	}
	for _, r := range RuntimeCatalog {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Cache multipliers relative to the model's input price.
const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.10
)

func ValidModel(id string) bool {
	for _, m := range ModelCatalog {
		if m.ID == id {
			return true
		}
	}
	return false
}

func lookupModel(id string) (ModelOption, bool) {
	for _, m := range ModelCatalog {
		if m.ID == id {
			return m, true
		}
	}
	return ModelOption{}, false
}

// TokenUsage is the token breakdown of one or more model calls.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
}

func (u TokenUsage) Empty() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheWrite == 0 && u.CacheRead == 0
}

func (u *TokenUsage) Add(o TokenUsage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheWrite += o.CacheWrite
	u.CacheRead += o.CacheRead
}

// CostOf prices usage at the catalog's list price for the model. An unknown
// model costs 0 — loom would rather report nothing than a made-up number.
func CostOf(modelID string, u TokenUsage) float64 {
	m, ok := lookupModel(modelID)
	if !ok {
		return 0
	}
	const perMTok = 1_000_000.0
	in, out := m.InputPerMTok/perMTok, m.OutputPerMTok/perMTok
	return float64(u.Input)*in +
		float64(u.Output)*out +
		float64(u.CacheWrite)*cacheWriteMultiplier*in +
		float64(u.CacheRead)*cacheReadMultiplier*in
}
