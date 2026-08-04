package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// ACP executes agents over the Agent Client Protocol via coder/acp-go-sdk:
// one agent process + session, cwd = the agent's own home workspace, so
// AGENTS.md and the agent's private .claude/skills are loaded natively by the
// runtime. Tool restriction is enforced by answering permission requests per
// the agent's allowed-tool list. Model selection rides on ANTHROPIC_MODEL.
//
// Sessions are the primitive: Open keeps the process and session alive for as
// many turns as the caller needs (dynamic mode), and Complete is the one-turn
// wrapper the static engine and planner use.
type ACP struct {
	Command string   // path to an ACP agent binary (e.g. claude-code-acp)
	Args    []string // extra args for the agent process
}

func (a *ACP) Name() string { return "acp" }

var _ SessionBackend = (*ACP)(nil)

// allowedFn maps ACP tool-call kinds/titles onto the agent's tool list.
func allowedFn(tools string) func(title, kind string) bool {
	set := map[string]bool{}
	for _, t := range strings.Split(tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			set[strings.ToLower(t)] = true
		}
	}
	return func(title, kind string) bool {
		switch kind {
		case "read", "search":
			return set["read"] || set["grep"] || set["glob"]
		case "edit", "delete", "move":
			return set["write"] || set["edit"]
		case "execute":
			return set["bash"]
		case "fetch":
			return set["webfetch"] || set["websearch"]
		default:
			// Unknown kind: fall back to matching the title against the list.
			lt := strings.ToLower(title)
			for name := range set {
				if strings.Contains(lt, name) {
					return true
				}
			}
			return false
		}
	}
}

// hubToolRe matches the hub's own MCP tools, which arrive as an unknown kind
// and must never be filtered by the agent's file-tool allowlist: they are the
// orchestration channel, not a capability the agent was granted.
func isHubTool(title string) bool {
	return strings.Contains(strings.ToLower(title), loomToolPrefix)
}

// loomToolPrefix is the MCP server name the hub registers under; tool titles
// surfaced by the runtime contain it.
const loomToolPrefix = "loom"

// acpClient implements acp.Client for one session: it streams session updates
// into the collectors and answers permission requests by policy.
// fs/terminal methods are never expected — we advertise those capabilities off.
type acpClient struct {
	allow func(title, kind string) bool

	mu         sync.Mutex
	onMessage  func(text string)
	onThought  func(text string)
	onToolCall func(title, kind string)
}

var _ acp.Client = (*acpClient)(nil)

func (c *acpClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	title, kind := "", ""
	if p.ToolCall.Title != nil {
		title = *p.ToolCall.Title
	}
	if p.ToolCall.Kind != nil {
		kind = string(*p.ToolCall.Kind)
	}
	want := acp.PermissionOptionKindRejectOnce
	if isHubTool(title) || c.allow(title, kind) {
		want = acp.PermissionOptionKindAllowOnce
	}
	var optionID acp.PermissionOptionId
	for _, o := range p.Options {
		if o.Kind == want {
			optionID = o.OptionId
			break
		}
	}
	if optionID == "" && len(p.Options) > 0 {
		optionID = p.Options[0].OptionId
	}
	if optionID == "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: optionID}}}, nil
}

func (c *acpClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	onMessage, onThought, onToolCall := c.onMessage, c.onThought, c.onToolCall
	c.mu.Unlock()
	if onMessage == nil {
		return nil // between turns: nothing is collecting
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil {
			onMessage(t.Text)
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil {
			onThought(t.Text)
		}
	case u.ToolCall != nil:
		onToolCall(u.ToolCall.Title, string(u.ToolCall.Kind))
	}
	return nil
}

func (c *acpClient) setCollectors(msg, thought func(string), tool func(string, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage, c.onThought, c.onToolCall = msg, thought, tool
}

func (c *acpClient) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, fmt.Errorf("fs.readTextFile capability not advertised")
}

func (c *acpClient) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, fmt.Errorf("fs.writeTextFile capability not advertised")
}

func (c *acpClient) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("terminal capability not advertised")
}

func (c *acpClient) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, fmt.Errorf("terminal capability not advertised")
}

func (c *acpClient) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, fmt.Errorf("terminal capability not advertised")
}

func (c *acpClient) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, fmt.Errorf("terminal capability not advertised")
}

func (c *acpClient) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, fmt.Errorf("terminal capability not advertised")
}

// scrubEnv drops inherited Claude Code session markers: loom is a standalone
// orchestrator, and without this the adapter refuses to start when loom itself
// was launched from inside a Claude Code session (nested-session guard).
func scrubEnv(model string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
			"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_PID":
			continue
		}
		env = append(env, kv)
	}
	if model != "" {
		env = append(env, "ANTHROPIC_MODEL="+model)
	}
	return env
}

// acpSession is one live agent process + ACP session.
type acpSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	conn    *acp.ClientSideConnection
	client  *acpClient
	id      acp.SessionId
	req     SessionRequest
	stderr  *bytes.Buffer
	closeMu sync.Mutex
	closed  bool

	usageMu   sync.Mutex
	lastUsage modelUsage // per-model totals as of the previous turn
}

var _ Session = (*acpSession)(nil)

func (a *ACP) Open(ctx context.Context, req SessionRequest) (Session, error) {
	cmd := exec.Command(a.Command, a.Args...)
	cmd.Dir = req.WorkDir
	cmd.Env = scrubEnv(req.Model)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", a.Command, err)
	}

	s := &acpSession{cmd: cmd, stdin: stdin, stderr: stderr, req: req, client: &acpClient{allow: allowedFn(req.Tools)}}
	s.conn = acp.NewClientSideConnection(s.client, stdin, stdout)
	s.conn.SetLogger(slog.New(slog.DiscardHandler)) // teardown noise isn't loom's log

	initCtx, cancelInit := context.WithTimeout(ctx, 60*time.Second)
	defer cancelInit()
	if _, err := s.conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
		},
	}); err != nil {
		s.Close()
		return nil, fmt.Errorf("acp initialize: %w (stderr: %s)", err, s.stderrTail())
	}
	sess, err := s.conn.NewSession(initCtx, acp.NewSessionRequest{
		Cwd:        req.WorkDir,
		McpServers: mcpServers(req.MCPServers),
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("acp session: %w (stderr: %s)", err, s.stderrTail())
	}
	s.id = sess.SessionId
	// Auto-accept edits only when the agent is entitled to them; everything
	// else flows through RequestPermission and our policy.
	if s.client.allow("", "edit") {
		s.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: sess.SessionId, ModeId: acp.SessionModeId("acceptEdits")})
	}
	return s, nil
}

// mcpServers renders loom's MCP endpoints as ACP HTTP transport entries. The
// adapter advertises mcpCapabilities.http, and passes name/url/headers straight
// through to the Claude Code SDK.
func mcpServers(in []MCPServer) []acp.McpServer {
	out := make([]acp.McpServer, 0, len(in))
	for _, m := range in {
		headers := make([]acp.HttpHeader, 0, len(m.Headers))
		for k, v := range m.Headers {
			headers = append(headers, acp.HttpHeader{Name: k, Value: v})
		}
		out = append(out, acp.McpServer{Http: &acp.McpServerHttpInline{
			Type: "http", Name: m.Name, Url: m.URL, Headers: headers,
		}})
	}
	return out
}

func (s *acpSession) stderrTail() string {
	str := strings.TrimSpace(s.stderr.String())
	if len(str) > 500 {
		str = "…" + str[len(str)-500:]
	}
	return str
}

func (s *acpSession) Prompt(ctx context.Context, text string) (*Result, error) {
	var msgText, transcript strings.Builder
	lastThought := false
	s.client.setCollectors(
		func(t string) {
			msgText.WriteString(t)
			transcript.WriteString(t)
			lastThought = false
		},
		func(t string) {
			if !lastThought {
				transcript.WriteString("\n> [thinking] ")
				lastThought = true
			}
			transcript.WriteString(strings.ReplaceAll(t, "\n", " "))
		},
		func(title, kind string) {
			fmt.Fprintf(&transcript, "\n[tool:%s] %s\n", kind, title)
			lastThought = false
			if s.req.OnActivity != nil && title != "" {
				s.req.OnActivity(title)
			}
		},
	)
	defer s.client.setCollectors(nil, nil, nil)

	start := time.Now()
	// Drive the prompt on an inner context so that on cancellation we can send
	// session/cancel and give the agent a grace period to wind down the turn.
	promptCtx, cancelPrompt := context.WithCancel(context.Background())
	defer cancelPrompt()
	go func() {
		select {
		case <-ctx.Done():
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			s.conn.Cancel(c, acp.CancelNotification{SessionId: s.id})
			cc()
			select {
			case <-promptCtx.Done():
			case <-time.After(5 * time.Second):
				cancelPrompt()
			}
		case <-promptCtx.Done():
		}
	}()

	resp, err := s.conn.Prompt(promptCtx, acp.PromptRequest{
		SessionId: s.id,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	res := &Result{
		Text:       msgText.String(),
		Transcript: transcript.String(),
		Model:      s.req.Model,
		DurationMs: time.Since(start).Milliseconds(),
	}
	s.collectUsage(res)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("acp prompt: %w (stderr: %s)", err, s.stderrTail())
	}
	switch resp.StopReason {
	case acp.StopReasonEndTurn, acp.StopReasonMaxTurnRequests, "":
		return res, nil
	case acp.StopReasonCancelled:
		return res, context.Canceled
	default: // refusal, max_tokens, …
		return res, fmt.Errorf("acp prompt stopped: %s", resp.StopReason)
	}
}

// collectUsage attributes this turn's tokens by diffing the session transcript
// against the previous turn's totals. Failure is silent and lossless: Usage
// stays zero and the caller reports cost_unavailable.
func (s *acpSession) collectUsage(res *Result) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	delta, now, ok := usageDelta(string(s.id), s.lastUsage)
	if !ok {
		return
	}
	s.lastUsage = now
	usage, cost := delta.total()
	res.Usage = usage
	res.CostUSD = cost
	// Report the model the transcript actually names, which beats our own
	// request field when the runtime substituted something.
	for id := range delta {
		if id != "" {
			res.Model = id
			break
		}
	}
}

func (s *acpSession) Cancel(ctx context.Context) error {
	return s.conn.Cancel(ctx, acp.CancelNotification{SessionId: s.id})
}

func (s *acpSession) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.stdin.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.cmd.Wait()
	return nil
}

// Complete runs a single-turn session: the static engine's and planner's path.
func (a *ACP) Complete(ctx context.Context, req Request) (*Result, error) {
	sess, err := a.Open(ctx, SessionRequest{
		Kind:         req.Kind,
		SystemPrompt: req.SystemPrompt,
		Model:        req.Model,
		WorkDir:      req.WorkDir,
		AddDirs:      req.AddDirs,
		Tools:        req.Tools,
		MaxTurns:     req.MaxTurns,
		OnActivity:   req.OnActivity,
	})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.Prompt(ctx, req.Prompt)
}
