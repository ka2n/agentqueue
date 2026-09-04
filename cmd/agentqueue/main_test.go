package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestResolveRoot(t *testing.T) {
	tests := []struct {
		name     string
		override string
		env      map[string]string
		want     string
	}{
		{
			name:     "flag wins over everything",
			override: "/queues/here",
			env:      map[string]string{"AGENTQUEUE_ROOT": "/env", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			want:     "/queues/here",
		},
		{
			name: "env var beats xdg",
			env:  map[string]string{"AGENTQUEUE_ROOT": "/env/root", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			want: "/env/root",
		},
		{
			name: "xdg state home",
			env:  map[string]string{"XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			want: filepath.Join("/xdg", "agentqueue"),
		},
		{
			name: "home fallback",
			env:  map[string]string{"HOME": "/home/u"},
			want: filepath.Join("/home/u", ".local", "state", "agentqueue"),
		},
		{
			name:     "blank flag falls through",
			override: "   ",
			env:      map[string]string{"AGENTQUEUE_ROOT": "/env/root"},
			want:     "/env/root",
		},
		{
			name: "blank env falls through to xdg",
			env:  map[string]string{"AGENTQUEUE_ROOT": "  ", "XDG_STATE_HOME": "/xdg"},
			want: filepath.Join("/xdg", "agentqueue"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRoot(tt.override, func(k string) string { return tt.env[k] })
			if err != nil {
				t.Fatalf("resolveRoot: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveRoot(%q) = %q, want %q", tt.override, got, tt.want)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "explicit agent", in: "codex:thread-1", want: "codex:thread-1"},
		{name: "bare name defaults to claude", in: "reviewer", want: "claude:reviewer"},
		{name: "trims spaces", in: "  codex:t  ", want: "codex:t"},
		{name: "missing", in: "", wantErr: true},
		{name: "blank", in: "   ", wantErr: true},
		{name: "empty agent", in: ":t", wantErr: true},
		{name: "empty name", in: "codex:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTarget(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveTarget(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTarget(%q): %v", tt.in, err)
			}
			if got.String() != tt.want {
				t.Fatalf("resolveTarget(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseMeta(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[string]string
		wantErr bool
	}{
		{name: "none", in: nil, want: nil},
		{name: "one pair", in: []string{"from=tester"}, want: map[string]string{"from": "tester"}},
		{
			name: "several pairs",
			in:   []string{"from=tester", "kind=note"},
			want: map[string]string{"from": "tester", "kind": "note"},
		},
		{name: "value keeps equals signs", in: []string{"expr=a=b"}, want: map[string]string{"expr": "a=b"}},
		{name: "empty value is allowed", in: []string{"k="}, want: map[string]string{"k": ""}},
		{name: "key is trimmed", in: []string{"  k  =v"}, want: map[string]string{"k": "v"}},
		{name: "last wins", in: []string{"k=1", "k=2"}, want: map[string]string{"k": "2"}},
		{name: "no equals", in: []string{"nope"}, wantErr: true},
		{name: "empty key", in: []string{"=v"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMeta(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMeta(%v) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMeta(%v): %v", tt.in, err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("parseMeta(%v) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestProgName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "/usr/local/bin/agentqueue", want: "agentqueue"},
		{in: "agentqueue", want: "agentqueue"},
		{in: "./aq", want: "aq"},
		{in: `C:\bin\agentqueue.exe`, want: "agentqueue"},
		{in: "", want: "agentqueue"},
		{in: ".", want: "agentqueue"},
		// A name with shell-relevant characters must not end up in a notice.
		{in: "/tmp/go-build/b001/exe/weird name", want: "agentqueue"},
		{in: "/tmp/a;rm -rf /", want: "agentqueue"},
	}
	for _, tt := range tests {
		if got := progName(tt.in); got != tt.want {
			t.Fatalf("progName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got, want := firstLine("one\ntwo"), "one ..."; got != want {
		t.Fatalf("firstLine = %q, want %q", got, want)
	}
	if got, want := firstLine("  padded  "), "padded"; got != want {
		t.Fatalf("firstLine = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 250)
	if got := firstLine(long); len(got) != 203 {
		t.Fatalf("firstLine(len 250) = %d bytes, want 203", len(got))
	}
}

// TestRunEndToEnd drives push, list, take and ack through run() against a
// temporary root, so the argument wiring and exit codes are covered.
func TestRunEndToEnd(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	var out, errOut bytes.Buffer
	code := run(ctx, []string{"push", "--root", root, "--to", "claude:reviewer", "--meta", "from=test", "hello there"}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("push exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	id := strings.TrimSpace(out.String())
	if id == "" {
		t.Fatal("push printed no item id")
	}

	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"list", "--root", root, "--to", "claude:reviewer"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("list exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) {
		t.Fatalf("list output %q does not mention %q", out.String(), id)
	}

	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"take", "--root", root, "--to", "claude:reviewer", "--next"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("take exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	for _, want := range []string{id, "hello there", "meta.from: test"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("take output %q does not contain %q", out.String(), want)
		}
	}

	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"ack", "--root", root, "--to", "claude:reviewer", id}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("ack exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "acked "+id) {
		t.Fatalf("ack output = %q", out.String())
	}
}

func TestRunPushReadsStdin(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"push", "--root", root, "--to", "claude:s"}, strings.NewReader("from stdin"), &out, &errOut)
	if code != exitOK {
		t.Fatalf("push exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}

	out.Reset()
	if code := run(context.Background(), []string{"take", "--root", root, "--to", "claude:s", "--next"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("take exit = %d", code)
	}
	if !strings.Contains(out.String(), "from stdin") {
		t.Fatalf("take output %q lost the stdin body", out.String())
	}
}

func TestRunWaitTimeoutExitCode(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"wait", "--root", t.TempDir(), "--to", "claude:nobody", "--timeout", "1"}, strings.NewReader(""), &out, &errOut)
	if code != exitTimeout {
		t.Fatalf("wait exit = %d, want %d", code, exitTimeout)
	}
	if !strings.Contains(errOut.String(), "timeout") {
		t.Fatalf("wait stderr = %q, want a timeout notice", errOut.String())
	}
}

func TestRunWaitPrintsBodies(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"push", "--root", root, "--to", "claude:s", "the full body"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("push exit = %d", code)
	}

	out.Reset()
	code := run(context.Background(), []string{"wait", "--root", root, "--to", "claude:s", "--timeout", "5"}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("wait exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// Without --take the wait output is the fetch path, so it must carry the body.
	if !strings.Contains(out.String(), "the full body") {
		t.Fatalf("wait output %q does not include the message body", out.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"frobnicate"}, strings.NewReader(""), &out, &errOut)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "unknown subcommand") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "Commands:") {
		t.Fatalf("stderr %q does not include usage", errOut.String())
	}
}

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}, {"help"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != exitOK {
			t.Fatalf("run(%v) exit = %d, want 0", args, code)
		}
		for _, want := range []string{"push", "wait", "list", "take", "ack", "AGENTQUEUE_ROOT"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("run(%v) usage does not mention %q", args, want)
			}
		}
	}
}

func TestSubcommandHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "push", args: []string{"push", "-h"}, want: []string{"Usage:", "meta", "root", "to"}},
		{name: "wait", args: []string{"wait", "-h"}, want: []string{"Usage:", "timeout", "take", "Exit 0"}},
		{name: "list", args: []string{"list", "-h"}, want: []string{"Usage:", "state", "claimed"}},
		{name: "take", args: []string{"take", "-h"}, want: []string{"Usage:", "--next", "message ID"}},
		{name: "ack", args: []string{"ack", "-h"}, want: []string{"Usage:", "claimed", "message ID"}},
		{name: "targets", args: []string{"targets", "-h"}, want: []string{"Usage:", "mailboxes", "json"}},
		{name: "sessions", args: []string{"sessions", "-h"}, want: []string{"Usage:", "SOURCE", "STATE", "/proc"}},
		{name: "register", args: []string{"register", "-h"}, want: []string{"Usage:", "session", "quiet", "json"}},
		{name: "unregister", args: []string{"unregister", "-h"}, want: []string{"Usage:", "Remove", "quiet"}},
		{name: "hook claude", args: []string{"hook", "claude", "-h"}, want: []string{"Usage:", "synchronous", "max", "no-block"}},
		{name: "hook parent", args: []string{"hook", "-h"}, want: []string{"Usage:", "Claude Code", "root"}},
		{name: "install", args: []string{"install", "-h"}, want: []string{"Usage:", "safety-checked", "skip-self-check", "settings"}},
		{name: "uninstall", args: []string{"uninstall", "-h"}, want: []string{"Usage:", "only Claude", "dry-run", "diff"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(context.Background(), tt.args, strings.NewReader(""), &out, &errOut); code != exitUsage {
				t.Fatalf("help exit = %d, want %d (stdout: %s; stderr: %s)", code, exitUsage, out.String(), errOut.String())
			}
			if errOut.Len() != 0 {
				t.Fatalf("help stderr = %q, want empty", errOut.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("help output %q does not contain %q", out.String(), want)
				}
			}
		})
	}
}

func TestRunTakeArgumentErrors(t *testing.T) {
	root := t.TempDir()
	tests := [][]string{
		{"take", "--root", root, "--to", "claude:s"},                     // neither --next nor ID
		{"take", "--root", root, "--to", "claude:s", "--next", "someid"}, // both
		{"take", "--root", root, "--to", "claude:s", "a", "b"},           // two IDs
		{"take", "--root", root, "--next"},                               // no --to
		{"ack", "--root", root, "--to", "claude:s"},                      // no ID
	}
	for _, args := range tests {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != exitError {
			t.Fatalf("run(%v) exit = %d, want %d", args, code, exitError)
		}
	}
}
