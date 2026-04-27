package main

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Hosts    map[string]HostConfig `json:"hosts" yaml:"hosts"`
	Defaults Defaults              `json:"defaults" yaml:"defaults"`
}

type Defaults struct {
	KeepOpen        bool `json:"keep_open" yaml:"keep_open"`
	OutputTailLines int  `json:"output_tail_lines" yaml:"output_tail_lines"`
}

type HostConfig struct {
	SSHTarget       string   `json:"ssh_target" yaml:"ssh_target"`
	Local           bool     `json:"local" yaml:"local"`
	SSHOptions      []string `json:"ssh_options" yaml:"ssh_options"`
	RemoteBinary    string   `json:"remote_binary" yaml:"remote_binary"`
	BootstrapBinary string   `json:"bootstrap_binary" yaml:"bootstrap_binary"`
	DefaultSession  string   `json:"default_session" yaml:"default_session"`
	RemoteStateDir  string   `json:"remote_state_dir" yaml:"remote_state_dir"`
	TmuxSocketName  string   `json:"tmux_socket_name" yaml:"tmux_socket_name"`
	MaxOutputBytes  int      `json:"max_output_bytes" yaml:"max_output_bytes"`
}

func loadConfig(p string) (Config, error) {
	b, err := os.ReadFile(expandLocal(p))
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if len(c.Hosts) == 0 {
		return c, errors.New("config requires at least one host")
	}
	if c.Defaults.OutputTailLines == 0 {
		c.Defaults.OutputTailLines = 200
	}
	for id, h := range c.Hosts {
		if h.SSHTarget == "" && !h.Local {
			h.SSHTarget = id
		}
		if h.RemoteBinary == "" {
			if h.Local {
				h.RemoteBinary, _ = os.Executable()
			} else {
				h.RemoteBinary = "~/.local/bin/remote-tmux-mcp"
			}
		}
		if h.Local {
			h.RemoteBinary = expandLocal(h.RemoteBinary)
		}
		if h.DefaultSession == "" {
			h.DefaultSession = "agent"
		}
		if h.RemoteStateDir == "" {
			h.RemoteStateDir = "~/.cache/tmux-mcp"
		}
		if h.MaxOutputBytes == 0 {
			h.MaxOutputBytes = 65536
		}
		h.BootstrapBinary = expandLocal(h.BootstrapBinary)
		c.Hosts[id] = h
	}
	return c, nil
}

func expandLocal(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, _ := os.UserHomeDir()
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
