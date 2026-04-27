package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServerToolFlow(t *testing.T) {
	if os.Getenv("TMUX_MCP_INTEGRATION") != "1" {
		t.Skip("set TMUX_MCP_INTEGRATION=1")
	}
	bin := os.Getenv("TMUX_MCP_BINARY")
	cfg := os.Getenv("TMUX_MCP_CONFIG")
	host := getenv("TMUX_MCP_TEST_HOST", "local")
	cwd := getenv("TMUX_MCP_TEST_CWD", "/tmp")
	if bin == "" || cfg == "" {
		t.Fatal("TMUX_MCP_BINARY and TMUX_MCP_CONFIG are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "mcp", "--config", cfg)
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "remote-tmux-mcp-test", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = session.Close()
		if stderr.Len() > 0 {
			t.Logf("mcp stderr:\n%s", stderr.String())
		}
	}()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(tools, "tmux_run_command") || !hasTool(tools, "tmux_remote_status") || !hasTool(tools, "tmux_session_snapshot") || !hasTool(tools, "tmux_capture_pane") {
		t.Fatalf("tools missing from MCP list: %+v", tools.Tools)
	}

	status := callTool(t, ctx, session, "tmux_remote_status", map[string]any{"host": host})
	if status["connected"] != true {
		t.Fatalf("remote status = %+v", status)
	}

	run := callTool(t, ctx, session, "tmux_run_command", map[string]any{
		"host":    host,
		"cwd":     cwd,
		"command": "printf 'mcp-ok\\n'; pwd; printf 'tmux=%s\\n' \"${TMUX:-unset}\"; printf 'tmux_pane=%s\\n' \"${TMUX_PANE:-unset}\"; printf 'agent=%s:%s:%s\\n' \"${REMOTE_TMUX_MCP:-}\" \"${REMOTE_TMUX_MCP_SESSION:-}\" \"${REMOTE_TMUX_MCP_SOCKET_NAME:-}\"",
		"name":    "mcp-smoke",
	})
	text := run["text"].(string)
	if run["status"] != "done" || int(run["exit_code"].(float64)) != 0 || !strings.Contains(text, "mcp-ok") {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(text, "tmux=unset") || !strings.Contains(text, "tmux_pane=unset") || !strings.Contains(text, "agent=1:") {
		t.Fatalf("runner environment not sanitized: %q", text)
	}
	snap := callTool(t, ctx, session, "tmux_session_snapshot", map[string]any{"host": host})
	if len(snap["windows"].([]any)) == 0 {
		t.Fatalf("empty snapshot: %+v", snap)
	}
	capture := callTool(t, ctx, session, "tmux_capture_pane", map[string]any{"host": host, "target": run["pane"], "tail_lines": 80})
	if !strings.Contains(capture["text"].(string), "mcp-ok") {
		t.Fatalf("capture = %+v", capture)
	}
	target := reusablePaneFromMap(t, snap, run["pane"].(string))
	reuse := callTool(t, ctx, session, "tmux_run_command", map[string]any{
		"host":    host,
		"cwd":     cwd,
		"target":  target,
		"command": "printf 'mcp-reuse-ok\\n'",
		"name":    "mcp-reuse",
	})
	if reuse["reused"] != true || reuse["pane"] != target || !strings.Contains(reuse["text"].(string), "mcp-reuse-ok") {
		t.Fatalf("reuse = %+v target=%s", reuse, target)
	}
}

func hasTool(tools *mcp.ListToolsResult, name string) bool {
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func callTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s returned tool error: %s", name, contentText(res.Content))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func contentText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func reusablePaneFromMap(t *testing.T, snap map[string]any, avoid string) string {
	t.Helper()
	for _, rawWindow := range snap["windows"].([]any) {
		window := rawWindow.(map[string]any)
		for _, rawPane := range window["panes"].([]any) {
			pane := rawPane.(map[string]any)
			id, _ := pane["id"].(string)
			cmd, _ := pane["command"].(string)
			if id != avoid && isShellCommand(cmd) {
				return id
			}
		}
	}
	t.Fatalf("no reusable shell pane in snapshot: %+v", snap)
	return ""
}
