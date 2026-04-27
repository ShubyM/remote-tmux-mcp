# Agent Instructions

Use the registered `tmux` MCP tools for shell work on configured hosts. Do not fall back to raw `ssh` for normal command execution unless the MCP server itself is broken.

## Operating Model

- Run commands with `tmux_run_command`.
- Omit `target` when a command deserves a new tmux window/tab; set `target` to an existing idle shell pane for small follow-up commands.
- Do not create a new window for every command. Reuse a suitable pane when continuing the same task.
- Managed tmux sessions use 1-based window and pane indexes.
- Use `background: true` for servers, watches, REPLs, attach sessions, and other long-running or interactive commands.
- Use `tmux_command_status`, `tmux_command_output`, `tmux_interrupt_command`, and `tmux_send_input` with the returned command id.
- Use `tmux_session_snapshot` and `tmux_capture_pane` as the invisible observation layer over the managed tmux session before deciding whether to reuse a pane or allocate a new window.
- Use `tmux_attach_session` to get the human attach command for a host/session.

## Remote Shells

When working on a remote machine, treat the managed tmux session as the shared execution surface:

1. Ask `tmux_attach_session` for the attach command.
2. Attach to that session to observe or interact manually.
3. Keep separate tasks in separate tmux windows/tabs, but reuse a task's existing idle shell pane for follow-up commands.
4. Avoid split panes for agent-launched commands.

## Tmux Inside Commands

The runner scrubs `TMUX` and `TMUX_PANE` from user commands so plain `tmux` behaves like it would in a fresh SSH shell. The command still runs visibly inside the managed tmux window.

For commands that intentionally need the managed tmux server, use the explicit environment variables:

```bash
tmux -L "$REMOTE_TMUX_MCP_SOCKET_NAME" list-windows -t "$REMOTE_TMUX_MCP_SESSION"
```

For commands that need the user's default tmux server, plain `tmux` is normally correct because `TMUX` has been scrubbed.

## Do Not

- Do not kill the managed tmux session from inside one of its own command windows.
- Do not use split panes for command launches.
- Do not assume window or pane index `0`; managed sessions are 1-indexed.
- Do not return unbounded logs; use bounded output tools or tail commands.
