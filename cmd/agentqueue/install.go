package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

// eventSessionEnd is where the address registration is torn down. It cannot
// deliver anything: SessionEnd hooks have no decision control and their output
// never reaches the model.
const eventSessionEnd = "SessionEnd"

// Config scopes for Claude Code settings, as accepted by --scope.
const (
	scopeUser    = "user"
	scopeProject = "project"
	scopeLocal   = "local"
)

// settingsBackupSuffix names the copy taken before the first write.
const settingsBackupSuffix = ".agentqueue.bak"

// hookEntry is one command agentqueue installs on one hook event.
type hookEntry struct {
	Event   string
	Command string
}

// claudeHookEntries lists everything the claude integration needs, where
// invocation is how the agentqueue binary is spelled in a hook command.
//
// Each delivery point closes a different hole: SessionStart drains what was
// queued while no session was running, UserPromptSubmit catches what arrived
// while the session sat idle, and Stop is the only one that can act on a
// message without the user typing anything. Delivery claims the item, so all
// three can be installed at once without double delivery.
func claudeHookEntries(invocation string) []hookEntry {
	return []hookEntry{
		{Event: eventSessionStart, Command: invocation + " register --agent claude"},
		{Event: eventSessionStart, Command: invocation + " hook claude"},
		{Event: eventUserPromptSubmit, Command: invocation + " hook claude"},
		{Event: eventStop, Command: invocation + " hook claude"},
		{Event: eventSessionEnd, Command: invocation + " unregister --agent claude"},
	}
}

// claudeEventOrder is the order events are written and printed in.
var claudeEventOrder = []string{eventSessionStart, eventUserPromptSubmit, eventStop, eventSessionEnd}

// shellQuote renders s so a shell runs it as one word. Hook commands go
// through a shell, so an install path containing a space has to be quoted -
// and the quoted form is what the settings file holds, so prefix matching on
// uninstall sees the same bytes.
func shellQuote(s string) string {
	safe := true
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-', c == '/', c == ':':
		default:
			safe = false
		}
	}
	if safe && s != "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveHookCommand decides how to spell the agentqueue binary in a hook.
//
// A hook runs with a different environment than the shell that installed it,
// so a relative path or a shell alias is useless. When the published name is
// on $PATH the bare name is best - it survives a reinstall to a new location.
// Otherwise the running binary's absolute path is written.
func resolveHookCommand(override, exePath string, look func(string) (string, error)) (string, error) {
	if s := strings.TrimSpace(override); s != "" {
		return shellQuote(s), nil
	}
	if _, err := look("agentqueue"); err == nil {
		return "agentqueue", nil
	}
	if strings.TrimSpace(exePath) == "" {
		return "", errors.New("cannot tell where the agentqueue binary is; pass --command <path>")
	}
	abs, err := filepath.Abs(exePath)
	if err != nil {
		return "", fmt.Errorf("resolve agentqueue path: %w", err)
	}
	return shellQuote(abs), nil
}

// settingsPath resolves the Claude Code settings file for a scope. An explicit
// override wins, which is what keeps tests off the real config.
func settingsPath(scope, override string, getenv func(string) string) (string, error) {
	if s := strings.TrimSpace(override); s != "" {
		return s, nil
	}
	switch strings.TrimSpace(scope) {
	case scopeUser, "":
		home := strings.TrimSpace(getenv("HOME"))
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return "", fmt.Errorf("resolve home directory for --scope user: %w", err)
			}
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	case scopeProject:
		return filepath.Join(".claude", "settings.json"), nil
	case scopeLocal:
		return filepath.Join(".claude", "settings.local.json"), nil
	default:
		return "", fmt.Errorf("unknown --scope %q: want user, project or local", scope)
	}
}

// --- settings file surgery ---

// loadSettings reads a settings file into a generic map. A missing file is an
// empty settings object, not an error.
//
// The file round-trips through encoding/json, which sorts object keys and
// drops comments, so a rewrite can reorder a hand-written file. That is why
// install confirms by default and offers --print for pasting by hand.
func loadSettings(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

// saveSettings writes the settings back, indented, after taking a one-time
// backup of whatever was there.
func saveSettings(path string, settings map[string]any) error {
	if err := backupSettings(path); err != nil {
		return err
	}
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// backupSettings copies path to path+.agentqueue.bak once, so the state before
// the first agentqueue write stays recoverable.
func backupSettings(path string) error {
	backup := path + settingsBackupSuffix
	if _, err := os.Stat(backup); err == nil {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s for backup: %w", path, err)
	}
	if err := os.WriteFile(backup, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", backup, err)
	}
	return nil
}

// hooksSection returns settings["hooks"], optionally creating it.
func hooksSection(settings map[string]any, create bool) map[string]any {
	if raw, ok := settings["hooks"]; ok {
		if m, ok := raw.(map[string]any); ok {
			return m
		}
		// Something else lives under "hooks". Leave it alone rather than
		// overwriting a shape this tool does not understand.
		return nil
	}
	if !create {
		return nil
	}
	m := map[string]any{}
	settings["hooks"] = m
	return m
}

// eventCommands lists the hook command strings configured for one event.
func eventCommands(settings map[string]any, event string) []string {
	hooks := hooksSection(settings, false)
	if hooks == nil {
		return nil
	}
	groups, _ := hooks[event].([]any)
	var out []string
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := group["hooks"].([]any)
		for _, h := range inner {
			hook, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hook["command"].(string); ok {
				out = append(out, cmd)
			}
		}
	}
	return out
}

// ownedCommand reports whether a configured hook command is one agentqueue
// wrote, and so may be removed or counted as already present.
//
// Two ways to tell: it starts with the invocation being installed now, or its
// argv[0] is an agentqueue binary. The second catches a hook installed under a
// different path than the one in use today; neither can match a hook belonging
// to some other tool, because that tool's argv[0] is its own binary.
func ownedCommand(cmd, invocation string) bool {
	cmd = strings.TrimSpace(cmd)
	if invocation != "" && strings.HasPrefix(cmd, invocation+" ") {
		return true
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	argv0 := strings.Trim(fields[0], `"'`)
	return filepath.Base(argv0) == "agentqueue"
}

// actionKey is the subcommand path of a hook command, ignoring flags: it turns
// "agentqueue hook claude --log /x" into "hook claude". Two commands with the
// same key on the same event are the same integration point, however their
// flags differ, so one of them is a duplicate.
func actionKey(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}
	var parts []string
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			break
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, " ")
}

// missingEntries returns the entries not already configured.
func missingEntries(settings map[string]any, invocation string, entries []hookEntry) []hookEntry {
	var missing []hookEntry
	for _, e := range entries {
		present := false
		for _, cmd := range eventCommands(settings, e.Event) {
			if ownedCommand(cmd, invocation) && actionKey(cmd) == actionKey(e.Command) {
				present = true
				break
			}
		}
		if !present {
			missing = append(missing, e)
		}
	}
	return missing
}

// hookBlock renders the entries in Claude Code's nested hooks shape, one
// wrapper group per event. This is both what gets merged into the settings
// file and what --print and the confirmation prompt show.
func hookBlock(entries []hookEntry) map[string]any {
	byEvent := map[string][]any{}
	for _, e := range entries {
		byEvent[e.Event] = append(byEvent[e.Event], map[string]any{
			"type":    "command",
			"command": e.Command,
		})
	}
	hooks := map[string]any{}
	for event, inner := range byEvent {
		hooks[event] = []any{map[string]any{"hooks": inner}}
	}
	return map[string]any{"hooks": hooks}
}

// mergeEntries adds the entries into settings, one wrapper group per event,
// leaving every other key and every hook it did not write untouched.
func mergeEntries(settings map[string]any, entries []hookEntry) error {
	hooks := hooksSection(settings, true)
	if hooks == nil {
		return errors.New(`the settings file has a "hooks" key that is not an object; fix it or use --print`)
	}
	byEvent := map[string][]any{}
	for _, e := range entries {
		byEvent[e.Event] = append(byEvent[e.Event], map[string]any{
			"type":    "command",
			"command": e.Command,
		})
	}
	for _, event := range claudeEventOrder {
		inner, ok := byEvent[event]
		if !ok {
			continue
		}
		groups, _ := hooks[event].([]any)
		hooks[event] = append(groups, map[string]any{"hooks": inner})
	}
	return nil
}

// removeOwnedEntries strips every agentqueue hook from settings and prunes the
// wrappers, event lists and hooks section it empties. It returns the commands
// it removed.
func removeOwnedEntries(settings map[string]any, invocation string) []string {
	hooks := hooksSection(settings, false)
	if hooks == nil {
		return nil
	}
	var removed []string
	for event, raw := range hooks {
		groups, ok := raw.([]any)
		if !ok {
			continue
		}
		keptGroups := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			inner, ok := group["hooks"].([]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			keptInner := make([]any, 0, len(inner))
			for _, h := range inner {
				hook, ok := h.(map[string]any)
				if !ok {
					keptInner = append(keptInner, h)
					continue
				}
				cmd, _ := hook["command"].(string)
				if cmd != "" && ownedCommand(cmd, invocation) {
					removed = append(removed, cmd)
					continue
				}
				keptInner = append(keptInner, h)
			}
			if len(keptInner) == 0 {
				// The wrapper existed only to hold our commands.
				continue
			}
			group["hooks"] = keptInner
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = keptGroups
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return removed
}

// --- install ---

// isTTY reports whether r is an interactive terminal. Without one there is
// nobody to answer a prompt, so install must not ask.
//
// A character device is as close as a check gets without an ioctl, and
// /dev/null is the case that has to be excluded by hand: it is a character
// device too, and it is exactly what a service manager, a cron job or a hook
// hands a process as stdin.
func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if devNull, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, devNull) {
		return false
	}
	return true
}

func cmdInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var (
		fs       = newFlagSet("install")
		agent    = fs.String("agent", "", "set up just this agent, skipping the prompt")
		scope    = fs.String("scope", scopeUser, "which config to write: user, project or local")
		settings = fs.String("settings", "", "settings file to write, overriding --scope")
		command  = fs.String("command", "", "how to spell the agentqueue binary in a hook command")
		yes      = fs.Bool("yes", false, "do not ask for confirmation")
		dryRun   = fs.Bool("dry-run", false, "print what would be added and exit without writing")
		printer  = fs.Bool("print", false, "print the hooks block to paste in by hand and exit")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue install [--agent claude] [--scope user|project|local] [--settings FILE] [--command PATH] [--yes] [--dry-run] [--print]", err)
	}

	invocation, err := resolveHookCommand(*command, currentExe(), realBinEnv().look)
	if err != nil {
		return err
	}

	if *printer {
		// Just the snippet: no detection, no file, nothing written.
		return printJSON(stdout, hookBlock(claudeHookEntries(invocation)))
	}

	infos := detectAgents(realBinEnv())
	path, err := settingsPath(*scope, *settings, os.Getenv)
	if err != nil {
		return err
	}

	chosen, err := chooseAgents(*agent, infos, path, invocation, stdin, stdout)
	if err != nil {
		return err
	}
	for _, info := range chosen {
		if err := installAgent(info, path, invocation, *yes, *dryRun, stdin, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// currentExe is the running binary's path, or "" when it cannot be found.
func currentExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// chooseAgents resolves which agents to set up: the --agent flag, or an
// interactive choice, or none.
func chooseAgents(agent string, infos []agentInfo, path, invocation string, stdin io.Reader, stdout io.Writer) ([]agentInfo, error) {
	if s := strings.TrimSpace(agent); s != "" {
		info, err := findAgent(infos, s)
		if err != nil {
			return nil, err
		}
		return []agentInfo{info}, nil
	}

	printAgentTable(stdout, infos, path, invocation)
	if !isTTY(stdin) {
		fmt.Fprintf(stdout, "\nNot a terminal, so nothing was written. Pass --agent <name> to set one up, or --print to see the hooks block.\n")
		return nil, nil
	}
	return promptForAgents(infos, stdin, stdout)
}

// printAgentTable shows what was detected and what is left to do.
func printAgentTable(stdout io.Writer, infos []agentInfo, path, invocation string) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  #\tAGENT\tVERSION\tDELIVERY\tSETUP")
	for i, info := range infos {
		installed := false
		if info.setup == setupHooks {
			if s, err := loadSettings(path); err == nil {
				installed = len(missingEntries(s, invocation, claudeHookEntries(invocation))) == 0
			}
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n", i+1, info.Name, info.versionLabel(), info.Delivery, setupState(info, installed))
	}
	_ = tw.Flush()
	fmt.Fprintf(stdout, "\nclaude hooks are written to %s\n", path)
}

// promptForAgents asks which agents to set up.
func promptForAgents(infos []agentInfo, stdin io.Reader, stdout io.Writer) ([]agentInfo, error) {
	fmt.Fprintf(stdout, "\nSet up which agent? [1-%d, a for all, q to quit] ", len(infos))
	line, err := readLine(stdin)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "q", "quit", "n", "no":
		fmt.Fprintln(stdout, "nothing written")
		return nil, nil
	case "a", "all":
		var out []agentInfo
		for _, info := range infos {
			if info.installable() {
				out = append(out, info)
			}
		}
		return out, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(infos) {
		return nil, fmt.Errorf("not a choice: %q", strings.TrimSpace(line))
	}
	return []agentInfo{infos[n-1]}, nil
}

// readLine reads one line of an answer.
func readLine(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read answer: %w", err)
	}
	return line, nil
}

// installAgent sets up one agent, or explains why there is nothing to do.
func installAgent(info agentInfo, path, invocation string, yes, dryRun bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if !info.Found {
		fmt.Fprintf(stdout, "%s: not found on $PATH, nothing to set up\n", info.Name)
		return nil
	}
	switch info.setup {
	case setupNone:
		fmt.Fprintf(stdout, "%s: already supported through `codex queue`, no setup needed\n", info.Name)
		return nil
	case setupUnsupported:
		fmt.Fprintf(stdout, "%s: not supported yet (its integration would be a JS extension, which this release does not ship); nothing written\n", info.Name)
		return nil
	}
	return installClaudeHooks(path, invocation, yes, dryRun, stdin, stdout)
}

// installClaudeHooks merges the claude hook entries into the settings file.
func installClaudeHooks(path, invocation string, yes, dryRun bool, stdin io.Reader, stdout io.Writer) error {
	settings, err := loadSettings(path)
	if err != nil {
		return err
	}
	missing := missingEntries(settings, invocation, claudeHookEntries(invocation))
	if len(missing) == 0 {
		fmt.Fprintf(stdout, "claude: hooks already installed in %s, nothing to do\n", path)
		return nil
	}

	fmt.Fprintf(stdout, "claude: adding to %s:\n", path)
	if err := printJSON(stdout, hookBlock(missing)); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(stdout, "--dry-run: nothing written")
		return nil
	}
	if !yes {
		if !isTTY(stdin) {
			return errors.New("cannot ask for confirmation without a terminal: pass --yes, --dry-run or --print")
		}
		fmt.Fprint(stdout, "Add it? [y/N] ")
		line, err := readLine(stdin)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(stdout, "nothing written")
			return nil
		}
	}
	if err := mergeEntries(settings, missing); err != nil {
		return err
	}
	// saveSettings backs up the file as it is on disk before writing, so the
	// pre-install state is what lands in the .agentqueue.bak copy.
	if err := saveSettings(path, settings); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "claude: installed %d hook(s) into %s (backup: %s)\n", len(missing), path, path+settingsBackupSuffix)
	fmt.Fprintln(stdout, "Restart or start a Claude Code session for the hooks to take effect.")
	return nil
}

// --- uninstall ---

func cmdUninstall(args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs       = newFlagSet("uninstall")
		agent    = fs.String("agent", "claude", "agent whose integration to remove")
		scope    = fs.String("scope", scopeUser, "which config to write: user, project or local")
		settings = fs.String("settings", "", "settings file to write, overriding --scope")
		command  = fs.String("command", "", "how the agentqueue binary is spelled in the installed hooks")
		yes      = fs.Bool("yes", false, "do not ask for confirmation")
		dryRun   = fs.Bool("dry-run", false, "print what would be removed and exit without writing")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue uninstall [--agent claude] [--scope user|project|local] [--settings FILE] [--yes] [--dry-run]", err)
	}
	if name := strings.TrimSpace(*agent); name != "claude" {
		fmt.Fprintf(stdout, "%s: nothing was ever written for it, nothing to remove\n", name)
		return nil
	}
	invocation, err := resolveHookCommand(*command, currentExe(), realBinEnv().look)
	if err != nil {
		return err
	}
	path, err := settingsPath(*scope, *settings, os.Getenv)
	if err != nil {
		return err
	}
	current, err := loadSettings(path)
	if err != nil {
		return err
	}
	removed := removeOwnedEntries(current, invocation)
	if len(removed) == 0 {
		fmt.Fprintf(stdout, "claude: no agentqueue hooks in %s, nothing to do\n", path)
		return nil
	}
	fmt.Fprintf(stdout, "claude: removing from %s:\n", path)
	for _, cmd := range removed {
		fmt.Fprintf(stdout, "  %s\n", cmd)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "--dry-run: nothing written")
		return nil
	}
	if !*yes {
		if !isTTY(stdin) {
			return errors.New("cannot ask for confirmation without a terminal: pass --yes or --dry-run")
		}
		fmt.Fprint(stdout, "Remove it? [y/N] ")
		line, err := readLine(stdin)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(stdout, "nothing written")
			return nil
		}
	}
	if err := saveSettings(path, current); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "claude: removed %d hook(s) from %s\n", len(removed), path)
	return nil
}
