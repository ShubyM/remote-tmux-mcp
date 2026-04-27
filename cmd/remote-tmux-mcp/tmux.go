package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

type TmuxControl struct {
	Socket string
	mu     sync.Mutex
	ctrls  map[string]*Ctrl
	events chan TmuxEvent
}

type TmuxEvent struct{ Name, PaneID, Data string }

func NewTmux(s string) *TmuxControl {
	return &TmuxControl{Socket: s, ctrls: map[string]*Ctrl{}, events: make(chan TmuxEvent, 256)}
}

func (t *TmuxControl) Events() <-chan TmuxEvent { return t.events }

func (t *TmuxControl) args(a ...string) []string {
	if t.Socket == "" {
		return a
	}
	return append([]string{"-L", t.Socket}, a...)
}

func (t *TmuxControl) exec(ctx context.Context, a ...string) error {
	return exec.CommandContext(ctx, "tmux", t.args(a...)...).Run()
}

func (t *TmuxControl) Ensure(ctx context.Context, s string) error {
	if t.exec(ctx, "has-session", "-t", s) != nil {
		if err := t.exec(ctx, "new-session", "-d", "-s", s, "-n", "shell"); err != nil {
			return err
		}
	}
	return t.configureSession(ctx, s)
}

func (t *TmuxControl) configureSession(ctx context.Context, s string) error {
	cmds := [][]string{
		{"set-option", "-t", s, "base-index", "1"},
		{"set-option", "-t", s, "renumber-windows", "on"},
		{"set-window-option", "-t", s, "pane-base-index", "1"},
		{"move-window", "-r", "-t", s},
	}
	for _, cmd := range cmds {
		if err := t.exec(ctx, cmd...); err != nil {
			return err
		}
	}
	return nil
}

func (t *TmuxControl) WatchPane(ctx context.Context, s, p string) error {
	c, e := t.ctrl(ctx, s)
	if e != nil || p == "" {
		return e
	}
	_, e = c.Cmd(ctx, "refresh-client", "-A", p+":on")
	return e
}

func (t *TmuxControl) NewWindow(ctx context.Context, s, w string, cmd []string) (string, error) {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return "", e
	}
	args := append([]string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", s, "-n", w, "--"}, cmd...)
	lines, e := c.Cmd(ctx, args...)
	if e != nil {
		return "", e
	}
	pane := first(lines)
	if pane != "" {
		e = t.WatchPane(ctx, s, pane)
	}
	return pane, e
}

func (t *TmuxControl) ResolveTarget(ctx context.Context, s, target string) (string, string, error) {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return "", "", e
	}
	if target == "" {
		return "", "", errors.New("target is required")
	}
	lines, e := c.Cmd(ctx, "display-message", "-p", "-t", target, "#{pane_id}\t#{window_name}")
	if e != nil {
		return "", "", e
	}
	f := strings.SplitN(first(lines), "\t", 2)
	if len(f) != 2 || f[0] == "" {
		return "", "", fmt.Errorf("could not resolve tmux target %q", target)
	}
	return f[0], f[1], nil
}

func (t *TmuxControl) PaneExists(ctx context.Context, s, pane string) bool {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return false
	}
	lines, e := c.Cmd(ctx, "list-panes", "-a", "-F", "#{pane_id}")
	if e != nil {
		return false
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == pane {
			return true
		}
	}
	return false
}

func (t *TmuxControl) Snapshot(ctx context.Context, s string) (SessionSnapshot, error) {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return SessionSnapshot{}, e
	}
	wlines, e := c.Cmd(ctx, "list-windows", "-t", s, "-F", "#{window_id}\t#{window_index}\t#{window_name}\t#{window_active}")
	if e != nil {
		return SessionSnapshot{}, e
	}
	plines, e := c.Cmd(ctx, "list-panes", "-t", s, "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_index}\t#{pane_active}\t#{pane_current_command}\t#{pane_current_path}")
	if e != nil {
		return SessionSnapshot{}, e
	}
	snap := SessionSnapshot{Session: s}
	byWindow := map[string]int{}
	for _, line := range wlines {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		idx, _ := strconv.Atoi(f[1])
		byWindow[f[0]] = len(snap.Windows)
		snap.Windows = append(snap.Windows, WindowInfo{ID: f[0], Index: idx, Name: f[2], Active: f[3] == "1"})
	}
	for _, line := range plines {
		f := strings.SplitN(line, "\t", 6)
		if len(f) != 6 {
			continue
		}
		w, ok := byWindow[f[0]]
		if !ok {
			continue
		}
		idx, _ := strconv.Atoi(f[2])
		snap.Windows[w].Panes = append(snap.Windows[w].Panes, PaneInfo{ID: f[1], Index: idx, Active: f[3] == "1", Command: f[4], CurrentPath: f[5]})
	}
	return snap, nil
}

func (t *TmuxControl) Capture(ctx context.Context, s, target string, tail, max int) (map[string]any, error) {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return nil, e
	}
	if target == "" {
		target = s
	}
	if tail <= 0 {
		tail = 200
	}
	lines, e := c.Cmd(ctx, "capture-pane", "-p", "-J", "-S", fmt.Sprintf("-%d", tail), "-t", target)
	if e != nil {
		return nil, e
	}
	return boundText(strings.Join(lines, "\n"), max), nil
}

func (t *TmuxControl) CtrlC(ctx context.Context, s, p string) error {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return e
	}
	_, e = c.Cmd(ctx, "send-keys", "-t", p, "C-c")
	return e
}

func (t *TmuxControl) Send(ctx context.Context, s, p, text string) error {
	c, e := t.ctrl(ctx, s)
	if e != nil {
		return e
	}
	for _, part := range strings.SplitAfter(text, "\n") {
		line := strings.TrimSuffix(part, "\n")
		if line != "" {
			if _, e = c.Cmd(ctx, "send-keys", "-t", p, "-l", "--", line); e != nil {
				return e
			}
		}
		if strings.HasSuffix(part, "\n") {
			if _, e = c.Cmd(ctx, "send-keys", "-t", p, "Enter"); e != nil {
				return e
			}
		}
	}
	return nil
}

func (t *TmuxControl) ctrl(ctx context.Context, s string) (*Ctrl, error) {
	t.mu.Lock()
	if c := t.ctrls[s]; c != nil && c.alive() {
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()
	if e := t.Ensure(ctx, s); e != nil {
		return nil, e
	}
	c, e := startCtrl(ctx, t, s)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	t.ctrls[s] = c
	t.mu.Unlock()
	return c, nil
}

type Ctrl struct {
	t      *TmuxControl
	cmd    *exec.Cmd
	in     io.WriteCloser
	blocks chan ctrlBlock
	done   chan struct{}
	mu     sync.Mutex
}

type ctrlBlock struct {
	lines []string
	err   error
}

func startCtrl(ctx context.Context, t *TmuxControl, s string) (*Ctrl, error) {
	cmd := exec.CommandContext(context.Background(), "tmux", t.args("-CC", "attach", "-t", s)...)
	rw, e := pty.Start(cmd)
	if e != nil {
		return startCtrlPipe(ctx, t, s)
	}
	c := &Ctrl{t: t, cmd: cmd, in: rw, blocks: make(chan ctrlBlock, 32), done: make(chan struct{})}
	go c.read(rw)
	go func() { _ = cmd.Wait(); _ = rw.Close(); close(c.done) }()
	if e = c.ready(ctx, nil); e != nil {
		_ = rw.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return startCtrlPipe(ctx, t, s)
	}
	return c, nil
}

func startCtrlPipe(ctx context.Context, t *TmuxControl, s string) (*Ctrl, error) {
	cmd := exec.CommandContext(context.Background(), "tmux", t.args("-C", "attach", "-t", s)...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var se bytes.Buffer
	cmd.Stderr = &se
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	c := &Ctrl{t: t, cmd: cmd, in: in, blocks: make(chan ctrlBlock, 32), done: make(chan struct{})}
	go c.read(out)
	go func() { _ = cmd.Wait(); close(c.done) }()
	return c, c.ready(ctx, &se)
}

func (c *Ctrl) ready(ctx context.Context, se *bytes.Buffer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case b := <-c.blocks:
		return b.err
	case <-ctx.Done():
		if se != nil {
			return fmt.Errorf("%w: %s", ctx.Err(), strings.TrimSpace(se.String()))
		}
		return ctx.Err()
	case <-c.done:
		return errors.New("tmux control exited")
	}
}

func (c *Ctrl) Cmd(ctx context.Context, a ...string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, e := io.WriteString(c.in, join(a)+"\n"); e != nil {
		return nil, e
	}
	select {
	case b := <-c.blocks:
		return b.lines, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("tmux control exited")
	}
}

func (c *Ctrl) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *Ctrl) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "%begin ") {
			c.block(sc)
			continue
		}
		c.note(line)
	}
}

func (c *Ctrl) block(sc *bufio.Scanner) {
	var lines []string
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(l, "%end ") {
			c.blocks <- ctrlBlock{lines: lines}
			return
		}
		if strings.HasPrefix(l, "%error ") {
			c.blocks <- ctrlBlock{lines: lines, err: errors.New(strings.Join(lines, "\n"))}
			return
		}
		lines = append(lines, l)
	}
}

func (c *Ctrl) note(l string) {
	if !strings.HasPrefix(l, "%") {
		return
	}
	ev := parseEvent(l)
	if (ev.Name == "output" || ev.Name == "extended-output") && !strings.Contains(ev.Data, "[tmux] done id=") {
		return
	}
	select {
	case c.t.events <- ev:
	default:
	}
}

func parseEvent(l string) TmuxEvent {
	f := strings.Fields(l)
	if len(f) == 0 {
		return TmuxEvent{}
	}
	ev := TmuxEvent{Name: strings.TrimPrefix(f[0], "%")}
	if len(f) > 1 {
		ev.PaneID = f[1]
	}
	if ev.Name == "output" {
		p := strings.SplitN(l, " ", 3)
		if len(p) == 3 {
			ev.PaneID = p[1]
			ev.Data = unesc(p[2])
		}
	} else if ev.Name == "extended-output" {
		if len(f) > 1 {
			ev.PaneID = f[1]
		}
		if i := strings.Index(l, " : "); i >= 0 {
			ev.Data = unesc(l[i+3:])
		}
	}
	return ev
}

func unesc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, e := strconv.ParseUint(s[i+1:i+4], 8, 8); e == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func join(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		q[i] = tmuxQ(s)
	}
	return strings.Join(q, " ")
}

func tmuxQ(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) < 0 {
		return s
	}
	return shq(s)
}

func first(lines []string) string {
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			return s
		}
	}
	return ""
}
