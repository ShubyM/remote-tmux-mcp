package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type RunnerConfig struct {
	ID, Session, Cwd, StateDir, SocketName string
	KeepOpen                               bool
}

func runRunner(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var c RunnerConfig
	fs.StringVar(&c.ID, "id", "", "id")
	fs.StringVar(&c.Session, "session", "", "session")
	fs.StringVar(&c.Cwd, "cwd", "", "cwd")
	fs.StringVar(&c.StateDir, "state-dir", "", "state")
	fs.StringVar(&c.SocketName, "tmux-socket-name", "", "tmux socket")
	fs.BoolVar(&c.KeepOpen, "keep-open", false, "keep open")
	if fs.Parse(args) != nil {
		return 2
	}
	code, e := runnerRun(context.Background(), c)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	return code
}

func runnerRun(ctx context.Context, c RunnerConfig) (int, error) {
	if c.ID == "" || c.Cwd == "" || c.StateDir == "" {
		return 1, errors.New("--id, --cwd, and --state-dir are required")
	}
	out, e := os.OpenFile(filepath.Join(c.StateDir, "output.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return 1, e
	}
	defer out.Close()
	start := now()
	_ = writeStatus(c.StateDir, CommandStatus{ID: c.ID, Status: "running", StartedAt: &start})
	cmd := exec.CommandContext(ctx, "/bin/bash", filepath.Join(c.StateDir, "cmd.sh"))
	cmd.Dir = c.Cwd
	cmd.Env = commandEnv(os.Environ(), c)
	cmd.Stdout = io.MultiWriter(os.Stdout, out)
	cmd.Stderr = io.MultiWriter(os.Stderr, out)
	if e = cmd.Start(); e != nil {
		return 127, finish(c, 127, start, e)
	}
	sig := forward(cmd)
	e = cmd.Wait()
	stopSignals(sig)
	code := exitCode(e)
	if e = finish(c, code, start, e); e != nil {
		return code, e
	}
	if c.KeepOpen {
		fmt.Printf("\n[tmux] command finished with exit code %d\n[tmux] window kept open for inspection\n", code)
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/bash"
		}
		_ = syscall.Exec(sh, []string{sh, "-i"}, commandEnv(os.Environ(), c))
	}
	return code, nil
}

func commandEnv(env []string, c RunnerConfig) []string {
	out := make([]string, 0, len(env)+5)
	for _, v := range env {
		if strings.HasPrefix(v, "TMUX=") || strings.HasPrefix(v, "TMUX_PANE=") {
			continue
		}
		out = append(out, v)
	}
	out = append(out,
		"REMOTE_TMUX_MCP=1",
		"REMOTE_TMUX_MCP_COMMAND_ID="+c.ID,
		"REMOTE_TMUX_MCP_SESSION="+c.Session,
		"REMOTE_TMUX_MCP_STATE_DIR="+c.StateDir,
	)
	if c.SocketName != "" {
		out = append(out, "REMOTE_TMUX_MCP_SOCKET_NAME="+c.SocketName)
	}
	return out
}

func finish(c RunnerConfig, code int, start string, cause error) error {
	if cause != nil {
		fmt.Fprintln(os.Stderr, cause)
	}
	end := now()
	s := CommandStatus{ID: c.ID, Status: "done", ExitCode: &code, StartedAt: &start, FinishedAt: &end}
	if cause != nil {
		s.Reason = cause.Error()
	}
	if e := writeStatus(c.StateDir, s); e != nil {
		return e
	}
	fmt.Printf("\n[tmux] done id=%s exit=%d\n", c.ID, code)
	return nil
}

func forward(cmd *exec.Cmd) chan os.Signal {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for s := range ch {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	return ch
}

func stopSignals(ch chan os.Signal) { signal.Stop(ch); close(ch) }

func exitCode(e error) int {
	if e == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(e, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok {
			if st.Exited() {
				return st.ExitStatus()
			}
			if st.Signaled() {
				return 128 + int(st.Signal())
			}
		}
		return ee.ExitCode()
	}
	return 1
}

func writeStatus(dir string, s CommandStatus) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	tmp := filepath.Join(dir, "status.json.tmp")
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, filepath.Join(dir, "status.json"))
}

func readStatus(dir string) (CommandStatus, error) {
	var s CommandStatus
	b, e := os.ReadFile(filepath.Join(dir, "status.json"))
	if e == nil {
		e = json.Unmarshal(b, &s)
	}
	return s, e
}

func bound(p string, tail, max int) (map[string]any, error) {
	b, e := os.ReadFile(p)
	if errors.Is(e, os.ErrNotExist) {
		return map[string]any{"text": "", "truncated": false}, nil
	}
	if e != nil {
		return nil, e
	}
	text := string(b)
	if tail > 0 {
		ls := strings.SplitAfter(text, "\n")
		if len(ls) > tail {
			text = strings.Join(ls[len(ls)-tail:], "")
		}
	}
	tr := false
	if len(text) > max {
		return boundText(text, max), nil
	}
	return map[string]any{"text": text, "truncated": tr}, nil
}

func boundText(text string, max int) map[string]any {
	tr := false
	if max > 0 && len(text) > max {
		tr = true
		text = text[len(text)-max:]
	}
	return map[string]any{"text": text, "truncated": tr}
}

func writeFiles(dir string, r CommandRecord) error {
	body := "#!/usr/bin/env bash\nset -o pipefail\n\n# user command starts below\n" + r.Command + "\n"
	if e := os.WriteFile(filepath.Join(dir, "cmd.sh"), []byte(body), 0700); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0600)
}

func now() string { return time.Now().Format(time.RFC3339) }
