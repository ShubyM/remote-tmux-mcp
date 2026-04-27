package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type App struct {
	cfg    Config
	remote *RemoteClient
}

func (a *App) host(id string) (HostConfig, error) {
	h, ok := a.cfg.Hosts[id]
	if !ok {
		return h, fmt.Errorf("unknown host %q", id)
	}
	return h, nil
}

func (a *App) run(ctx context.Context, p ToolParams) (map[string]any, error) {
	h, err := a.host(p.Host)
	if err != nil {
		return nil, err
	}
	if err := validateRun(p); err != nil {
		return nil, err
	}
	session, keep := p.Session, a.cfg.Defaults.KeepOpen
	if session == "" {
		session = h.DefaultSession
	}
	if p.KeepOpen != nil {
		keep = *p.KeepOpen
	} else if p.Target != "" {
		keep = false
	}
	var rr map[string]string
	if err := a.remote.Call(ctx, p.Host, "run", ToolParams{Session: session, Target: p.Target, Cwd: p.Cwd, Command: p.Command, Name: p.Name, KeepOpen: &keep}, &rr); err != nil {
		return nil, err
	}
	id := rr["command_id"]
	reused := rr["reused"] == "true"
	res := map[string]any{"id": id, "host": p.Host, "session": rr["session"], "window": rr["window"], "pane": rr["pane"], "status": rr["status"], "reused": reused, "attach": attachCmd(h, rr["session"])}
	if reused {
		res["mode"] = "reused_pane"
	} else {
		res["mode"] = "new_window"
	}
	if p.Background {
		return res, nil
	}
	status, err := a.remote.Wait(ctx, p.Host, id)
	if err != nil {
		return res, err
	}
	addStatus(res, status)
	out, err := a.output(ctx, ToolParams{Host: p.Host, ID: id, TailLines: p.TailLines, MaxBytes: p.MaxBytes})
	if err != nil {
		return res, err
	}
	res["text"] = out["text"]
	res["truncated"] = out["truncated"]
	return res, nil
}

func (a *App) status(ctx context.Context, p ToolParams) (*CommandStatus, error) {
	var s CommandStatus
	err := a.remote.Call(ctx, p.Host, "status", ToolParams{CommandID: p.ID}, &s)
	if s.ID == "" {
		s.ID = p.ID
	}
	return &s, err
}

func (a *App) output(ctx context.Context, p ToolParams) (map[string]any, error) {
	h, err := a.host(p.Host)
	if err != nil {
		return nil, err
	}
	if p.TailLines == 0 {
		p.TailLines = a.cfg.Defaults.OutputTailLines
	}
	if p.MaxBytes == 0 || p.MaxBytes > h.MaxOutputBytes {
		p.MaxBytes = h.MaxOutputBytes
	}
	var out map[string]any
	err = a.remote.Call(ctx, p.Host, "output", ToolParams{CommandID: p.ID, TailLines: p.TailLines, MaxBytes: p.MaxBytes}, &out)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "text": out["text"], "truncated": out["truncated"]}, err
}

func (a *App) snapshot(ctx context.Context, p ToolParams) (*SessionSnapshot, error) {
	h, err := a.host(p.Host)
	if err != nil {
		return nil, err
	}
	if p.Session == "" {
		p.Session = h.DefaultSession
	}
	var out SessionSnapshot
	err = a.remote.Call(ctx, p.Host, "snapshot", ToolParams{Session: p.Session}, &out)
	return &out, err
}

func (a *App) capturePane(ctx context.Context, p ToolParams) (map[string]any, error) {
	h, err := a.host(p.Host)
	if err != nil {
		return nil, err
	}
	if p.Session == "" {
		p.Session = h.DefaultSession
	}
	if p.TailLines == 0 {
		p.TailLines = a.cfg.Defaults.OutputTailLines
	}
	if p.MaxBytes == 0 || p.MaxBytes > h.MaxOutputBytes {
		p.MaxBytes = h.MaxOutputBytes
	}
	target := p.Target
	if target == "" {
		target = p.Session
	}
	var out map[string]any
	err = a.remote.Call(ctx, p.Host, "capture_pane", ToolParams{Session: p.Session, Target: p.Target, TailLines: p.TailLines, MaxBytes: p.MaxBytes}, &out)
	if err != nil {
		return nil, err
	}
	return map[string]any{"session": p.Session, "target": target, "text": out["text"], "truncated": out["truncated"]}, err
}

func (a *App) interrupt(ctx context.Context, p ToolParams) (map[string]bool, error) {
	e := a.remote.Call(ctx, p.Host, "interrupt", ToolParams{CommandID: p.ID}, nil)
	return map[string]bool{"ok": e == nil}, e
}

func (a *App) sendInput(ctx context.Context, p ToolParams) (map[string]bool, error) {
	e := a.remote.Call(ctx, p.Host, "send_input", ToolParams{CommandID: p.ID, Text: p.Text}, nil)
	return map[string]bool{"ok": e == nil}, e
}

func (a *App) attach(ctx context.Context, p ToolParams) (map[string]string, error) {
	_ = ctx
	h, e := a.host(p.Host)
	if e != nil {
		return nil, e
	}
	if p.Session == "" {
		p.Session = h.DefaultSession
	}
	return map[string]string{"attach": attachCmd(h, p.Session)}, nil
}

func (a *App) remoteStatus(ctx context.Context, p ToolParams) (map[string]any, error) {
	h, e := a.host(p.Host)
	if e != nil {
		return nil, e
	}
	var hello map[string]string
	e = a.remote.Call(ctx, p.Host, "hello", ToolParams{}, &hello)
	if e != nil {
		return map[string]any{"connected": false, "reason": e.Error(), "remote_binary": h.RemoteBinary}, nil
	}
	return map[string]any{"connected": e == nil, "version": hello["version"], "remote_binary": h.RemoteBinary, "tmux": hello["tmux"]}, e
}

func validateRun(p ToolParams) error {
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("empty_command: command is required")
	}
	if strings.TrimSpace(p.Cwd) == "" {
		return errors.New("empty_cwd: cwd is required")
	}
	return nil
}

func addStatus(m map[string]any, s *CommandStatus) {
	m["status"] = s.Status
	if s.Reason != "" {
		m["reason"] = s.Reason
	}
	if s.ExitCode != nil {
		m["exit_code"] = *s.ExitCode
	}
	if s.StartedAt != nil {
		m["started_at"] = *s.StartedAt
	}
	if s.FinishedAt != nil {
		m["finished_at"] = *s.FinishedAt
	}
}
