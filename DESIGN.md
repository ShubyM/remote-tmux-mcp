# Architecture

`remote-tmux-mcp` is a single Go binary with three modes:

```text
mcp      local MCP stdio server
remote   per-host control process, spoken to over stdio
run      per-command runner launched inside tmux
```

The main design point is that command execution is visible in tmux while command control is structured through MCP tools and newline-delimited JSON. Commands normally get their own tmux window/tab, not a split pane, but a run can target an existing idle pane when continuing the same task.

## Process Model

For a local host:

```text
MCP client
  -> remote-tmux-mcp mcp --config config.yaml
    -> remote-tmux-mcp remote --stdio
      -> tmux -L <socket> -CC attach -t <session>
        -> remote-tmux-mcp run --id <command-id> ...
          -> /bin/bash cmd.sh
```

For an SSH host:

```text
MCP client
  -> remote-tmux-mcp mcp --config config.yaml
    -> ssh -T <target> '<remote_binary> remote --stdio ...'
      -> tmux -L <socket> -CC attach -t <session>
        -> <remote_binary> run --id <command-id> ...
          -> /bin/bash cmd.sh
```

The local and SSH paths use the same `remote` protocol. SSH is only the transport used to start and connect to the remote control process.

## Responsibilities

`mcp` mode:

- runs the official Go MCP SDK server over stdio
- validates host ids against config
- starts one control process per host
- translates MCP tool calls into remote protocol requests
- keeps MCP stdout protocol-only

`remote` mode:

- speaks NDJSON over stdin/stdout
- owns the tmux control-mode connection
- creates tmux sessions and command windows
- can reuse an existing target pane for tracked follow-up commands
- configures managed sessions with 1-based window and pane indexes
- writes command state directories
- emits command events to the MCP process
- keeps stdout protocol-only

`run` mode:

- writes `status.json` as running
- executes `cmd.sh` in the requested cwd
- scrubs `TMUX` and `TMUX_PANE` from the user command environment
- exposes `REMOTE_TMUX_MCP_*` environment variables for explicit managed-session access
- tees stdout/stderr to tmux and `output.log`
- writes final `status.json` with exit code
- prints a `[tmux] done id=... exit=...` marker
- optionally execs an interactive shell to keep the command window open

## Tmux Control Mode

The control process uses:

```bash
tmux -L <socket> -CC attach -t <session>
```

If PTY startup is unavailable, it falls back to:

```bash
tmux -L <socket> -C attach -t <session>
```

Commands such as `new-window`, `send-keys`, `list-panes`, and `refresh-client -A <pane>:on` go through this persistent control-mode connection.

The remote process sets these options on managed sessions:

```text
base-index 1
pane-base-index 1
renumber-windows on
```

Tooling still stores tmux's stable pane id, such as `%12`, for control operations. Human-facing tmux indexes should start at `1`.

Command completion is event-driven:

```text
runner writes final status.json
runner prints [tmux] done id=<id> exit=<code>
tmux control mode emits %output
remote process emits command_done
MCP process wakes the foreground run call or any internal waiter
```

`status.json` remains the durable recovery source. If a client reconnects after the event, status is read from disk.

## Remote Protocol

The MCP process and the control process exchange newline-delimited JSON.

Request:

```json
{"id":"req_1","op":"status","params":{"command_id":"cmd_123"}}
```

Success response:

```json
{"id":"req_1","ok":true,"result":{"id":"cmd_123","status":"done","exit_code":0}}
```

Error response:

```json
{"id":"req_1","ok":false,"error":{"code":"operation_failed","message":"unknown command"}}
```

Event:

```json
{"event":"command_done","command_id":"cmd_123","exit_code":0}
```

Current operations:

- `hello`
- `run`
- `status`
- `output`
- `snapshot`
- `capture_pane`
- `interrupt`
- `send_input`

Foreground `tmux_run_command` calls wait internally using status checks plus remote events. Background commands return immediately and can be inspected with status and output tools.

If `tmux_run_command` receives a `target`, the remote process writes the command state files and sends the same runner invocation into that existing pane instead of creating a new window. This preserves command ids, output files, status, and exit codes while avoiding window buildup for small follow-up commands.

Session observation is exposed without a raw terminal stream:

- `snapshot` lists windows/tabs and panes with stable pane ids.
- `capture_pane` returns bounded visible pane text for a tmux target.

## State Layout

Each command writes:

```text
<remote_state_dir>/commands/<command-id>/
  cmd.sh
  meta.json
  status.json
  output.log
```

`cmd.sh` contains the exact requested command text with a small shell header:

```bash
#!/usr/bin/env bash
set -o pipefail

# user command starts below
```

The runner does not force `set -e`; multiline scripts keep normal shell semantics unless the user includes stricter options.

The runner removes `TMUX` and `TMUX_PANE` before starting the user command. This avoids a command accidentally controlling the managed tmux server when it runs plain `tmux`. The managed server is still available explicitly through:

```text
REMOTE_TMUX_MCP_SOCKET_NAME
REMOTE_TMUX_MCP_SESSION
REMOTE_TMUX_MCP_COMMAND_ID
REMOTE_TMUX_MCP_STATE_DIR
```

## Configuration Model

Important host fields:

- `local`: start the control process locally instead of over SSH
- `ssh_target`: SSH target for remote hosts
- `ssh_options`: extra SSH/SCP options
- `remote_binary`: binary path on the execution machine
- `bootstrap_binary`: local binary to upload for SSH hosts when missing
- `default_session`: default tmux session name
- `remote_state_dir`: state directory on the execution machine
- `tmux_socket_name`: optional isolated tmux socket name
- `max_output_bytes`: maximum bytes returned by output tools

For `local: true`, `remote_binary` defaults to the current executable. For SSH hosts, it defaults to `~/.local/bin/remote-tmux-mcp`.

## Invariants

- MCP stdout is protocol-only.
- Control process stdout is NDJSON protocol-only.
- Configured host ids are the command-execution boundary.
- The server does not restrict cwd paths or reject commands by pattern.
- Shell command text is written to `cmd.sh`, not nested in SSH or tmux shell quoting.
- Command launches use tmux windows/tabs or explicitly targeted existing panes, never split panes.
- Managed tmux sessions use 1-based window and pane indexes.
- User commands do not inherit `TMUX` or `TMUX_PANE`; managed tmux access is explicit through `REMOTE_TMUX_MCP_*`.
- Final `status.json` is written before the completion marker is printed.
- Output returned to MCP is bounded.
- Commands continue inside tmux if the MCP process or SSH connection exits.
- A human can attach to the same tmux session at any time.

## Current Tool Surface

- `tmux_run_command`
- `tmux_command_status`
- `tmux_command_output`
- `tmux_session_snapshot`
- `tmux_capture_pane`
- `tmux_interrupt_command`
- `tmux_send_input`
- `tmux_attach_session`
- `tmux_remote_status`
