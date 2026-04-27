package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
)

type RemoteClient struct {
	hosts   map[string]HostConfig
	mu      sync.Mutex
	clients map[string]*remoteConnection
}

func NewRemoteClient(h map[string]HostConfig) *RemoteClient {
	return &RemoteClient{hosts: h, clients: map[string]*remoteConnection{}}
}

func (m *RemoteClient) Call(ctx context.Context, host, op string, p any, out any) error {
	c, e := m.client(ctx, host)
	if e != nil {
		return e
	}
	return c.Call(ctx, op, p, out)
}

func (m *RemoteClient) Wait(ctx context.Context, host, id string) (*CommandStatus, error) {
	c, e := m.client(ctx, host)
	if e != nil {
		return nil, e
	}
	return c.Wait(ctx, id)
}

func (m *RemoteClient) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = map[string]*remoteConnection{}
}

func (m *RemoteClient) client(ctx context.Context, id string) (*remoteConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.clients[id]; c != nil && c.alive() {
		return c, nil
	}
	h, ok := m.hosts[id]
	if !ok {
		return nil, fmt.Errorf("unknown host %q", id)
	}
	if err := ensureControlProcess(ctx, h); err != nil {
		return nil, err
	}
	c, err := startRemoteConnection(ctx, h)
	if err == nil {
		m.clients[id] = c
	}
	return c, err
}

type remoteConnection struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	enc     *json.Encoder
	done    chan struct{}
	next    atomic.Uint64
	wmu, mu sync.Mutex
	pending map[string]chan RemoteResponse
	waiters map[string][]chan RemoteEvent
	last    map[string]RemoteEvent
}

func startRemoteConnection(ctx context.Context, h HostConfig) (*remoteConnection, error) {
	var cmd *exec.Cmd
	if h.Local {
		cmd = exec.Command(h.RemoteBinary, remoteArgs(h)...)
	} else {
		args := append(append([]string{}, h.SSHOptions...), "-T", h.SSHTarget, remoteCmd(h))
		cmd = exec.Command("ssh", args...)
	}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	cmd.Stderr = os.Stderr
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	c := &remoteConnection{cmd: cmd, stdin: stdin, enc: json.NewEncoder(stdin), done: make(chan struct{}), pending: map[string]chan RemoteResponse{}, waiters: map[string][]chan RemoteEvent{}, last: map[string]RemoteEvent{}}
	go c.read(stdout)
	go func() { _ = cmd.Wait(); c.fail(); close(c.done) }()
	var hello map[string]string
	if e = c.Call(ctx, "hello", ToolParams{}, &hello); e != nil {
		_ = c.Close()
		return nil, fmt.Errorf("remote hello failed: %w", e)
	}
	return c, nil
}

func (c *remoteConnection) Call(ctx context.Context, op string, p any, out any) error {
	id := reqID(c.next.Add(1))
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	ch := make(chan RemoteResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	c.wmu.Lock()
	e = c.enc.Encode(RemoteRequest{ID: id, Op: op, Params: b})
	c.wmu.Unlock()
	if e != nil {
		return e
	}
	select {
	case r := <-ch:
		if !r.OK {
			if r.Error == nil {
				return errors.New("remote returned error")
			}
			return fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
		}
		if out == nil || len(r.Result) == 0 {
			return nil
		}
		return json.Unmarshal(r.Result, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("remote exited")
	}
}

func (c *remoteConnection) Wait(ctx context.Context, id string) (*CommandStatus, error) {
	s, e := c.status(ctx, id)
	if e != nil || terminal(s.Status) {
		return s, e
	}
	ch := c.waiter(id)
	defer c.unwait(id, ch)
	s, e = c.status(ctx, id)
	if e != nil || terminal(s.Status) {
		return s, e
	}
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return nil, errors.New("remote exited")
			}
			s, e = c.status(ctx, id)
			if e != nil || terminal(s.Status) {
				return s, e
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, errors.New("remote exited")
		}
	}
}

func (c *remoteConnection) status(ctx context.Context, id string) (*CommandStatus, error) {
	var s CommandStatus
	e := c.Call(ctx, "status", ToolParams{CommandID: id}, &s)
	if s.ID == "" {
		s.ID = id
	}
	return &s, e
}

func (c *remoteConnection) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		var ev RemoteEvent
		if json.Unmarshal(b, &ev) == nil && ev.Event != "" {
			c.event(ev)
			continue
		}
		var resp RemoteResponse
		if json.Unmarshal(b, &resp) == nil && resp.ID != "" {
			c.mu.Lock()
			ch := c.pending[resp.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}
}

func (c *remoteConnection) waiter(id string) chan RemoteEvent {
	ch := make(chan RemoteEvent, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ev, ok := c.last[id]; ok && terminalEvent(ev.Event) {
		ch <- ev
	} else {
		c.waiters[id] = append(c.waiters[id], ch)
	}
	return ch
}

func (c *remoteConnection) unwait(id string, ch chan RemoteEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.waiters[id]
	for i, w := range ws {
		if w == ch {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(c.waiters, id)
	} else {
		c.waiters[id] = ws
	}
}

func (c *remoteConnection) event(ev RemoteEvent) {
	if ev.CommandID == "" || !terminalEvent(ev.Event) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[ev.CommandID] = ev
	ws := c.waiters[ev.CommandID]
	delete(c.waiters, ev.CommandID)
	for _, ch := range ws {
		ch <- ev
	}
}

func (c *remoteConnection) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- failure(id, "remote_exited", "remote process exited")
	}
	for _, ws := range c.waiters {
		for _, ch := range ws {
			close(ch)
		}
	}
}

func (c *remoteConnection) Close() error {
	select {
	case <-c.done:
		return nil
	default:
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-c.done
		return nil
	}
}

func (c *remoteConnection) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func terminal(s string) bool      { return s == "done" || s == "lost" }
func terminalEvent(s string) bool { return s == "command_done" || s == "command_lost" }

func ensureControlProcess(ctx context.Context, h HostConfig) error {
	if h.Local {
		if _, e := exec.LookPath("tmux"); e != nil {
			return e
		}
		if st, e := os.Stat(h.RemoteBinary); e != nil || st.IsDir() || st.Mode()&0111 == 0 {
			return fmt.Errorf("local remote binary is not executable: %s", h.RemoteBinary)
		}
		return nil
	}
	check := fmt.Sprintf("if test -x %s && command -v tmux >/dev/null; then printf TMUX_REMOTE_OK; else printf TMUX_REMOTE_MISSING; fi", qpath(h.RemoteBinary))
	args := append(append([]string{}, h.SSHOptions...), h.SSHTarget, check)
	if out, e := exec.CommandContext(ctx, "ssh", args...).CombinedOutput(); e == nil && strings.Contains(string(out), "TMUX_REMOTE_OK") {
		return nil
	}
	if h.BootstrapBinary == "" {
		return fmt.Errorf("remote binary missing on %s; install with scp <binary> %s:%s", h.SSHTarget, h.SSHTarget, h.RemoteBinary)
	}
	dir := path.Dir(h.RemoteBinary)
	args = append(append([]string{}, h.SSHOptions...), h.SSHTarget, "mkdir -p "+qpath(dir))
	if out, e := exec.CommandContext(ctx, "ssh", args...).CombinedOutput(); e != nil {
		return fmt.Errorf("create remote dir: %w: %s", e, out)
	}
	args = append(append([]string{}, h.SSHOptions...), h.BootstrapBinary, h.SSHTarget+":"+h.RemoteBinary)
	if out, e := exec.CommandContext(ctx, "scp", args...).CombinedOutput(); e != nil {
		return fmt.Errorf("upload binary: %w: %s", e, out)
	}
	args = append(append([]string{}, h.SSHOptions...), h.SSHTarget, "chmod +x "+qpath(h.RemoteBinary))
	if out, e := exec.CommandContext(ctx, "ssh", args...).CombinedOutput(); e != nil {
		return fmt.Errorf("chmod binary: %w: %s", e, out)
	}
	return nil
}

func remoteCmd(h HostConfig) string {
	a := []string{qpath(h.RemoteBinary)}
	for _, v := range remoteArgs(h) {
		a = append(a, shq(v))
	}
	return strings.Join(a, " ")
}

func remoteArgs(h HostConfig) []string {
	a := []string{"remote", "--stdio", "--state-dir", h.RemoteStateDir, "--max-output-bytes", fmt.Sprint(h.MaxOutputBytes)}
	if h.TmuxSocketName != "" {
		a = append(a, "--tmux-socket-name", h.TmuxSocketName)
	}
	return a
}

func qpath(p string) string {
	if p == "~" {
		return "~"
	}
	if strings.HasPrefix(p, "~/") {
		return "~/" + shq(p[2:])
	}
	return shq(p)
}

func shq(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func attachCmd(h HostConfig, session string) string {
	t := "tmux"
	if h.TmuxSocketName != "" {
		t += " -L " + shq(h.TmuxSocketName)
	}
	cmd := t + " attach -t " + shq(session)
	if h.Local {
		return cmd
	}
	return "ssh -t " + shq(h.SSHTarget) + " " + shq(cmd)
}
