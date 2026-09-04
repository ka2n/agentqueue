package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
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
// These entries are deliberately synchronous: they carry no "async": true,
// and they never may. Our stdout is the protocol - the delivered message
// travels in hookSpecificOutput.additionalContext, and the Stop hook's
// decision is what keeps the turn alive - and Claude Code reads neither from
// an async hook. Marking these async would silently deliver nothing, with no
// error to notice. That is also why `hook claude` has to stay fast: it scans
// a directory and renames a file, with no network call and no transcript
// read, because Claude Code waits for it.
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

// defaultSettingsMode is the mode a settings file gets when we are the ones
// creating it. An existing file keeps its own mode.
const defaultSettingsMode = os.FileMode(0o644)

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
	return writeFileAtomic(path, append(body, '\n'), settingsMode(path))
}

// settingsMode is the mode to write path with: its own, if it exists. A
// settings file the user chmodded to 0600 must not come back world-readable
// because we rewrote it.
func settingsMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode().Perm(); mode != 0 {
			return mode
		}
	}
	return defaultSettingsMode
}

// writeFileAtomic writes body to path through a temp file in the same
// directory and one rename.
//
// The point is what happens when the write does not finish: an interrupt, a
// full disk or a crash part-way through leaves the temp file behind and the
// user's settings.json exactly as it was. Writing in place would truncate it
// first and leave half a config, which for this file means a Claude Code that
// will not start. The temp file has to be a sibling, because rename is only
// atomic within a filesystem.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".agentqueue-*")
	if err != nil {
		return fmt.Errorf("create a temporary file next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Every failure from here on has to take the temp file with it.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// CreateTemp makes the file 0600, which is not necessarily what the
	// settings file had.
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set the mode on %s: %w", tmpName, err)
	}
	// Flush before the rename, so a crash cannot leave the renamed file
	// holding nothing.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("move %s into place at %s: %w", tmpName, path, err)
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
	// The backup is a copy of the settings file, so it gets the same mode:
	// a 0600 config must not be backed up world-readable.
	return writeFileAtomic(backup, body, settingsMode(path))
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

// splitArgv0 splits a hook command into the binary it runs and the rest of
// the command line. It understands the single-quoted form shellQuote writes,
// so a path with a space in it comes back in one piece.
func splitArgv0(cmd string) (argv0, rest string) {
	s := strings.TrimLeft(cmd, " \t")
	if strings.HasPrefix(s, "'") {
		if end := strings.Index(s[1:], "'"); end >= 0 {
			return s[:end+2], s[end+2:]
		}
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// rewriteArgv0 points a command at a different binary, keeping its arguments.
// It is how a hand-edited flag survives an update: only argv[0] moves.
func rewriteArgv0(cmd, invocation string) string {
	_, rest := splitArgv0(cmd)
	return invocation + rest
}

// stalePathEntries lists the agentqueue hook commands that run a different
// binary than the one being installed now.
//
// ownedCommand recognises an entry as ours by its argv[0] basename, whatever
// path it sits at, and missingEntries then keys on the subcommand - so a hook
// left over from a binary that has since moved reads as "already installed"
// and would be left pointing at a path that no longer exists. Finding those
// is what turns an install into an update.
func stalePathEntries(settings map[string]any, invocation string) []string {
	hooks := hooksSection(settings, false)
	if hooks == nil {
		return nil
	}
	var stale []string
	for _, event := range sortedKeys(hooks) {
		for _, cmd := range eventCommands(settings, event) {
			if !ownedCommand(cmd, invocation) {
				continue
			}
			if argv0, _ := splitArgv0(strings.TrimSpace(cmd)); argv0 != invocation {
				stale = append(stale, cmd)
			}
		}
	}
	return stale
}

// staleCommandSummary names the binary the stale entries were running, for
// the "command changed from X to Y" line. Normally they all ran the same one;
// if a config somehow collected several, all of them are named rather than
// picking one to report.
func staleCommandSummary(stale []string) string {
	var seen []string
	for _, cmd := range stale {
		argv0, _ := splitArgv0(strings.TrimSpace(cmd))
		if !slices.Contains(seen, argv0) {
			seen = append(seen, argv0)
		}
	}
	if len(seen) == 0 {
		return "an older command"
	}
	return strings.Join(seen, ", ")
}

// asyncOwnedEntries lists agentqueue hook entries that were marked
// "async": true by hand.
//
// An async hook's output is not read, and our output is the whole delivery
// mechanism, so such an entry delivers nothing while looking installed.
// Rather than rewrite somebody's hand edit, install stops and says so.
func asyncOwnedEntries(settings map[string]any, invocation string) []string {
	hooks := hooksSection(settings, false)
	if hooks == nil {
		return nil
	}
	var async []string
	for _, event := range sortedKeys(hooks) {
		groups, _ := hooks[event].([]any)
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
				cmd, _ := hook["command"].(string)
				if !ownedCommand(cmd, invocation) {
					continue
				}
				if on, _ := hook["async"].(bool); on {
					async = append(async, event+": "+cmd)
				}
			}
		}
	}
	return async
}

// convergeEntries rewrites agentqueue's own hooks so the settings file holds
// exactly the entries this invocation would install, and nothing left over
// from an older path.
//
// It takes every owned entry out and puts one back per entry in desired,
// which is what makes repeated installs converge instead of accumulating. Two
// details matter to the user: the replacement goes into the wrapper group the
// first owned entry was found in, so their own grouping survives, and a
// hand-edited flag is carried over by rewriting only argv[0] of the command
// it was on. Entries on events we no longer install are dropped.
func convergeEntries(settings map[string]any, invocation string, desired []hookEntry) (added, removed []string, err error) {
	hooks := hooksSection(settings, true)
	if hooks == nil {
		return nil, nil, errors.New(`the settings file has a "hooks" key that is not an object; fix it or use --print`)
	}
	wanted := map[string]bool{}
	for _, e := range desired {
		wanted[e.Event] = true
	}

	// anchors[event] is the wrapper the event's replacement entries go into,
	// and oldCommands[event+action] is the command they inherit their flags
	// from.
	anchors := map[string]map[string]any{}
	oldCommands := map[string]string{}
	for _, event := range sortedKeys(hooks) {
		groups, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			inner, ok := group["hooks"].([]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			isAnchor := false
			keptInner := make([]any, 0, len(inner))
			for _, h := range inner {
				hook, ok := h.(map[string]any)
				if !ok {
					keptInner = append(keptInner, h)
					continue
				}
				cmd, _ := hook["command"].(string)
				if cmd == "" || !ownedCommand(cmd, invocation) {
					keptInner = append(keptInner, h)
					continue
				}
				removed = append(removed, cmd)
				if key := event + "\x00" + actionKey(cmd); oldCommands[key] == "" {
					oldCommands[key] = cmd
				}
				if anchors[event] == nil {
					anchors[event] = group
					isAnchor = true
				}
			}
			if len(keptInner) == 0 && !(isAnchor && wanted[event]) {
				// The wrapper existed only to hold entries that are going,
				// and nothing is going back into it.
				continue
			}
			group["hooks"] = keptInner
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}

	for _, e := range desired {
		cmd := e.Command
		if old, ok := oldCommands[e.Event+"\x00"+actionKey(e.Command)]; ok {
			cmd = rewriteArgv0(old, invocation)
		}
		entry := map[string]any{"type": "command", "command": cmd}
		if anchor := anchors[e.Event]; anchor != nil {
			inner, _ := anchor["hooks"].([]any)
			anchor["hooks"] = append(inner, entry)
		} else {
			groups, _ := hooks[e.Event].([]any)
			hooks[e.Event] = append(groups, map[string]any{"hooks": []any{entry}})
		}
		added = append(added, cmd)
	}
	return added, removed, nil
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

const selfCheckTimeout = time.Second

// executableFromInvocation extracts the argv[0] that shellQuote put into the
// hook command. The installer only emits bare words or single-quoted paths.
func executableFromInvocation(invocation string) (string, error) {
	argv0, _ := splitArgv0(strings.TrimSpace(invocation))
	if argv0 == "" {
		return "", errors.New("empty hook command")
	}
	if strings.HasPrefix(argv0, "'") && strings.HasSuffix(argv0, "'") && len(argv0) >= 2 {
		argv0 = strings.TrimSuffix(strings.TrimPrefix(argv0, "'"), "'")
		argv0 = strings.ReplaceAll(argv0, `\'\''`, "'")
	}
	if argv0 == "" {
		return "", errors.New("empty hook executable")
	}
	return argv0, nil
}

// checkHookCommand proves the exact binary that will be written into the
// settings file can serve the Claude hook protocol. It deliberately runs the
// binary directly rather than through a shell, so a path with spaces is tested
// as one executable and shell syntax cannot affect the probe.
func checkHookCommand(invocation string) (string, error) {
	executable, err := executableFromInvocation(invocation)
	if err != nil {
		return "", fmt.Errorf("refusing to install: ran %s hook claude --self-check; could not resolve the executable: %v. Update or reinstall the agentqueue binary, then try again", invocation, err)
	}
	ran := strings.TrimSpace(invocation) + " hook claude --self-check"
	ctx, cancel := context.WithTimeout(context.Background(), selfCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "hook", "claude", "--self-check")
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("refusing to install: ran %s; probe timed out after %s. Update or reinstall the agentqueue binary, then try again", ran, selfCheckTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("refusing to install: ran %s; probe failed: %v. Update or reinstall the agentqueue binary, then try again", ran, err)
	}
	got := strings.TrimSpace(string(output))
	if got != selfCheckToken {
		return "", fmt.Errorf("refusing to install: ran %s; probe printed %q, want %q. Update or reinstall the agentqueue binary, then try again", ran, got, selfCheckToken)
	}
	return got, nil
}

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
		fs        = newFlagSet("install")
		agent     = fs.String("agent", "", "set up just this agent, skipping the prompt")
		scope     = fs.String("scope", scopeUser, "which config to write: user, project or local")
		settings  = fs.String("settings", "", "settings file to write, overriding --scope")
		command   = fs.String("command", "", "how to spell the agentqueue binary in a hook command")
		yes       = fs.Bool("yes", false, "do not ask for confirmation")
		dryRun    = fs.Bool("dry-run", false, "print what would be added and exit without writing")
		diffOnly  = fs.Bool("diff", false, "print the diff of the settings file and exit without writing or asking")
		printer   = fs.Bool("print", false, "print the hooks block to paste in by hand and exit")
		skipCheck = fs.Bool("skip-self-check", false, "skip verifying the hook command before writing")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue install [--agent claude] [--scope user|project|local] [--settings FILE] [--command PATH] [--yes] [--dry-run] [--diff] [--print] [--skip-self-check]", err)
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
		check := ""
		if info.Found && info.setup == setupHooks {
			if *skipCheck {
				check = fmt.Sprintf("%s hook claude --self-check skipped (--skip-self-check)", invocation)
			} else {
				result, err := checkHookCommand(invocation)
				if err != nil {
					return err
				}
				check = fmt.Sprintf("%s hook claude --self-check passed (%s)", invocation, result)
			}
		}
		if err := installAgent(info, path, invocation, check, *yes, *dryRun, *diffOnly, stdin, stdout, stderr); err != nil {
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
	// Where each choice would write, printed before the choice is made: the
	// destination is the part of this that is hard to undo.
	fmt.Fprintln(stdout)
	for _, info := range infos {
		switch {
		case !info.Found:
			fmt.Fprintf(stdout, "%s is not on $PATH, so nothing would be written for it\n", info.Name)
		case info.setup == setupHooks:
			fmt.Fprintf(stdout, "%s hooks are written to %s\n", info.Name, path)
		case info.setup == setupNone:
			fmt.Fprintf(stdout, "%s needs no configuration, so no file is written for it\n", info.Name)
		case info.setup == setupExtension:
			fmt.Fprintf(stdout, "%s uses the in-process extension; copy extensions/pi/agentqueue.ts as described in its README (no file is written here)\n", info.Name)
		default:
			fmt.Fprintf(stdout, "%s has no integration in this release, so no file is written for it\n", info.Name)
		}
	}
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
func installAgent(info agentInfo, path, invocation, check string, yes, dryRun, diffOnly bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if !info.Found {
		fmt.Fprintf(stdout, "%s: not found on $PATH, nothing to set up\n", info.Name)
		return nil
	}
	switch info.setup {
	case setupNone:
		fmt.Fprintf(stdout, "%s: already supported through `codex queue`, no setup needed\n", info.Name)
		return nil
	case setupExtension:
		fmt.Fprintf(stdout, "%s: use the in-process extension from extensions/pi/README.md; nothing written\n", info.Name)
		return nil
	}
	return installClaudeHooks(path, invocation, check, yes, dryRun, diffOnly, stdin, stdout)
}

// installClaudeHooks merges the claude hook entries into the settings file.
//
// The order is: build the prospective document, prove it safe, show the user
// the real diff, then write. Nothing reaches the disk before
// verifyMergeSafe agrees that the change is the original file plus our own
// entries and nothing else.
func installClaudeHooks(path, invocation, check string, yes, dryRun, diffOnly bool, stdin io.Reader, stdout io.Writer) error {
	before, err := loadSettings(path)
	if err != nil {
		return err
	}
	desired := claudeHookEntries(invocation)

	// A hand-added "async": true on one of our hooks stops delivery dead, and
	// silently, so it is not something to write around.
	if async := asyncOwnedEntries(before, invocation); len(async) > 0 {
		return fmt.Errorf(`%s has agentqueue hooks marked "async": true:
  %s
An async hook's output is never read, and the delivered message travels in
this hook's output, so those entries deliver nothing. Remove the "async" field
(or run `+"`agentqueue uninstall`"+` and install again), then re-run this`,
			path, strings.Join(async, "\n  "))
	}

	after, err := cloneSettings(before)
	if err != nil {
		return err
	}

	// Three outcomes: our hooks point at a different binary and have to be
	// rewritten, some are missing and get added, or there is nothing to do.
	stale := stalePathEntries(before, invocation)
	from := staleCommandSummary(stale)
	missing := missingEntries(before, invocation, desired)
	updating := len(stale) > 0
	switch {
	case updating:
		addedCommands, removedCommands, err := convergeEntries(after, invocation, desired)
		if err != nil {
			return err
		}
		if err := verifyUpdateSafe(before, after, invocation); err != nil {
			return fmt.Errorf("refusing to write %s: the update would touch more than agentqueue's own hooks: %w", path, err)
		}
		missing = desired
		fmt.Fprintf(stdout, "claude: the hooks in %s run a different agentqueue binary than this one:\n", path)
		for _, cmd := range stale {
			fmt.Fprintf(stdout, "    %s\n", cmd)
		}
		fmt.Fprintf(stdout, "  so %d entr%s are replaced by %d pointing at %s, one per delivery point:\n",
			len(removedCommands), plural(len(removedCommands), "y", "ies"), len(addedCommands), invocation)
	case len(missing) == 0:
		fmt.Fprintf(stdout, "claude: hooks already installed in %s, nothing to do\n", path)
		return nil
	default:
		if err := mergeEntries(after, missing); err != nil {
			return err
		}
		if err := verifyMergeSafe(before, after, invocation); err != nil {
			return fmt.Errorf("refusing to write %s: the merge would not be additive: %w", path, err)
		}
		fmt.Fprintf(stdout, "claude: %d hook(s) to add, merged into the existing hooks:\n", len(missing))
	}
	verb := "install"
	if updating {
		verb = "update"
	}
	if err := printChangePlan(stdout, path, verb, before, after); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  backup:  %s (written before the first change)\n", path+settingsBackupSuffix)
	printPathWarning(stdout, invocation)
	if check != "" {
		fmt.Fprintf(stdout, "  checked: %s\n", check)
	}
	if updating {
		fmt.Fprintln(stdout, "  checked: every entry belonging to anything else survives, nothing outside")
		fmt.Fprintln(stdout, "           \"hooks\" changes, and every entry that goes or arrives is an")
		fmt.Fprintln(stdout, "           agentqueue command. The write is refused if that does not hold.")
	} else {
		fmt.Fprintln(stdout, "  checked: every entry already in the file survives, nothing outside \"hooks\"")
		fmt.Fprintln(stdout, "           changes, and every added entry is an agentqueue command. The")
		fmt.Fprintln(stdout, "           write is refused if that does not hold.")
	}

	if diffOnly {
		fmt.Fprintln(stdout, "--diff: nothing written")
		return nil
	}
	if dryRun {
		fmt.Fprintln(stdout, "--dry-run: nothing written")
		return nil
	}
	if !yes {
		if !isTTY(stdin) {
			return errors.New("cannot ask for confirmation without a terminal: pass --yes, --dry-run, --diff or --print")
		}
		fmt.Fprint(stdout, "Proceed? [y/N] ")
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
	// saveSettings backs up the file as it is on disk before writing, so the
	// pre-install state is what lands in the .agentqueue.bak copy.
	if err := saveSettings(path, after); err != nil {
		return err
	}
	if updating {
		// The backup path was printed with the plan above, so it is not
		// repeated here: what matters now is which command the hooks moved to.
		fmt.Fprintf(stdout, "claude: updated %d hook(s) in %s (command changed from %s to %s)\n",
			len(missing), path, from, invocation)
	} else {
		fmt.Fprintf(stdout, "claude: installed %d hook(s) into %s (backup: %s)\n", len(missing), path, path+settingsBackupSuffix)
	}
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
		diffOnly = fs.Bool("diff", false, "print the diff of the settings file and exit without writing or asking")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue uninstall [--agent claude] [--scope user|project|local] [--settings FILE] [--yes] [--dry-run] [--diff]", err)
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
	before, err := loadSettings(path)
	if err != nil {
		return err
	}
	after, err := cloneSettings(before)
	if err != nil {
		return err
	}
	removed := removeOwnedEntries(after, invocation)
	if len(removed) == 0 {
		fmt.Fprintf(stdout, "claude: no agentqueue hooks in %s, nothing to do\n", path)
		return nil
	}
	if err := verifyUninstallSafe(before, after, invocation); err != nil {
		return fmt.Errorf("refusing to write %s: the removal would touch more than agentqueue's own hooks: %w", path, err)
	}
	fmt.Fprintf(stdout, "claude: %d hook(s) to remove from %s:\n", len(removed), path)
	for _, cmd := range removed {
		fmt.Fprintf(stdout, "    %s\n", cmd)
	}
	if err := printChangePlan(stdout, path, "uninstall", before, after); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  backup:  %s (written before the first change)\n", path+settingsBackupSuffix)
	fmt.Fprintln(stdout, "  checked: only agentqueue's own entries go, nothing outside \"hooks\" changes,")
	fmt.Fprintln(stdout, "           and nothing is added. Wrappers and events left empty are pruned.")
	if *diffOnly {
		fmt.Fprintln(stdout, "--diff: nothing written")
		return nil
	}
	if *dryRun {
		fmt.Fprintln(stdout, "--dry-run: nothing written")
		return nil
	}
	if !*yes {
		if !isTTY(stdin) {
			return errors.New("cannot ask for confirmation without a terminal: pass --yes, --dry-run or --diff")
		}
		fmt.Fprint(stdout, "Proceed? [y/N] ")
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
	if err := saveSettings(path, after); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "claude: removed %d hook(s) from %s\n", len(removed), path)
	return nil
}
