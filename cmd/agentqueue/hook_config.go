package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ka2n/crossagent"
	crosshooks "github.com/ka2n/crossagent/hooks"
	"github.com/ka2n/crossagent/paths"
)

const (
	agentqueueToolName = "agentqueue"
	selfCheckTimeout   = time.Second
)

// claudeHookSpecs is the complete agentqueue integration for Claude. IDs are
// stable so crossagent can identify the same hook when an invocation changes.
func claudeHookSpecs(invocation string) []crosshooks.HookSpec {
	return []crosshooks.HookSpec{
		{Event: crosshooks.EventSessionStart, Command: invocation + " register --agent claude", ID: "session-start-register"},
		{Event: crosshooks.EventSessionStart, Command: invocation + " hook claude", ID: "session-start-hook"},
		{Event: crosshooks.EventUserPromptSubmit, Command: invocation + " hook claude", ID: "user-prompt-submit-hook"},
		{Event: crosshooks.EventStop, Command: invocation + " hook claude", ID: "stop-hook"},
		{Event: crosshooks.EventSessionEnd, Command: invocation + " unregister --agent claude", ID: "session-end-unregister"},
	}
}

// hookBlock is used only for --print. Configuration mutation itself is wholly
// delegated to crossagent's plan-first ConfigManager.
func hookBlock(specs []crosshooks.HookSpec) map[string]any {
	byEvent := make(map[string][]any)
	for _, spec := range specs {
		entry := map[string]any{
			"type":                      "command",
			"command":                   spec.Command,
			crosshooks.MarkerOwnerField: agentqueueToolName,
			crosshooks.MarkerIDField(agentqueueToolName): spec.ID,
		}
		event := string(spec.Event)
		byEvent[event] = append(byEvent[event], entry)
	}
	hooks := map[string]any{}
	for event, entries := range byEvent {
		hooks[event] = []any{map[string]any{"hooks": entries}}
	}
	return map[string]any{"hooks": hooks}
}

// shellQuote produces the single-word invocation used in Claude's shell hook
// configuration. The executable passed to the self-check is kept separately,
// because Probe takes an argv vector rather than a shell command string.
func shellQuote(value string) string {
	safe := value != ""
	for i := range value {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-', c == '/', c == ':':
		default:
			safe = false
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// resolveHookCommand chooses the invocation written to settings and the
// executable used by crossagent's direct self-check.
func resolveHookCommand(override, exePath string, look func(string) (string, error)) (invocation, executable string, err error) {
	if value := strings.TrimSpace(override); value != "" {
		return shellQuote(value), value, nil
	}
	if _, err := look(agentqueueToolName); err == nil {
		return agentqueueToolName, agentqueueToolName, nil
	}
	if strings.TrimSpace(exePath) == "" {
		return "", "", errors.New("cannot tell where the agentqueue binary is; pass --command <path>")
	}
	absolute, err := filepath.Abs(exePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve agentqueue path: %w", err)
	}
	return shellQuote(absolute), absolute, nil
}

func currentExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

func parseHookScope(value string) (paths.Scope, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(paths.ScopeUser):
		return paths.ScopeUser, nil
	case string(paths.ScopeProject):
		return paths.ScopeProject, nil
	case string(paths.ScopeLocal):
		return paths.ScopeLocal, nil
	default:
		return "", fmt.Errorf("unknown --scope %q: want user, project or local", value)
	}
}

func currentWorkingDirectory() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return filepath.Clean(cwd), nil
}

func claudeConfigManager(scope, settings, invocation, executable string) (crosshooks.ConfigManager, error) {
	scopeValue, err := parseHookScope(scope)
	if err != nil {
		return crosshooks.ConfigManager{}, err
	}
	cwd, err := currentWorkingDirectory()
	if err != nil {
		return crosshooks.ConfigManager{}, err
	}
	manager := crosshooks.ConfigManager{
		Resolver:      paths.DefaultResolver(),
		Agent:         crosshooks.AgentClaude,
		Scope:         scopeValue,
		CWD:           cwd,
		SettingsPath:  strings.TrimSpace(settings),
		ToolName:      agentqueueToolName,
		Invocation:    invocation,
		Ownership:     crosshooks.DefaultOwnershipPredicate(agentqueueToolName),
		AdoptUnmarked: true,
	}
	if strings.TrimSpace(invocation) != "" {
		manager.Hooks = claudeHookSpecs(invocation)
		manager.Probe = &crosshooks.Probe{
			Command:       []string{executable, "hook", "claude", "--self-check"},
			ExpectedToken: selfCheckToken,
			Timeout:       selfCheckTimeout,
		}
	}
	return manager, nil
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func printHookPlan(stdout io.Writer, plan crosshooks.ChangePlan) {
	if plan.FileExists {
		fmt.Fprintf(stdout, "  file:    %s (exists, %d bytes)\n", plan.Path, plan.FileSize)
	} else {
		fmt.Fprintf(stdout, "  file:    %s (does not exist yet)\n", plan.Path)
	}
	fmt.Fprintf(stdout, "  change:  +%d hook entries, -%d removed, %d modified\n",
		plan.Summary.Added, plan.Summary.Removed, plan.Summary.Modified)
	if len(plan.Unmarked) > 0 {
		fmt.Fprintf(stdout, "  adoption: adopting %d unmarked %s by default (crossagent AdoptUnmarked=true)\n",
			len(plan.Unmarked), plural(len(plan.Unmarked), "agentqueue hook", "agentqueue hooks"))
	}
	for _, event := range plan.Summary.Events {
		fmt.Fprintf(stdout, "    %s: %d existing kept, +%d, -%d, %d modified\n",
			event.Event, event.Kept, event.Added, event.Removed, event.Modified)
	}
	if plan.Diff == "" {
		fmt.Fprintln(stdout, "  diff:    none")
		return
	}
	if plan.DiffNote != "" {
		fmt.Fprintf(stdout, "  note:    %s\n", plan.DiffNote)
	}
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, plan.Diff)
	if !strings.HasSuffix(plan.Diff, "\n") {
		fmt.Fprintln(stdout)
	}
}

func confirmChange(stdin io.Reader, stdout io.Writer) error {
	if !isTTY(stdin) {
		return errors.New("cannot ask for confirmation without a terminal: pass --yes, --dry-run, --diff or --print")
	}
	fmt.Fprint(stdout, "Proceed? [y/N] ")
	line, err := readLine(stdin)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes") {
		return nil
	}
	fmt.Fprintln(stdout, "nothing written")
	return errNoChange
}

var errNoChange = errors.New("change declined")

func applyHookPlan(ctx context.Context, manager crosshooks.ConfigManager, plan crosshooks.ChangePlan, stdin io.Reader, stdout io.Writer, yes, dryRun, diffOnly, skipProbe bool) error {
	if !plan.HasChanges {
		fmt.Fprintf(stdout, "%s: nothing to do\n", plan.Agent)
		return nil
	}
	printHookPlan(stdout, plan)
	if diffOnly {
		fmt.Fprintln(stdout, "--diff: nothing written")
		return nil
	}
	if dryRun {
		fmt.Fprintln(stdout, "--dry-run: nothing written")
		return nil
	}
	if !yes {
		if err := confirmChange(stdin, stdout); err != nil {
			if errors.Is(err, errNoChange) {
				return nil
			}
			return err
		}
	}
	if err := manager.Apply(ctx, plan, crosshooks.ApplyOptions{SkipProbe: skipProbe}); err != nil {
		return err
	}
	if plan.Operation == crosshooks.Install {
		if skipProbe {
			fmt.Fprintln(stdout, "  checked: self-check skipped (--skip-self-check)")
		} else {
			fmt.Fprintln(stdout, "  checked: hook self-check passed")
		}
	}
	if plan.Operation == crosshooks.Install {
		fmt.Fprintf(stdout, "claude: installed hooks into %s (backup: %s)\n", plan.Path, plan.BackupPath)
	} else {
		fmt.Fprintf(stdout, "claude: removed hooks from %s\n", plan.Path)
	}
	return nil
}

func agentVersion(agent crossagent.Agent) string {
	if !agent.Found {
		return "-"
	}
	if agent.Version == "" {
		return "unknown"
	}
	return agent.Version
}

func printDetectedAgents(stdout io.Writer, agents []crossagent.Agent) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tVERSION\tFOUND\tCAPABILITIES")
	for _, agent := range agents {
		found := "no"
		if agent.Found {
			found = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", agent.Name, agentVersion(agent), found, agent.Capabilities)
	}
	_ = tw.Flush()
}

func chooseInstallAgents(ctx context.Context, requested string, stdin io.Reader, stdout io.Writer, yes, dryRun, diffOnly bool) ([]crossagent.Agent, error) {
	detector := crossagent.NewDetector()
	if value := strings.TrimSpace(requested); value != "" {
		name, err := parseAgent(value)
		if err != nil {
			return nil, err
		}
		detected, err := detector.DetectOne(ctx, name)
		if err != nil {
			fmt.Fprintf(stdout, "detection warning: %v\n", err)
		}
		return []crossagent.Agent{detected}, nil
	}

	agents, detectErr := detector.Detect(ctx)
	if detectErr != nil {
		fmt.Fprintf(stdout, "detection warning: %v\n", detectErr)
	}
	printDetectedAgents(stdout, agents)
	var installable []crossagent.Agent
	for _, agent := range agents {
		if agent.Found && agent.Name == crosshooks.AgentClaude {
			installable = append(installable, agent)
		}
	}
	if len(installable) == 0 {
		return nil, nil
	}
	if !isTTY(stdin) {
		if yes || dryRun || diffOnly {
			return installable, nil
		}
		fmt.Fprintln(stdout, "Not a terminal, so nothing was written. Pass --agent claude or --yes to choose Claude.")
		return nil, nil
	}
	fmt.Fprint(stdout, "Set up Claude hooks? [y/N] ")
	line, err := readLine(stdin)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes") {
		return installable, nil
	}
	fmt.Fprintln(stdout, "nothing written")
	return nil, nil
}

func cmdInstall(ctx context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	var (
		fs        = newFlagSet("install")
		agentFlag = fs.String("agent", "", "set up only this agent")
		scope     = fs.String("scope", string(paths.ScopeUser), "which config to write: user, project or local")
		settings  = fs.String("settings", "", "settings file to write, overriding --scope")
		command   = fs.String("command", "", "how to spell the agentqueue binary in a hook command")
		yes       = fs.Bool("yes", false, "do not ask for confirmation")
		dryRun    = fs.Bool("dry-run", false, "print the plan and exit without writing")
		diffOnly  = fs.Bool("diff", false, "print the plan and diff without writing or asking")
		printer   = fs.Bool("print", false, "print the marked hooks block and exit")
		skipCheck = fs.Bool("skip-self-check", false, "skip the pre-write probe of the hook command")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue install [--agent AGENT] [--scope user|project|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff] [--print] [--skip-self-check]",
		"Install Claude hooks through crossagent's safety-checked, plan-first configuration manager. Unmarked agentqueue-looking hooks are adopted by default and shown in the plan; other tools' hooks are preserved. The hook command is self-checked before a write unless --skip-self-check is set. Refusals exit non-zero.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue install [--agent AGENT] [--scope user|project|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff] [--print] [--skip-self-check]", err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	invocation, executable, err := resolveHookCommand(*command, currentExe(), exec.LookPath)
	if err != nil {
		return err
	}
	if *printer {
		return printJSON(stdout, hookBlock(claudeHookSpecs(invocation)))
	}

	chosen, err := chooseInstallAgents(ctx, *agentFlag, stdin, stdout, *yes, *dryRun, *diffOnly)
	if err != nil {
		return err
	}
	for _, detected := range chosen {
		switch detected.Name {
		case crosshooks.AgentClaude:
			if !detected.Found {
				fmt.Fprintln(stdout, "claude: not found on $PATH, nothing to set up")
				continue
			}
			manager, err := claudeConfigManager(*scope, *settings, invocation, executable)
			if err != nil {
				return err
			}
			plan, err := manager.PlanInstall()
			if err != nil {
				return err
			}
			if err := applyHookPlan(ctx, manager, plan, stdin, stdout, *yes, *dryRun, *diffOnly, *skipCheck); err != nil {
				return err
			}
		case crosshooks.AgentCodex:
			fmt.Fprintln(stdout, "codex: no configuration needed")
		case crosshooks.AgentPi:
			fmt.Fprintln(stdout, "pi: use extensions/pi/agentqueue.ts; nothing written")
		default:
			fmt.Fprintf(stdout, "%s: no configuration managed by agentqueue\n", detected.Name)
		}
	}
	return nil
}

func cmdUninstall(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs        = newFlagSet("uninstall")
		agentFlag = fs.String("agent", string(crosshooks.AgentClaude), "agent whose integration to remove")
		scope     = fs.String("scope", string(paths.ScopeUser), "which config to write: user, project or local")
		settings  = fs.String("settings", "", "settings file to write, overriding --scope")
		command   = fs.String("command", "", "hook command invocation used to identify legacy entries")
		yes       = fs.Bool("yes", false, "do not ask for confirmation")
		dryRun    = fs.Bool("dry-run", false, "print the plan and exit without writing")
		diffOnly  = fs.Bool("diff", false, "print the plan and diff without writing or asking")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue uninstall [--agent AGENT] [--scope user|project|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff]",
		"Remove only Claude hooks through crossagent's plan-first configuration manager. Explicitly marked hooks and legacy unmarked agentqueue-looking hooks are removed; other tools' hooks are preserved. Refusals exit non-zero.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue uninstall [--agent AGENT] [--scope user|project|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff]", err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	name, err := parseAgent(*agentFlag)
	if err != nil {
		return err
	}
	if name != crosshooks.AgentClaude {
		fmt.Fprintf(stdout, "%s: nothing managed, nothing to remove\n", name)
		return nil
	}

	invocation := ""
	if value := strings.TrimSpace(*command); value != "" {
		invocation = shellQuote(value)
	}
	manager, err := claudeConfigManager(*scope, *settings, invocation, "")
	if err != nil {
		return err
	}
	manager.Hooks = nil
	manager.Probe = nil
	plan, err := manager.PlanUninstall()
	if err != nil {
		return err
	}
	if !plan.HasChanges {
		fmt.Fprintf(stdout, "claude: no hooks in %s, nothing to do\n", plan.Path)
		return nil
	}
	if *command != "" {
		// The marker-first policy does not need a command to find marked hooks;
		// retain this flag as an explanatory acknowledgement for legacy users.
		fmt.Fprintf(stdout, "uninstall: considering legacy hooks matching %q\n", strings.TrimSpace(*command))
	}
	return applyHookPlan(ctx, manager, plan, stdin, stdout, *yes, *dryRun, *diffOnly, true)
}
