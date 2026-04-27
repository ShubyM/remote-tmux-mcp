package main

import (
	"encoding/json"
	"fmt"
)

type ToolParams struct {
	Host       string `json:"host,omitempty" jsonschema:"configured host id to run against"`
	ID         string `json:"id,omitempty" jsonschema:"command id returned by tmux_run_command"`
	CommandID  string `json:"command_id,omitempty" jsonschema:"command id used by the remote protocol"`
	Session    string `json:"session,omitempty" jsonschema:"tmux session name; defaults to the host default_session"`
	Target     string `json:"target,omitempty" jsonschema:"tmux target pane/window/session. On tmux_run_command, set this to reuse an idle existing pane instead of creating a new window. Prefer stable pane ids like %12 from snapshot or run results"`
	Cwd        string `json:"cwd,omitempty" jsonschema:"working directory for the command"`
	Command    string `json:"command,omitempty" jsonschema:"shell command or script text to run inside a tmux window/tab. Omit target for a fresh window; set target to reuse an existing idle pane. TMUX and TMUX_PANE are scrubbed; use REMOTE_TMUX_MCP_SOCKET_NAME and REMOTE_TMUX_MCP_SESSION when intentionally targeting the managed tmux server"`
	Name       string `json:"name,omitempty" jsonschema:"short human-readable name for the command window/tab"`
	Text       string `json:"text,omitempty" jsonschema:"literal input to send to the background command window/tab"`
	Background bool   `json:"background,omitempty" jsonschema:"set true for long-running, interactive, server, watch, REPL, or attach-style commands so the tool returns immediately with a command id"`
	KeepOpen   *bool  `json:"keep_open,omitempty" jsonschema:"keep the tmux command window/tab open after the command finishes for inspection"`
	TailLines  int    `json:"tail_lines,omitempty" jsonschema:"number of output lines to return from the end of output.log"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"maximum output bytes to return"`
}

type CommandStatus struct {
	ID         string  `json:"id,omitempty"`
	Status     string  `json:"status"`
	Reason     string  `json:"reason,omitempty"`
	ExitCode   *int    `json:"exit_code,omitempty"`
	StartedAt  *string `json:"started_at,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
}

type RemoteRequest struct {
	ID     string          `json:"id"`
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

type RemoteResponse struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RemoteError    `json:"error,omitempty"`
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RemoteEvent struct {
	Event     string `json:"event"`
	CommandID string `json:"command_id,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

type SessionSnapshot struct {
	Session string       `json:"session"`
	Windows []WindowInfo `json:"windows"`
}

type WindowInfo struct {
	ID     string     `json:"id"`
	Index  int        `json:"index"`
	Name   string     `json:"name"`
	Active bool       `json:"active"`
	Panes  []PaneInfo `json:"panes"`
}

type PaneInfo struct {
	ID          string `json:"id"`
	Index       int    `json:"index"`
	Active      bool   `json:"active"`
	Command     string `json:"command,omitempty"`
	CurrentPath string `json:"current_path,omitempty"`
}

func reqID(n uint64) string { return fmt.Sprintf("req_%d", n) }

func success(id string, v any) RemoteResponse {
	b, _ := json.Marshal(v)
	return RemoteResponse{ID: id, OK: true, Result: b}
}

func failure(id, c, m string) RemoteResponse {
	return RemoteResponse{ID: id, Error: &RemoteError{c, m}}
}

func decode[T any](r RemoteRequest) (T, error) {
	var v T
	if len(r.Params) > 0 {
		return v, json.Unmarshal(r.Params, &v)
	}
	return v, nil
}
