package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loom/internal/hub"
	"loom/internal/llm"
	"loom/internal/model"
)

// ---- the listener ----
//
// Every user message into a live run is classified by a tiny, tool-less model
// call: is this a NEW TASK (the main agent must re-assess before it touches
// anything), a continuation of the current work, a question, or a meta
// remark? Only "task" has a consequence — it makes an assessment pending, so
// the gate refuses writes until assess_task is filed. The classification runs
// CONCURRENTLY with delivery (the message reaches the main agent at once);
// a failure just means no consequence, and three failures in a row are
// surfaced to the user (RunSession.ListenerResult).

const (
	listenTask         = "task"
	listenContinuation = "continuation"
	listenQuestion     = "question"
	listenMeta         = "meta"
)

const listenerModel = "claude-haiku-4-5"

const listenerSystemPrompt = `You classify ONE message a user just sent into a running software-agent session. Answer with exactly
one word and nothing else:
- task: the user is bringing a NEW piece of work — a feature, a program, a document, a change with its own
  goal — distinct from what is already in progress (or the first goal of the session).
- continuation: steering, corrections, additions or follow-ups to the work already in progress
  ("no, use X", "also add a button", "that file too").
- question: the user asks something and expects an answer, not work.
- meta: remarks about the session itself, thanks, small talk, process comments.
When in doubt between task and continuation, prefer continuation.`

// messageMarkers fence the user's text so the classifier (and the mock) read
// exactly the message and not the instructions around it.
const (
	msgOpen  = "### MESSAGE\n"
	msgClose = "\n### END"
)

// classify asks the listener model what a message is. recent is the tail of
// the conversation, for context. It never blocks a caller for long: 45s cap.
func (e *Engine) classify(ctx context.Context, dryRun bool, recent []model.ChatMessage, text string) (string, error) {
	backend, err := e.runtimeFor("", dryRun)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(recent) > 0 {
		b.WriteString("Recent conversation (oldest first):\n")
		for _, m := range recent {
			t := m.Text
			if len(t) > 400 {
				t = t[:400] + "…"
			}
			fmt.Fprintf(&b, "- %s: %s\n", m.From, strings.ReplaceAll(t, "\n", " "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Classify this message:\n" + msgOpen + text + msgClose + "\n\nOne word: task | continuation | question | meta")
	dir := filepath.Join(e.store.Dir(), "listener")
	os.MkdirAll(dir, 0o755)
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	res, err := backend.Complete(cctx, llm.Request{
		Kind: llm.KindListen, SystemPrompt: listenerSystemPrompt, Prompt: b.String(),
		Model: listenerModel, WorkDir: dir, Tools: "", MaxTurns: 1,
	})
	if err != nil {
		return "", err
	}
	return parseListenKind(res.Text)
}

func parseListenKind(text string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, k := range []string{listenTask, listenContinuation, listenQuestion, listenMeta} {
		if strings.HasPrefix(t, k) {
			return k, nil
		}
	}
	// A chatty model: take the first recognized word anywhere.
	for _, k := range []string{listenContinuation, listenQuestion, listenMeta, listenTask} {
		if strings.Contains(t, k) {
			return k, nil
		}
	}
	return "", fmt.Errorf("unrecognized classification %q", firstLineOf(text))
}

// listen classifies one user message in the background and applies the
// consequence: a new task makes an assessment pending. recent is captured
// before the message was appended.
func (e *Engine) listen(ctx context.Context, rs *hub.RunSession, dryRun bool, recent []model.ChatMessage, text string) {
	go func() {
		kind, err := e.classify(ctx, dryRun, recent, text)
		rs.ListenerResult(kind, err)
		if err == nil && kind == listenTask {
			rs.RequireAssessment("the user brought a new task: " + firstLineOf(text))
		}
	}()
}
