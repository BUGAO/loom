package llm

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Runtime failure classification.
//
// A session's Prompt can fail for reasons that have nothing to do with the
// work: the model provider is overloaded (529), rate-limited (429), or briefly
// down (5xx), or the adapter's connection dropped. Those are TRANSIENT — the
// right response is to wait and resend, not to fail the task and make a
// coordinator re-plan around an error the worker never caused. Everything
// below exists so the engine can tell that class apart from a real failure
// and present either one as a single readable line instead of a JSON-RPC
// envelope wrapped around a provider envelope wrapped around a stderr dump.

// PromptError is a runtime (adapter / provider) failure of one prompt turn,
// already classified and summarized. Backends return it for failures they
// recognize; the text fallbacks below cover the ones they don't.
type PromptError struct {
	Msg       string // one line, human- and model-readable
	Transient bool   // worth retrying on the same session after a pause
	Raw       error  // the underlying error, for %w chains and logs
}

func (e *PromptError) Error() string { return e.Msg }
func (e *PromptError) Unwrap() error { return e.Raw }

// IsTransient reports whether err is a transient upstream failure — a
// provider overload / rate limit / 5xx or a dropped connection — as opposed
// to a real failure of the turn (refusal, cancellation, bad request).
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var pe *PromptError
	if errors.As(err, &pe) {
		return pe.Transient
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return transientText(err.Error())
}

// CleanError renders err as one readable line: a classified PromptError's
// message, or a best-effort summary of a raw provider/adapter error.
func CleanError(err error) string {
	if err == nil {
		return ""
	}
	var pe *PromptError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	return SummarizeAPIError(err.Error())
}

var (
	// "API Error: 529 {...json...}" — how the Claude Code SDK reports a
	// provider HTTP error, and therefore what claude-code-acp relays.
	apiErrorRe = regexp.MustCompile(`API Error:\s*(\d{3})`)
	// Provider error body: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"..."}
	transientStatus = map[string]bool{"429": true, "500": true, "502": true, "503": true, "504": true, "529": true}
	transientTypes  = []string{"overloaded_error", "rate_limit_error", "api_error", "internal_server_error"}
	transientHints  = []string{
		"overloaded", "rate limit", "rate_limit", "too many requests",
		"econnreset", "econnrefused", "etimedout", "socket hang up", "fetch failed",
		"connection reset", "connection refused", "broken pipe", "unexpected eof",
		"service unavailable", "bad gateway", "gateway timeout", "internal server error",
		"server_error", "upstream connect error",
	}
)

// transientText classifies a raw error string by its provider status code,
// error type, or the familiar network-failure phrases.
func transientText(s string) bool {
	if m := apiErrorRe.FindStringSubmatch(s); m != nil {
		if transientStatus[m[1]] {
			return true
		}
		// A recognized status that is NOT transient (400, 401, 403, 404,
		// 413) is a real failure whatever the body says.
		return false
	}
	l := strings.ToLower(s)
	for _, t := range transientTypes {
		if strings.Contains(l, t) {
			return true
		}
	}
	for _, h := range transientHints {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// SummarizeAPIError turns a raw adapter error — typically a JSON-RPC error
// whose message embeds "API Error: <status> <provider json>", followed by a
// stderr tail — into one line such as
//
//	API error 529 overloaded_error: Overloaded (request_id req_…)
//
// Text it does not recognize is returned clipped to its first line.
func SummarizeAPIError(s string) string {
	m := apiErrorRe.FindStringSubmatchIndex(s)
	if m == nil {
		return firstLineClip(s, 300)
	}
	out := "API error " + s[m[2]:m[3]]
	if body := balancedJSON(s[m[1]:]); body != "" {
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal([]byte(body), &env) == nil {
			if env.Error.Type != "" {
				out += " " + env.Error.Type
			}
			if env.Error.Message != "" {
				out += ": " + env.Error.Message
			}
			if env.RequestID != "" {
				out += " (request_id " + env.RequestID + ")"
			}
		}
	}
	return out
}

// balancedJSON extracts the first brace-balanced object from s. The provider
// body usually arrives inside a JSON-RPC message string, so its quotes are
// escaped; they are unescaped first.
func balancedJSON(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// classifyPromptError wraps a raw adapter error into a PromptError, keeping
// the raw error in the chain. detail is the adapter's stderr tail, attached
// only when the error itself is not self-explanatory.
func classifyPromptError(raw error, detail string) error {
	if raw == nil {
		return nil
	}
	msg := SummarizeAPIError(raw.Error())
	transient := transientText(raw.Error())
	if detail != "" && !transient && !apiErrorRe.MatchString(raw.Error()) {
		// Unrecognized failure: the adapter's stderr is the only clue there
		// is, so it stays attached (clipped) rather than being thrown away.
		msg += " (stderr: " + lastLineClip(detail, 300) + ")"
	}
	return &PromptError{Msg: msg, Transient: transient, Raw: raw}
}

func firstLineClip(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func lastLineClip(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}
