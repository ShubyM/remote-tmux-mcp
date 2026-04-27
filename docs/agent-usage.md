# Remote Tmux Agent Usage

`remote-tmux-mcp` is a structured way for an agent to run commands while keeping the execution visible in tmux.

## Mental Model

```text
agent
  -> MCP tool call
    -> configured local or SSH host
      -> managed tmux session
        -> tmux windows/tabs, with optional reuse of idle panes
```

The tmux session is the shared workspace. The MCP tools are the control plane.

## Defaults

- Commands launch in separate tmux windows/tabs unless `target` is set to reuse an existing pane.
- Windows and panes are shown with 1-based indexes.
- Agents should snapshot/capture first and avoid creating a new window for every small follow-up command.
- Foreground commands block until completion and return exit code plus bounded output.
- Long-running commands should use `background: true`.
- Full output remains in the command state directory on the execution host.

## Tool Pattern

Start a normal command:

```json
{
  "host": "devbox",
  "cwd": "/home/me/project",
  "command": "go test ./...",
  "name": "tests"
}
```

Start a server or watch:

```json
{
  "host": "devbox",
  "cwd": "/home/me/project",
  "command": "npm run dev",
  "name": "dev-server",
  "background": true
}
```

Reuse an existing idle shell pane:

```json
{
  "host": "devbox",
  "cwd": "/home/me/project",
  "target": "%12",
  "command": "git status --short",
  "name": "status"
}
```

Then inspect it with:

```text
tmux_command_status
tmux_command_output
tmux_session_snapshot
tmux_capture_pane
tmux_interrupt_command
tmux_send_input
```

`tmux_session_snapshot` returns windows/tabs and panes with stable pane ids. `tmux_capture_pane` reads a bounded tail from one of those targets. Together they provide an invisible observation layer over the remote tmux session without turning the MCP server into a raw terminal stream. Use that layer to decide whether a task already has an idle shell pane worth reusing.

## Attach

Use `tmux_attach_session` to get the exact attach command. For SSH hosts this will look like:

```bash
ssh -t devbox 'tmux -L tmux-mcp attach -t agent'
```

Humans can attach at any time. Agent-launched command windows remain visible.

## Tmux Commands From Inside Command Windows

The runner removes `TMUX` and `TMUX_PANE` from the user command environment. This prevents plain `tmux` from accidentally controlling the managed agent server just because the command is running inside a tmux pane.

The runner adds explicit variables:

```text
REMOTE_TMUX_MCP=1
REMOTE_TMUX_MCP_COMMAND_ID=<command-id>
REMOTE_TMUX_MCP_SESSION=<session>
REMOTE_TMUX_MCP_SOCKET_NAME=<socket-name, if configured>
REMOTE_TMUX_MCP_STATE_DIR=<state-dir>
```

Use these when you intentionally need the managed tmux server:

```bash
tmux -L "$REMOTE_TMUX_MCP_SOCKET_NAME" list-windows -t "$REMOTE_TMUX_MCP_SESSION"
```

Plain `tmux` is reserved for the default tmux server, matching a fresh SSH shell.
