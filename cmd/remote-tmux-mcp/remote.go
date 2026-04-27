package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type CommandRecord struct {
	ID, Session, Window, Pane, Cwd, Command, RemoteDir string
	CreatedAt                                          time.Time
}

type Remote struct {
	state, bin string
	max        int
	tmux       *TmuxControl
	enc        *json.Encoder
	outMu      sync.Mutex
	mu         sync.Mutex
	idxPath    string
	idx        map[string]CommandRecord
}

func runRemote(args []string) int {
	fs := flag.NewFlagSet("remote", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stdio := fs.Bool("stdio", false, "stdio")
	state := fs.String("state-dir", "~/.cache/tmux-mcp", "state")
	sock := fs.String("tmux-socket-name", "", "socket")
	max := fs.Int("max-output-bytes", 65536, "max")
	if fs.Parse(args) != nil || !*stdio {
		return 2
	}
	st, _ := expandRemote(*state)
	bin, _ := os.Executable()
	h := newRemote(st, bin, *max, *sock)
	if err := h.serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

func newRemote(state, bin string, max int, sock string) *Remote {
	h := &Remote{state: state, bin: bin, max: max, tmux: NewTmux(sock), idx: map[string]CommandRecord{}, idxPath: filepath.Join(state, "commands", "index.json")}
	if b, e := os.ReadFile(h.idxPath); e == nil {
		_ = json.Unmarshal(b, &h.idx)
	}
	return h
}

func (h *Remote) serve(ctx context.Context, r io.Reader, w io.Writer) error {
	h.enc = json.NewEncoder(w)
	go h.tmuxEvents(ctx)
	h.watchExisting(ctx)
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1024*1024)
	for sc.Scan() {
		var req RemoteRequest
		if e := json.Unmarshal(sc.Bytes(), &req); e != nil {
			_ = h.send(failure("", "bad_json", e.Error()))
			continue
		}
		_ = h.send(h.handle(ctx, req))
	}
	return sc.Err()
}

func (h *Remote) handle(ctx context.Context, r RemoteRequest) RemoteResponse {
	var out any
	var e error
	p, e := decode[ToolParams](r)
	if e != nil {
		return failure(r.ID, "bad_params", e.Error())
	}
	switch r.Op {
	case "hello":
		var path string
		path, e = exec.LookPath("tmux")
		out = map[string]string{"name": "remote-tmux-mcp", "version": version, "tmux": path}
	case "run":
		out, e = h.run(ctx, p)
	case "status":
		out, e = h.status(ctx, cmdID(p))
	case "output":
		out, e = h.output(cmdID(p), p.TailLines, p.MaxBytes)
	case "snapshot":
		out, e = h.snapshot(ctx, p)
	case "capture_pane":
		out, e = h.capturePane(ctx, p)
	case "interrupt":
		e = h.interrupt(ctx, cmdID(p))
		out = map[string]bool{"ok": true}
	case "send_input":
		e = h.sendInput(ctx, cmdID(p), p.Text)
		out = map[string]bool{"ok": true}
	default:
		return failure(r.ID, "unknown_op", "unknown op "+r.Op)
	}
	if e != nil {
		return failure(r.ID, "operation_failed", e.Error())
	}
	return success(r.ID, out)
}

func (h *Remote) run(ctx context.Context, p ToolParams) (map[string]string, error) {
	if strings.TrimSpace(p.Command) == "" || p.Cwd == "" {
		return nil, errors.New("command and cwd are required")
	}
	id := newID()
	if p.Session == "" {
		p.Session = "agent"
	}
	win := id + "_" + safeName(p.Name)
	dir := filepath.Join(h.state, "commands", id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	rec := CommandRecord{ID: id, Session: p.Session, Window: strings.TrimRight(win, "_"), Cwd: p.Cwd, Command: p.Command, RemoteDir: dir, CreatedAt: time.Now()}
	if e := writeFiles(dir, rec); e != nil {
		return nil, e
	}
	if e := h.tmux.Ensure(ctx, p.Session); e != nil {
		return nil, e
	}
	cmd := h.runnerCommand(id, p.Session, p.Cwd, dir, p.KeepOpen != nil && *p.KeepOpen)
	if p.Target != "" {
		return h.runInTarget(ctx, p, rec, cmd)
	}
	return h.runInNewWindow(ctx, p, rec, cmd)
}

func (h *Remote) runnerCommand(id, session, cwd, dir string, keepOpen bool) []string {
	cmd := []string{h.bin, "run", "--id", id, "--session", session, "--cwd", cwd, "--state-dir", dir}
	if h.tmux.Socket != "" {
		cmd = append(cmd, "--tmux-socket-name", h.tmux.Socket)
	}
	if keepOpen {
		cmd = append(cmd, "--keep-open")
	}
	return cmd
}

func (h *Remote) runInNewWindow(ctx context.Context, p ToolParams, rec CommandRecord, cmd []string) (map[string]string, error) {
	pane, e := h.tmux.NewWindow(ctx, p.Session, rec.Window, cmd)
	if e != nil {
		return nil, e
	}
	rec.Pane = pane
	if e := writeMeta(rec.RemoteDir, rec); e != nil {
		return nil, e
	}
	if e := h.save(rec); e != nil {
		return nil, e
	}
	h.emit(RemoteEvent{Event: "command_started", CommandID: rec.ID})
	return map[string]string{"command_id": rec.ID, "session": p.Session, "window": rec.Window, "pane": pane, "status": "running", "remote_dir": rec.RemoteDir, "reused": "false"}, nil
}

func (h *Remote) runInTarget(ctx context.Context, p ToolParams, rec CommandRecord, cmd []string) (map[string]string, error) {
	pane, win, e := h.tmux.ResolveTarget(ctx, p.Session, p.Target)
	if e != nil {
		return nil, e
	}
	rec.Pane = pane
	if win != "" {
		rec.Window = win
	}
	if e := h.tmux.WatchPane(ctx, p.Session, pane); e != nil {
		return nil, e
	}
	if e := writeMeta(rec.RemoteDir, rec); e != nil {
		return nil, e
	}
	if e := h.save(rec); e != nil {
		return nil, e
	}
	if e := h.tmux.Send(ctx, p.Session, pane, shellLine(cmd)+"\n"); e != nil {
		h.drop(rec.ID)
		return nil, e
	}
	h.emit(RemoteEvent{Event: "command_started", CommandID: rec.ID})
	return map[string]string{"command_id": rec.ID, "session": p.Session, "window": rec.Window, "pane": pane, "status": "running", "remote_dir": rec.RemoteDir, "reused": "true"}, nil
}

func (h *Remote) status(ctx context.Context, id string) (CommandStatus, error) {
	rec, ok := h.get(id)
	if !ok {
		return CommandStatus{}, fmt.Errorf("unknown command %q", id)
	}
	s, e := readStatus(rec.RemoteDir)
	if e == nil {
		if s.ID == "" {
			s.ID = id
		}
		if s.Status == "done" {
			return s, nil
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return CommandStatus{}, e
	}
	if !h.tmux.PaneExists(ctx, rec.Session, rec.Pane) {
		return CommandStatus{ID: id, Status: "lost", Reason: "pane missing and no final status"}, nil
	}
	if e == nil {
		return s, nil
	}
	return CommandStatus{ID: id, Status: "running"}, nil
}

func (h *Remote) output(id string, tail, max int) (map[string]any, error) {
	rec, ok := h.get(id)
	if !ok {
		return nil, fmt.Errorf("unknown command %q", id)
	}
	if tail <= 0 {
		tail = 200
	}
	if max <= 0 || max > h.max {
		max = h.max
	}
	return bound(filepath.Join(rec.RemoteDir, "output.log"), tail, max)
}

func (h *Remote) snapshot(ctx context.Context, p ToolParams) (SessionSnapshot, error) {
	if p.Session == "" {
		p.Session = "agent"
	}
	if e := h.tmux.Ensure(ctx, p.Session); e != nil {
		return SessionSnapshot{}, e
	}
	return h.tmux.Snapshot(ctx, p.Session)
}

func (h *Remote) capturePane(ctx context.Context, p ToolParams) (map[string]any, error) {
	if p.Session == "" {
		p.Session = "agent"
	}
	if p.MaxBytes <= 0 || p.MaxBytes > h.max {
		p.MaxBytes = h.max
	}
	if e := h.tmux.Ensure(ctx, p.Session); e != nil {
		return nil, e
	}
	return h.tmux.Capture(ctx, p.Session, p.Target, p.TailLines, p.MaxBytes)
}

func (h *Remote) interrupt(ctx context.Context, id string) error {
	r, ok := h.get(id)
	if !ok {
		return fmt.Errorf("unknown command %q", id)
	}
	return h.tmux.CtrlC(ctx, r.Session, r.Pane)
}

func (h *Remote) sendInput(ctx context.Context, id, text string) error {
	r, ok := h.get(id)
	if !ok {
		return fmt.Errorf("unknown command %q", id)
	}
	return h.tmux.Send(ctx, r.Session, r.Pane, text)
}

func (h *Remote) save(r CommandRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idx[r.ID] = r
	return h.writeIndexLocked()
}

func (h *Remote) drop(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.idx, id)
	_ = h.writeIndexLocked()
}

func (h *Remote) writeIndexLocked() error {
	if e := os.MkdirAll(filepath.Dir(h.idxPath), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(h.idx, "", "  ")
	if e != nil {
		return e
	}
	if e := os.WriteFile(h.idxPath+".tmp", b, 0600); e != nil {
		return e
	}
	return os.Rename(h.idxPath+".tmp", h.idxPath)
}

func (h *Remote) get(id string) (CommandRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.idx[id]
	return r, ok
}

func (h *Remote) list() []CommandRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := make([]CommandRecord, 0, len(h.idx))
	for _, r := range h.idx {
		a = append(a, r)
	}
	return a
}

func (h *Remote) emit(ev RemoteEvent) {
	_ = h.send(ev)
}

func (h *Remote) send(v any) error {
	if h.enc == nil {
		return nil
	}
	h.outMu.Lock()
	defer h.outMu.Unlock()
	return h.enc.Encode(v)
}

func (h *Remote) watchExisting(ctx context.Context) {
	for _, r := range h.list() {
		if s, e := readStatus(r.RemoteDir); e != nil || s.Status != "done" {
			_ = h.tmux.WatchPane(ctx, r.Session, r.Pane)
		}
	}
}

var doneRe = regexp.MustCompile(`\[tmux\] done id=([^\s]+) exit=([0-9]+)`)

func (h *Remote) tmuxEvents(ctx context.Context) {
	for {
		select {
		case ev := <-h.tmux.Events():
			if ev.Name == "output" || ev.Name == "extended-output" {
				if m := doneRe.FindStringSubmatch(ev.Data); len(m) == 3 {
					if r, ok := h.get(m[1]); ok {
						if s, e := readStatus(r.RemoteDir); e == nil {
							h.done(m[1], s)
						}
					}
				}
			} else if ev.Name == "pane-exited" {
				if r, ok := h.byPane(ev.PaneID); ok {
					h.doneOrLost(ctx, r)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (h *Remote) done(id string, s CommandStatus) {
	h.emit(RemoteEvent{Event: "command_done", CommandID: id, ExitCode: s.ExitCode})
}

func (h *Remote) doneOrLost(ctx context.Context, r CommandRecord) {
	if s, e := h.status(ctx, r.ID); e == nil {
		if s.Status == "done" {
			h.done(r.ID, s)
		} else if s.Status == "lost" {
			h.emit(RemoteEvent{Event: "command_lost", CommandID: r.ID})
		}
	}
}

func (h *Remote) byPane(p string) (CommandRecord, bool) {
	for _, r := range h.list() {
		if r.Pane == p {
			return r, true
		}
	}
	return CommandRecord{}, false
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("cmd_%d_%x", time.Now().UnixMilli(), b[:])
}

func safeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "_")
	s = strings.Trim(s, "_-")
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

func cmdID(p ToolParams) string {
	if p.CommandID != "" {
		return p.CommandID
	}
	return p.ID
}

func shellLine(args []string) string {
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = shq(arg)
	}
	return strings.Join(out, " ")
}

func expandRemote(p string) (string, error) {
	if p == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(p, "~/") {
		h, e := os.UserHomeDir()
		return filepath.Join(h, p[2:]), e
	}
	return p, nil
}
