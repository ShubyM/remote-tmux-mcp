package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

const usageHint = "Use this for shell work on configured hosts. Omit target to create a new tmux window/tab; set target to a stable pane id from tmux_session_snapshot when an idle existing shell pane should be reused. Do not create a new window for every small follow-up command. Managed sessions are 1-indexed and command launches never create split panes. Use background=true for servers, watches, REPLs, attach sessions, or interactive commands, then inspect or control them with status/output/interrupt/send_input. User commands do not inherit TMUX or TMUX_PANE; plain tmux targets the default tmux server. To control the managed agent tmux server from inside a command, use REMOTE_TMUX_MCP_SOCKET_NAME and REMOTE_TMUX_MCP_SESSION."

func main() {
	log.SetOutput(os.Stderr)
	mode, args := "mcp", os.Args[1:]
	if len(args) > 0 && (args[0] == "mcp" || args[0] == "remote" || args[0] == "run") {
		mode, args = args[0], args[1:]
	}
	code := map[string]func([]string) int{"mcp": runMCP, "remote": runRemote, "run": runRunner}[mode]
	if code == nil {
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(code(args))
}

func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "~/.config/remote-tmux-mcp/config.yaml", "config YAML")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Print(err)
		return 1
	}
	app := &App{cfg: cfg, remote: NewRemoteClient(cfg.Hosts)}
	defer app.remote.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "remote-tmux-mcp", Version: version}, nil)
	addTool(s, "tmux_run_command", usageHint+" By default this blocks until completion and returns exit code plus bounded output. With target set, the tracked runner is sent to that existing pane instead of allocating a new window.", app.run)
	addTool(s, "tmux_command_status", "Read status for a command id returned by tmux_run_command, especially for background commands. "+usageHint, app.status)
	addTool(s, "tmux_command_output", "Read bounded output for a command id returned by tmux_run_command, especially for background commands. "+usageHint, app.output)
	addTool(s, "tmux_session_snapshot", "Return a structured snapshot of the managed tmux session: windows/tabs, panes, active flags, pane ids, current commands, and paths. Use this as the invisible observation layer before capturing or sending input to a pane. "+usageHint, app.snapshot)
	addTool(s, "tmux_capture_pane", "Capture visible text from a tmux pane/window/session target. Prefer stable pane ids like %12 from tmux_session_snapshot or tmux_run_command. Returns a bounded tail, not unbounded scrollback. "+usageHint, app.capturePane)
	addTool(s, "tmux_interrupt_command", "Send Ctrl-C to a background command window/tab by command id. "+usageHint, app.interrupt)
	addTool(s, "tmux_send_input", "Send literal input to a background command window/tab by command id. Do not use this for secrets or passwords by default. "+usageHint, app.sendInput)
	addTool(s, "tmux_attach_session", "Return the human attach command for the managed tmux session. Attach there to observe or manually interact with agent-launched command windows/tabs. "+usageHint, app.attach)
	addTool(s, "tmux_remote_status", "Check that the configured host helper is reachable and report its tmux path/version. "+usageHint, app.remoteStatus)

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

func addTool[Out any](s *mcp.Server, name, desc string, h func(context.Context, ToolParams) (Out, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, p ToolParams) (*mcp.CallToolResult, Out, error) {
		out, err := h(ctx, p)
		return nil, out, err
	})
}
