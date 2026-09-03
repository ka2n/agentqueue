package main

import (
	"errors"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fakeBinEnv builds a binEnv from tables, so detection can be tested with none
// of the real agent CLIs installed.
func fakeBinEnv(paths map[string]string, outputs map[string]string, failures map[string]bool) binEnv {
	return binEnv{
		look: func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", os.ErrNotExist
		},
		run: func(name string, _ ...string) ([]byte, error) {
			if failures[name] {
				return []byte("boom"), errors.New("exit status 1")
			}
			return []byte(outputs[name]), nil
		},
	}
}

func TestDetectAgent(t *testing.T) {
	claude := detectors[0]
	tests := []struct {
		name        string
		paths       map[string]string
		outputs     map[string]string
		failures    map[string]bool
		wantFound   bool
		wantPath    string
		wantVersion string
	}{
		{
			name:        "found with a version",
			paths:       map[string]string{"claude": "/usr/local/bin/claude"},
			outputs:     map[string]string{"claude": "2.1.0 (Claude Code)\n"},
			wantFound:   true,
			wantPath:    "/usr/local/bin/claude",
			wantVersion: "2.1.0 (Claude Code)",
		},
		{
			name:      "not on PATH",
			paths:     map[string]string{},
			wantFound: false,
		},
		{
			name:        "version command fails",
			paths:       map[string]string{"claude": "/usr/local/bin/claude"},
			failures:    map[string]bool{"claude": true},
			wantFound:   true,
			wantPath:    "/usr/local/bin/claude",
			wantVersion: versionUnknown,
		},
		{
			name:        "version output is empty",
			paths:       map[string]string{"claude": "/usr/local/bin/claude"},
			outputs:     map[string]string{"claude": "\n  \n"},
			wantFound:   true,
			wantPath:    "/usr/local/bin/claude",
			wantVersion: versionUnknown,
		},
		{
			name:        "blank path counts as missing",
			paths:       map[string]string{"claude": "   "},
			wantFound:   false,
			wantVersion: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectAgent(claude, fakeBinEnv(tt.paths, tt.outputs, tt.failures))
			if got.Found != tt.wantFound {
				t.Fatalf("Found = %v, want %v", got.Found, tt.wantFound)
			}
			if got.Path != tt.wantPath {
				t.Fatalf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Version != tt.wantVersion {
				t.Fatalf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.Name != "claude" {
				t.Fatalf("Name = %q, want claude", got.Name)
			}
		})
	}
}

func TestDetectAgentsCoversEveryKnownAgent(t *testing.T) {
	env := fakeBinEnv(
		map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"},
		map[string]string{"claude": "2.1.0", "codex": "codex-cli 0.9.1"},
		nil,
	)
	infos := detectAgents(env)
	var names []string
	for _, info := range infos {
		names = append(names, info.Name)
	}
	if diff := cmp.Diff([]string{"claude", "codex", "pi"}, names); diff != "" {
		t.Fatalf("detected agents mismatch (-want +got):\n%s", diff)
	}
	if !infos[0].installable() {
		t.Fatal("claude on $PATH should be installable")
	}
	if infos[1].installable() {
		t.Fatal("codex needs no setup, so install must write nothing for it")
	}
	if infos[2].installable() {
		t.Fatal("pi has no integration yet, so install must write nothing for it")
	}
	if got := infos[2].versionLabel(); got != "-" {
		t.Fatalf("versionLabel for a missing agent = %q, want -", got)
	}
}

func TestSetupState(t *testing.T) {
	infos := detectAgents(fakeBinEnv(
		map[string]string{"claude": "/bin/claude", "codex": "/bin/codex", "pi": "/bin/pi"},
		map[string]string{"claude": "2.1.0", "codex": "0.9.1", "pi": "0.4.0"},
		nil,
	))
	tests := []struct {
		info      agentInfo
		installed bool
		want      string
	}{
		{info: infos[0], installed: false, want: "hooks not installed"},
		{info: infos[0], installed: true, want: "hooks installed"},
		{info: infos[1], want: "no setup needed"},
		{info: infos[2], want: "not supported yet"},
		{info: agentInfo{Name: "claude"}, want: "not found on $PATH"},
	}
	for _, tt := range tests {
		if got := setupState(tt.info, tt.installed); got != tt.want {
			t.Fatalf("setupState(%s, installed=%v) = %q, want %q", tt.info.Name, tt.installed, got, tt.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "2.1.0\n", want: "2.1.0"},
		{in: "2.1.0 (Claude Code)\n", want: "2.1.0 (Claude Code)"},
		{in: "\n  \n0.9.1\n", want: "0.9.1"},
		{in: "  spaced  ", want: "spaced"},
		{in: "", wantErr: true},
		{in: "\n\n", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseVersion([]byte(tt.in))
		if tt.wantErr {
			if err == nil {
				t.Fatalf("parseVersion(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFindAgent(t *testing.T) {
	infos := detectAgents(fakeBinEnv(nil, nil, nil))
	got, err := findAgent(infos, "codex")
	if err != nil {
		t.Fatalf("findAgent: %v", err)
	}
	if got.Name != "codex" {
		t.Fatalf("findAgent = %q, want codex", got.Name)
	}
	if _, err := findAgent(infos, "nope"); err == nil {
		t.Fatal("findAgent(nope) = nil error, want one naming the known agents")
	}
}
