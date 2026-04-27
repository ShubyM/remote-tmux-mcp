#!/usr/bin/env bash
set -euo pipefail

mkdir -p /tmp/tmux-mcp-test
go build -o /tmp/tmux-mcp-test/remote-tmux-mcp ./cmd/remote-tmux-mcp
chmod +x /tmp/tmux-mcp-test/remote-tmux-mcp

export TMUX_LOCAL_INTEGRATION=1
export TMUX_REMOTE_BINARY="${TMUX_REMOTE_BINARY:-/tmp/tmux-mcp-test/remote-tmux-mcp}"

go test ./cmd/remote-tmux-mcp -run TestIntegrationRemoteLocal -count=1 -v

echo "Attach manually with:"
echo "tmux -L tmux-local-test attach -t agent-local-test"
