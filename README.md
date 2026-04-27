# remote-tmux-mcp

`remote-tmux-mcp` is an MCP stdio server for running commands in tmux windows/tabs. It supports both:

- local tmux on the same machine as the MCP server
- tmux on an SSH-accessible host

The same binary has three modes:

```bash
remote-tmux-mcp mcp       # MCP stdio server
remote-tmux-mcp remote    # control process, spoken to over stdio
remote-tmux-mcp run       # per-command runner launched inside tmux
```

The MCP server starts one `remote` control process per configured host. For local hosts, it starts the process directly. For SSH hosts, it starts the process with `ssh -T`.

Terminology note: in this project, a command gets a new tmux window, which is what many terminal UIs call a tab. We avoid split panes for command launches. Managed sessions are configured with 1-based tmux window and pane indexes, so humans should see windows and panes numbered from `1`.

For agent-facing operating rules, see [AGENTS.md](AGENTS.md). For a concise usage guide, see [docs/agent-usage.md](docs/agent-usage.md).

## Requirements

- Go 1.25+
- `tmux`
- `/bin/bash` on machines that execute commands
- `ssh` and `scp` for SSH hosts

## Build

Build for the local machine:

```bash
go build -o ./remote-tmux-mcp ./cmd/remote-tmux-mcp
```

Build for a Linux amd64 remote:

```bash
GOOS=linux GOARCH=amd64 go build -o /tmp/remote-tmux-mcp-linux-amd64 ./cmd/remote-tmux-mcp
```

Build for a macOS arm64 remote:

```bash
GOOS=darwin GOARCH=arm64 go build -o /tmp/remote-tmux-mcp-darwin-arm64 ./cmd/remote-tmux-mcp
```

## Local Config

Use `local: true` when the MCP server should control tmux on the same machine.

```yaml
hosts:
  local:
    local: true
    remote_binary: /absolute/path/to/remote-tmux-mcp
    default_session: agent
    remote_state_dir: /tmp/tmux-mcp/state
    tmux_socket_name: tmux-mcp
    max_output_bytes: 65536

defaults:
  keep_open: true
  output_tail_lines: 200
```

If `remote_binary` is omitted for a local host, the server uses the current executable path.

## SSH Config

Use `ssh_target` for a remote host. The binary named by `remote_binary` must exist on the remote host, unless `bootstrap_binary` is configured.

```yaml
hosts:
  devbox:
    ssh_target: devbox
    remote_binary: ~/.local/bin/remote-tmux-mcp
    bootstrap_binary: /tmp/remote-tmux-mcp-linux-amd64
    default_session: agent
    remote_state_dir: ~/.cache/tmux-mcp
    tmux_socket_name: tmux-mcp
    max_output_bytes: 65536

defaults:
  keep_open: true
  output_tail_lines: 200
```

When `bootstrap_binary` is set, the MCP server uploads it to `remote_binary` with `scp` and runs `chmod +x` if the remote binary is missing.

## Run As An MCP Server

Run the server over stdio:

```bash
./remote-tmux-mcp mcp --config ./testdata/local.example.yaml
```

Example MCP client entry:

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/absolute/path/to/remote-tmux-mcp",
      "args": ["mcp", "--config", "/absolute/path/to/config.yaml"]
    }
  }
}
```

Do not run `remote` or `run` directly for normal use. The MCP server starts those modes as needed.

## Tools

- `tmux_run_command`: run a command; omitting `target` creates a new tmux window/tab, setting `target` reuses an existing pane
- `tmux_command_status`: read command status
- `tmux_command_output`: read bounded command output
- `tmux_session_snapshot`: list managed tmux windows/tabs and panes
- `tmux_capture_pane`: capture bounded visible text from a tmux pane/window/session target
- `tmux_interrupt_command`: send Ctrl-C to the command window/tab
- `tmux_send_input`: send literal input to the command window/tab
- `tmux_attach_session`: return the human attach command
- `tmux_remote_status`: check the control process

Typical flow:

```text
tmux_run_command
```

`tmux_run_command` returns the command id, status, exit code, and bounded output for normal foreground commands. Use `background: true` for long-running or interactive commands; then use the returned command id with status, output, interrupt, or send-input tools.

By default, a run gets a new tmux window/tab. For follow-up commands in an already-open shell, call `tmux_session_snapshot`, inspect/capture the pane, then pass that stable pane id as `target` to `tmux_run_command`. Targeted runs reuse the existing pane and still get a command id, status, exit code, and output file. When `target` is set and `keep_open` is omitted, `keep_open` defaults to `false` so the reused shell is not stacked with nested shells.

For a session-level invisible observation layer, call `tmux_session_snapshot` to get stable pane ids and then `tmux_capture_pane` to read a bounded tail from a pane. This avoids opening a raw terminal stream while still observing the remote tmux session.

## Attach

For a local host:

```bash
tmux -L tmux-mcp attach -t agent
```

For an SSH host:

```bash
ssh -t devbox 'tmux -L tmux-mcp attach -t agent'
```

The `tmux_attach_session` tool returns the correct command for the configured host.

## Command Environment

Commands run visibly inside tmux, but the runner removes `TMUX` and `TMUX_PANE` from the command environment. This keeps plain `tmux` behaving like it would in a fresh SSH shell instead of accidentally controlling the managed agent tmux server.

When a command intentionally needs to address the managed tmux server, use the explicit variables set by the runner:

```bash
tmux -L "$REMOTE_TMUX_MCP_SOCKET_NAME" list-windows -t "$REMOTE_TMUX_MCP_SESSION"
```

The runner also sets:

```text
REMOTE_TMUX_MCP=1
REMOTE_TMUX_MCP_COMMAND_ID=<command-id>
REMOTE_TMUX_MCP_STATE_DIR=<state-dir>
```

## State Files

Each command gets a state directory:

```text
<remote_state_dir>/commands/<command-id>/
  cmd.sh
  meta.json
  status.json
  output.log
```

`status.json` is the durable source of truth for command status and exit code. `output.log` stores full command output. MCP output responses return a bounded tail only.

## Tests

Unit tests:

```bash
go test ./...
go vet ./...
```

Local tmux integration:

```bash
./scripts/e2e-local.sh
```

SSH integration:

```bash
TMUX_REMOTE_INTEGRATION=1 \
TMUX_TEST_HOST=devbox \
TMUX_REMOTE_BINARY='~/.local/bin/remote-tmux-mcp' \
TMUX_BOOTSTRAP_BINARY=/tmp/remote-tmux-mcp-linux-amd64 \
go test ./cmd/remote-tmux-mcp -run TestIntegrationRemoteOverSSH -count=1 -v
```

## Notes

- MCP stdout is protocol-only; logs go to stderr.
- The `remote` control process stdout is NDJSON protocol-only; logs go to stderr.
- Command text is written to `cmd.sh`; it is not nested inside SSH/tmux shell quoting.
- Configured host ids are the security boundary. The MCP server does not restrict cwd paths or reject commands by pattern.
- Output returned through MCP tools is bounded; full logs stay on the execution host.
