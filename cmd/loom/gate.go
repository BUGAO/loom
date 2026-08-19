package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// gateCmd is the hook side of loom's tool gate. Claude Code runs it as a
// PreToolUse / PostToolUse hook inside every loom session: it forwards the
// hook payload from stdin to the server's /gate endpoint with the session's
// credential (LOOM_GATE_URL / LOOM_GATE_TOKEN, inherited from the session
// process environment) and prints the server's answer, which Claude Code
// reads as its permission decision.
//
// It is fail-open on purpose: no credential, no server, a timeout, a bad
// status — the hook prints nothing and exits 0, and the tool call proceeds.
// Outside a loom session (the same workspace opened in a plain Claude Code)
// there is no credential, so the hook is inert.
func gateCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "gate",
		Short:  "hook endpoint: forward a Claude Code tool call to the loom gate (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			os.Exit(runGate(os.Stdin, os.Stdout))
		},
	}
}

func runGate(in io.Reader, out io.Writer) int {
	url, tok := os.Getenv("LOOM_GATE_URL"), os.Getenv("LOOM_GATE_TOKEN")
	if url == "" || tok == "" {
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil || len(bytes.TrimSpace(payload)) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return 0
	}
	fmt.Fprint(out, string(body))
	return 0
}
