package llm

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	acp "github.com/coder/acp-go-sdk"
)

// ProbeACP spawns the adapter and performs a real ACP initialize handshake —
// the doctor's deep check. File-exists tests cannot tell a working adapter
// from a broken node install or an incompatible protocol; thirty seconds of
// actually speaking ACP can. No session is opened and no model is called, so
// the probe is free.
func ProbeACP(ctx context.Context, command string) (string, error) {
	cmd := exec.Command(command)
	cmd.Env = scrubEnv("")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	defer func() {
		stdin.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	}()

	conn := acp.NewClientSideConnection(&acpClient{allow: allowedFn("")}, stdin, stdout)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	resp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: true,
		},
	})
	if err != nil {
		tail := stderr.String()
		if len(tail) > 300 {
			tail = "…" + tail[len(tail)-300:]
		}
		return "", fmt.Errorf("initialize failed: %w (stderr: %s)", err, tail)
	}
	return fmt.Sprintf("protocol v%v", resp.ProtocolVersion), nil
}
