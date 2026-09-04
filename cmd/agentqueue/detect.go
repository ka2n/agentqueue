package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// detector describes one agent this tool knows how to look for. The set is a
// table so adding an agent is a row, not a branch.
type detector struct {
	// name is the agent name, which is also the binary name and the agent
	// part of a target.
	name string
	// versionArgs asks the binary for its version.
	versionArgs []string
	// delivery is how a message reaches a running session of this agent.
	delivery string
	// setup is what this tool has to write for the agent, if anything.
	setup setupKind
}

// setupKind says what installing this agent's integration involves.
type setupKind int

const (
	// setupHooks means the agent needs hook entries written into its config.
	setupHooks setupKind = iota
	// setupNone means the agent already works with no configuration.
	setupNone
	// setupExtension means the integration is shipped as an extension that
	// this command cannot install into the agent's global configuration.
	setupExtension
)

// detectors lists the agents in the order the install table prints them.
var detectors = []detector{
	{
		name:        "claude",
		versionArgs: []string{"--version"},
		delivery:    "hooks (push at the next session boundary)",
		setup:       setupHooks,
	},
	{
		name:        "codex",
		versionArgs: []string{"--version"},
		delivery:    "codex queue (push into the running thread)",
		setup:       setupNone,
	},
	{
		name:        "pi",
		versionArgs: []string{"--version"},
		delivery:    "in-process extension (queue watcher)",
		setup:       setupExtension,
	},
}

// binEnv is how detection reaches the outside world. Both fields are injected
// so the detection tests need none of the real CLIs installed.
type binEnv struct {
	// look resolves a binary name to a path, like exec.LookPath.
	look func(name string) (string, error)
	// run executes the binary and returns its combined output.
	run func(name string, args ...string) ([]byte, error)
}

// realBinEnv is the production environment: the actual $PATH and exec.
func realBinEnv() binEnv {
	return binEnv{
		look: exec.LookPath,
		run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
	}
}

// agentInfo is what detection found out about one agent.
type agentInfo struct {
	Name     string `json:"name"`
	Found    bool   `json:"found"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Delivery string `json:"delivery"`

	setup setupKind
}

// versionUnknown is the version shown when the binary is there but will not
// say what it is. That is not a reason to refuse to set it up.
const versionUnknown = "unknown"

// parseVersion pulls a version string out of a --version output. Agents print
// anything from "2.1.0" to "2.1.0 (Claude Code)", so the first non-empty line
// is as far as this goes.
func parseVersion(out []byte) (string, error) {
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s, nil
		}
	}
	return "", errors.New("no version output")
}

// detectAgent looks for one agent and asks it for its version.
func detectAgent(d detector, env binEnv) agentInfo {
	info := agentInfo{Name: d.name, Delivery: d.delivery, setup: d.setup}
	path, err := env.look(d.name)
	if err != nil || strings.TrimSpace(path) == "" {
		return info
	}
	info.Found = true
	info.Path = path
	out, err := env.run(d.name, d.versionArgs...)
	if err != nil {
		info.Version = versionUnknown
		return info
	}
	version, err := parseVersion(out)
	if err != nil {
		info.Version = versionUnknown
		return info
	}
	info.Version = version
	return info
}

// detectAgents runs every detector.
func detectAgents(env binEnv) []agentInfo {
	out := make([]agentInfo, 0, len(detectors))
	for _, d := range detectors {
		out = append(out, detectAgent(d, env))
	}
	return out
}

// setupState renders the third column of the install table: what, if anything,
// this tool still has to do for the agent.
func setupState(info agentInfo, installed bool) string {
	if !info.Found {
		return "not found on $PATH"
	}
	switch info.setup {
	case setupNone:
		return "no setup needed"
	case setupExtension:
		return "install extension manually"
	default:
		if installed {
			return "hooks installed"
		}
		return "hooks not installed"
	}
}

// installable reports whether `agentqueue install` can write anything for the
// agent.
func (info agentInfo) installable() bool {
	return info.Found && info.setup == setupHooks
}

// versionLabel renders the version column.
func (info agentInfo) versionLabel() string {
	if !info.Found {
		return "-"
	}
	if info.Version == "" {
		return versionUnknown
	}
	return info.Version
}

// findAgent returns the detected info for one agent name.
func findAgent(infos []agentInfo, name string) (agentInfo, error) {
	for _, info := range infos {
		if info.Name == name {
			return info, nil
		}
	}
	return agentInfo{}, fmt.Errorf("unknown agent %q: known agents are %s", name, strings.Join(detectorNames(), ", "))
}

// detectorNames lists the agent names this tool knows.
func detectorNames() []string {
	names := make([]string, 0, len(detectors))
	for _, d := range detectors {
		names = append(names, d.name)
	}
	return names
}
