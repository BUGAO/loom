// Package llm abstracts the model runtime behind two interfaces:
//
//	Backend        one-shot completion — planning, and static-mode nodes
//	SessionBackend a live session that can be prompted repeatedly — dynamic mode
//
// "acp" runs agents over the Agent Client Protocol (each agent gets its own
// home, AGENTS.md and private skills); "claude" shells out to the Claude Code
// CLI in headless mode; "mock" returns canned results for zero-cost demos and
// tests.
package llm

import (
	"context"

	"loom/internal/model"
)

// Kind of completion being requested; lets simple backends (mock) shape output.
const (
	KindPlan = "plan"
	KindNode = "node"
	// KindCoordinator and KindWorker identify the two dynamic-mode roles. They
	// matter to the mock backend, which scripts a different tool sequence for
	// each; real backends treat them like any other session.
	KindCoordinator = "coordinator"
	KindWorker      = "worker"
)

type Request struct {
	Kind         string
	SystemPrompt string
	Prompt       string
	Model        string
	WorkDir      string       // session cwd (the agent's own home workspace)
	AddDirs      []string     // extra dirs the agent may access (e.g. run exchange dir)
	Tools        string       // comma-separated allowed tools, "" = no tools
	MaxTurns     int          // 0 = backend default
	Pool         []string     // agent names available (used by mock planner)
	OnActivity   func(string) // optional live-activity callback (tool calls etc.)
}

type Result struct {
	Text       string // agent's message text (envelope is parsed from this)
	Transcript string // full interleaved transcript incl. tool calls; "" = same as Text
	Model      string // model that actually served the call, for costing
	StopReason string // why the turn ended, verbatim from the runtime ("" = unknown/normal)
	Usage      model.TokenUsage
	CostUSD    float64
	DurationMs int64
	NumTurns   int
}

type Backend interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Result, error)
}

// MCPServer is one HTTP MCP endpoint offered to a session. Dynamic mode uses
// this to hand an agent its hub toolset; the token rides in a header rather
// than in a config file under the agent's home, because homes are persistent
// and shared across concurrent tasks (see docs/DECISIONS-v2.md D6).
type MCPServer struct {
	Name    string
	URL     string
	Headers map[string]string
}

// SessionRequest opens a live agent session.
type SessionRequest struct {
	Kind         string
	SystemPrompt string
	Model        string
	WorkDir      string
	AddDirs      []string
	Tools        string
	MaxTurns     int
	MCPServers   []MCPServer
	OnActivity   func(string)
}

// Session is one live agent conversation. Prompt may be called repeatedly;
// each call is one turn. Implementations must be safe for a single driving
// goroutine plus concurrent Cancel/Close.
type Session interface {
	Prompt(ctx context.Context, text string) (*Result, error)
	Cancel(ctx context.Context) error
	Close() error
}

// Image is one inline image attachment for a prompt turn.
type Image struct {
	Name     string // display / audit name (upload filename)
	MimeType string // image/png, image/jpeg, image/webp, image/gif
	Data     []byte // raw bytes (encoding is the transport's business)
}

// ImageSession is optionally implemented by sessions whose transport can carry
// inline images with a prompt. Callers must check for it and fall back to a
// text-only Prompt when it is absent — never assume every runtime has eyes.
type ImageSession interface {
	PromptImages(ctx context.Context, text string, images []Image) (*Result, error)
}

// SessionBackend is a backend that can hold a session open. Backends without a
// native session concept (the CLI) are adapted by SingleShotSessions.
type SessionBackend interface {
	Open(ctx context.Context, req SessionRequest) (Session, error)
}

// Sessions returns a SessionBackend for be: its own implementation if it has
// one, otherwise a stateless adapter that replays the conversation on every
// turn. The adapter cannot host MCP tools, so dynamic mode rejects backends
// that end up on that path.
func Sessions(be Backend) SessionBackend {
	if sb, ok := be.(SessionBackend); ok {
		return sb
	}
	return &singleShotBackend{be: be}
}
