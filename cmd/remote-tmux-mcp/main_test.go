package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultsAndProtocol(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("hosts:\n  local:\n    ssh_target: localhost\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Hosts["local"]
	if h.DefaultSession != "agent" || h.RemoteStateDir != "~/.cache/tmux-mcp" || h.RemoteBinary != "~/.local/bin/remote-tmux-mcp" || h.MaxOutputBytes != 65536 {
		t.Fatalf("defaults not applied: %+v", h)
	}
	if err := validateRun(ToolParams{Cwd: "/tmp/project", Command: "sudo whoami"}); err != nil {
		t.Fatal(err)
	}
	if err := validateRun(ToolParams{Cwd: "/tmp", Command: ""}); err == nil || !strings.Contains(err.Error(), "empty_command") {
		t.Fatalf("empty command error = %v", err)
	}
	if err := validateRun(ToolParams{Cwd: "", Command: "echo ok"}); err == nil || !strings.Contains(err.Error(), "empty_cwd") {
		t.Fatalf("empty cwd error = %v", err)
	}
	req := RemoteRequest{ID: "req_1", Op: "run"}
	req.Params, _ = json.Marshal(ToolParams{Session: "agent", Cwd: "/tmp", Command: "echo hi"})
	params, err := decode[ToolParams](req)
	if err != nil || params.Command != "echo hi" {
		t.Fatalf("decode = %+v %v", params, err)
	}
	if resp := failure("req_1", "bad", "nope"); resp.OK || resp.Error.Code != "bad" {
		t.Fatalf("failure = %+v", resp)
	}
}

func TestRunnerWritesOutputAndStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX", "/tmp/tmux-100/default,1,0")
	t.Setenv("TMUX_PANE", "%99")
	script := `#!/usr/bin/env bash
echo hello
printf 'tmux=%s\n' "${TMUX:-unset}"
printf 'tmux_pane=%s\n' "${TMUX_PANE:-unset}"
printf 'agent=%s:%s:%s:%s\n' "${REMOTE_TMUX_MCP:-}" "${REMOTE_TMUX_MCP_COMMAND_ID:-}" "${REMOTE_TMUX_MCP_SESSION:-}" "${REMOTE_TMUX_MCP_SOCKET_NAME:-}"
exit 7
`
	if err := os.WriteFile(filepath.Join(dir, "cmd.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	code, err := runnerRun(context.Background(), RunnerConfig{ID: "cmd_test", Session: "agent", Cwd: dir, StateDir: dir, SocketName: "tmux-test"})
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("code = %d", code)
	}
	out, err := os.ReadFile(filepath.Join(dir, "output.log"))
	output := string(out)
	if err != nil || !strings.Contains(output, "hello") {
		t.Fatalf("output = %q err=%v", out, err)
	}
	if !strings.Contains(output, "tmux=unset") || !strings.Contains(output, "tmux_pane=unset") {
		t.Fatalf("tmux env leaked to command: %q", output)
	}
	if !strings.Contains(output, "agent=1:cmd_test:agent:tmux-test") {
		t.Fatalf("agent env missing from command: %q", output)
	}
	status := mustStatus(t, dir)
	if status.Status != "done" || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("status = %+v", status)
	}
}

func TestIntegrationRemoteOverSSH(t *testing.T) {
	if os.Getenv("TMUX_REMOTE_INTEGRATION") != "1" {
		t.Skip("set TMUX_REMOTE_INTEGRATION=1")
	}
	target := getenv("TMUX_TEST_HOST", "localhost")
	remoteBinary := getenv("TMUX_REMOTE_BINARY", "/tmp/tmux-mcp-test/remote-tmux-mcp")
	bootstrapBinary := os.Getenv("TMUX_BOOTSTRAP_BINARY")
	_ = exec.Command("ssh", target, "tmux -L tmux-remote-test kill-server 2>/dev/null || true; rm -rf /tmp/tmux-remote-test").Run()
	cfg := Config{Hosts: map[string]HostConfig{"test": {
		SSHTarget: target, RemoteBinary: remoteBinary, BootstrapBinary: bootstrapBinary,
		DefaultSession: "agent-remote-test", RemoteStateDir: "/tmp/tmux-remote-test/state",
		TmuxSocketName: "tmux-remote-test", MaxOutputBytes: 65536,
	}}, Defaults: Defaults{KeepOpen: true, OutputTailLines: 200}}
	app := &App{cfg: cfg, remote: NewRemoteClient(cfg.Hosts)}
	defer app.remote.Close()
	ctx := context.Background()
	run, err := app.run(ctx, ToolParams{Host: "test", Cwd: "/tmp", Command: "echo remote-ok\npwd", Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if run["status"] != "done" || run["exit_code"] != 0 {
		t.Fatalf("run=%+v", run)
	}
	out, err := app.output(ctx, ToolParams{Host: "test", ID: run["id"].(string), TailLines: 40})
	if err != nil || !strings.Contains(out["text"].(string), "remote-ok") || !strings.Contains(run["text"].(string), "remote-ok") {
		t.Fatalf("run=%+v output=%+v err=%v", run, out, err)
	}
	t.Logf("attach: %s", run["attach"])
}

func TestIntegrationRemoteLocal(t *testing.T) {
	if os.Getenv("TMUX_LOCAL_INTEGRATION") != "1" {
		t.Skip("set TMUX_LOCAL_INTEGRATION=1")
	}
	remoteBinary := getenv("TMUX_REMOTE_BINARY", "/tmp/tmux-mcp-test/remote-tmux-mcp")
	_ = exec.Command("tmux", "-L", "tmux-local-test", "kill-server").Run()
	_ = os.RemoveAll("/tmp/tmux-local-test")
	cfg := Config{Hosts: map[string]HostConfig{"local": {
		Local: true, RemoteBinary: remoteBinary, DefaultSession: "agent-local-test",
		RemoteStateDir: "/tmp/tmux-local-test/state", TmuxSocketName: "tmux-local-test",
		MaxOutputBytes: 65536,
	}}, Defaults: Defaults{KeepOpen: true, OutputTailLines: 200}}
	app := &App{cfg: cfg, remote: NewRemoteClient(cfg.Hosts)}
	defer app.remote.Close()
	ctx := context.Background()
	run, err := app.run(ctx, ToolParams{Host: "local", Cwd: "/tmp", Command: "echo local-ok\npwd", Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if run["status"] != "done" || run["exit_code"] != 0 {
		t.Fatalf("run=%+v", run)
	}
	out, err := app.output(ctx, ToolParams{Host: "local", ID: run["id"].(string), TailLines: 40})
	if err != nil || !strings.Contains(out["text"].(string), "local-ok") || !strings.Contains(run["text"].(string), "local-ok") {
		t.Fatalf("run=%+v output=%+v err=%v", run, out, err)
	}
	requireOneBasedTmuxIndexes(t, "tmux-local-test", "agent-local-test")
	t.Logf("attach: %s", run["attach"])
}

func mustStatus(t *testing.T, dir string) CommandStatus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status CommandStatus
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func requireOneBasedTmuxIndexes(t *testing.T, socket, session string) {
	t.Helper()
	windows, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", session, "-F", "#{window_index}").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range strings.Fields(string(windows)) {
		if w == "0" {
			t.Fatalf("managed session has 0-based window index: %q", windows)
		}
	}
	panes, err := exec.Command("tmux", "-L", socket, "list-panes", "-a", "-F", "#{pane_index}").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(panes)) {
		if p == "0" {
			t.Fatalf("managed session has 0-based pane index: %q", panes)
		}
	}
}
