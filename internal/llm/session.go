package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// singleShotBackend adapts a plain Backend to the Session interface by
// replaying the accumulated conversation on every turn. It exists so the
// static engine and the CLI backend can share one code path with dynamic mode;
// it deliberately does NOT support MCP servers, because there is no live
// process to attach them to — dynamic mode checks for that up front rather
// than silently running a coordinator with no tools.
type singleShotBackend struct{ be Backend }

func (s *singleShotBackend) Open(_ context.Context, req SessionRequest) (Session, error) {
	if len(req.MCPServers) > 0 {
		return nil, fmt.Errorf("backend %q cannot host MCP tools: it has no persistent session", s.be.Name())
	}
	return &singleShotSession{be: s.be, req: req}, nil
}

type singleShotSession struct {
	be  Backend
	req SessionRequest

	mu      sync.Mutex
	history []string
	closed  bool
}

func (s *singleShotSession) Prompt(ctx context.Context, text string) (*Result, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session closed")
	}
	s.history = append(s.history, text)
	prompt := strings.Join(s.history, "\n\n---\n\n")
	s.mu.Unlock()

	res, err := s.be.Complete(ctx, Request{
		Kind:         s.req.Kind,
		SystemPrompt: s.req.SystemPrompt,
		Prompt:       prompt,
		Model:        s.req.Model,
		WorkDir:      s.req.WorkDir,
		AddDirs:      s.req.AddDirs,
		Tools:        s.req.Tools,
		MaxTurns:     s.req.MaxTurns,
		OnActivity:   s.req.OnActivity,
	})
	if res != nil && res.Text != "" {
		s.mu.Lock()
		s.history = append(s.history, "(your previous reply)\n"+res.Text)
		s.mu.Unlock()
	}
	return res, err
}

func (s *singleShotSession) Cancel(context.Context) error { return nil }

func (s *singleShotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
