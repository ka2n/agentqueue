# agentqueue

A filesystem-backed message queue for AI coding-agent sessions. Pure Go, no
CGO: it cross-compiles anywhere and `go install` needs no toolchain beyond Go.

## The problem

You have a coding agent working in a session — a Claude Code session, a Codex
thread — and something outside it needs to tell it something: a CI job finished,
a reviewer left a comment, another agent produced the artifact it was waiting
for. There is no inbox. The session is either mid-turn or idle at a prompt, and
whatever wants its attention has no supported way to hand it a message.

`agentqueue` gives each session a mailbox. A producer pushes a message addressed
to `<agent>:<name>`; the session consumes it. The queue is a directory tree, so
producer and consumer share nothing but a path — no daemon, no broker, no port.
Every state change is an atomic `rename(2)`, so any number of processes can push
and consume against the same root without locking.

Delivery is per-agent, because agents differ in how a message can reach a
running session:

| Agent | Model | How it works |
| --- | --- | --- |
| `claude` | push, at the next boundary | Hooks installed by `agentqueue install` claim what is pending and inject it at session start, at the next prompt, or at the end of a turn. Optionally the session can also pull, by blocking on `agentqueue wait` as a background command. |
| `codex` | push | `codex queue --thread <name> --message <notice>` injects an arrival notice into the running thread. No setup needed. |
| `pi` | push | Supported by the [Pi extension](extensions/pi/README.md), which watches the queue and injects with `pi.sendMessage(..., { triggerTurn: true })`. It claims each item through `agentqueue take`, so delivery stays exactly-once. |
| anything else | — | Write a `Transport` (see below). |

### pi

[pi](https://github.com/earendil-works/pi) needs a third shape. It has no
background-bash equivalent, so the pull model does not apply, and there is no
external CLI or socket to inject into a running interactive session. Its
maintainer resolved that request — issue #145, "Add in-agent-loop event
messaging" — by adding *in-process* injection instead: an extension calls
`pi.sendMessage(..., { triggerTurn: true })`, which starts a new agent loop.

The shipped [extension](extensions/pi/agentqueue.ts) watches the current
session's `pending/` directory, claims each item through `agentqueue take`, and
then injects it with `pi.sendMessage`. Claiming before injection makes delivery
exactly-once even if another consumer is watching the same mailbox. Install it
as described in [extensions/pi/README.md](extensions/pi/README.md); the
extension uses `PI_SESSION_ID` as the target name and needs no Go-side
`Transport`.

## Install

```sh
go get github.com/ka2n/agentqueue                            # library
go install github.com/ka2n/agentqueue/cmd/agentqueue@latest  # CLI
```

Then set up the agents you have:

```sh
agentqueue install
```

It prints what it found - which agents are on your `$PATH`, how a message
reaches each one, where each one's config would be written, and what is left to
configure - and asks which to set up. Claude Code needs hooks; Codex needs
nothing; Pi uses the extension described in
[extensions/pi/README.md](extensions/pi/README.md) and writes no agent config.

## Claude Code (hook delivery)

Claude Code cannot be pushed to from outside, but it runs **hooks**, and a hook
can return text that reaches the model's context. That is the supported
delivery path, and `agentqueue install` sets it up:

```sh
agentqueue install --agent claude --scope user
```

Before writing anything it shows the change to your settings file and asks:

- the file, whether it exists, and how big it is;
- a count of what changes, in hook entries: `+5 hook entries, -0 removed, 0 modified`;
- every hook event already in the file and what happens to it, including the
  events agentqueue never touches - `PreToolUse: 4 existing entries kept, none
  added`, so you can see they are accounted for;
- a unified diff of the file before and after, with three lines of context;
- the backup path, `<file>.agentqueue.bak`, written before the first change.

Both sides of the diff are printed key-sorted, because writing the file
re-encodes it as JSON and that sorts the keys: the key order on disk will
change even where no value does, and a key-sorted diff shows the value changes
only.

**The presentation is backed by an enforced invariant.** install builds the
prospective document, then verifies it against the original before anything
reaches the disk: entries belonging to other tools remain under wrappers whose
other fields (`matcher` included) survive; nothing outside `hooks` changes; and
every added or rewritten entry is an agentqueue command. If any of that does not
hold, nothing is written and the command exits non-zero. `uninstall` enforces
the mirror image: only entries owned by the agentqueue command predicate may
disappear, nothing may be added, nothing outside `hooks` may change, and wrappers
and event lists that removal empties are pruned.

Claude ownership is command-based: a hook whose executable basename is
`agentqueue` is owned by this integration, regardless of unknown marker keys
left by an older installer. Newly written Claude entries contain no ownership
markers, because Claude Code strips unknown hook-entry keys when it rewrites
settings. Other tools' commands remain untouched even when they carry their own
marker fields.

The write itself goes through a temp file in the same directory and one
rename, so an interrupt or a full disk part-way through leaves your
`settings.json` exactly as it was rather than truncated, and the file keeps its
own mode - a config you chmodded to `0600` does not come back
world-readable.

`--scope user|project|local` selects the Claude settings file and
`--settings FILE` overrides that selection. `--command INVOCATION` controls
how the binary is spelled in the installed hook. `--yes` skips the question,
`--diff` prints the diff and exits without writing or asking, `--dry-run` prints
the whole plan and stops, and `--print` emits just the hooks block to paste in
yourself without inspecting or writing a settings file. By default install
runs a self-check of the exact command it is about to write; `--skip-self-check`
opts out only for an intentionally older or wrapped binary. Installing is
idempotent: re-running it is a no-op. `agentqueue uninstall` removes entries
owned by the same command predicate.

### Reinstalling after the binary moves

An installed hook is recognised as agentqueue's own by the binary it runs,
whatever path that binary sits at. So when the binary moves - a new
`GOBIN`, a Nix store path, `--command` pointed somewhere else - a reinstall
does not report "already installed" and leave the old path behind: it replaces
every one of its own entries with exactly one per delivery point at the current
path, and the plan reports those entries as modifications. Repeated installs
converge on the current path instead of accumulating copies. Your grouping is
kept - the replacement goes back into the wrapper the old entry was in, matcher
and all - and a flag you added to one of our commands by hand is carried over,
because only `argv[0]` is rewritten. The same enforced invariant applies, with
removals allowed for agentqueue's own entries only.

### Why these hooks are not async

Claude Code can run a hook asynchronously, and for a fire-and-forget observer
that is the right thing: the session does not wait. **These hooks must stay
synchronous.** Their *output is the protocol* - a delivered message travels in
`hookSpecificOutput.additionalContext`, and the `Stop` hook's
`decision: "block"` is what keeps the turn alive - and Claude Code reads
neither from an async hook. An `"async": true` on one of these entries would
silently deliver nothing, with no error to notice, which is why `install`
never writes one and refuses to run if it finds one that was added by hand.

It is also why `hook claude` has to stay fast: Claude Code waits for it, so it
does a directory scan and a rename, with no network call and no transcript
read.

Then, from anywhere:

```sh
agentqueue push --to claude:<session-id> "PR 42 is green, please review it"
```

### The three delivery points

Each one fills a hole the others cannot:

| Hook | Fires | What it covers |
| --- | --- | --- |
| `SessionStart` | a session starts, resumes, is cleared or forked | Drains everything queued while no session was running. Skipped on `source: "compact"`, where the context is being rebuilt rather than a turn beginning. |
| `UserPromptSubmit` | the user submits a prompt | Catches what arrived while the session sat idle, delivered alongside the prompt. |
| `Stop` | the turn is about to end | The only point that can act with no user present: when something was delivered it also returns `decision: "block"` with a reason, so the turn continues and the agent works on the message. |

`SessionEnd` gets a hook too, but only to drop the address record: `SessionEnd`
has no decision control, and nothing it prints reaches the model.

**Why all three can be installed at once:** delivery *claims* the item. The
hook runs `TakeNext` in a loop, which renames the file out of `pending/`, so
whichever hook fires first is the only one that sees it. No message is
delivered twice.

The loop guard is Claude Code's own `stop_hook_active` flag. When a `Stop` hook
has already blocked the current turn, the next `Stop` delivery injects context
but does not block again. `--no-block` disables blocking entirely.

The hook command is `agentqueue hook claude`. You do not run it by hand: it
reads the hook payload on stdin and prints, only when it claimed something:

```json
{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"..."},"decision":"block","reason":"1 queued message(s) arrived for claude:abc123; act on them before finishing."}
```

With nothing pending it prints nothing at all. It is built to never disrupt a
session: any internal error is logged (`--log <file>`) and it still exits 0
with empty stdout. `--max` caps how many messages one invocation delivers
(default 5), since every delivered byte is spent from the session's context
window.

The injected text names each message's id, creation time, metadata and body,
says plainly that these came from an external producer rather than from the
user, and tells the agent the items are already claimed and how to acknowledge
one:

```sh
agentqueue ack --to claude:<session-id> <id>
```

### Addressing a session

A session id is not something a producer knows in advance, so a session records
itself. The `SessionStart` hook runs `agentqueue register`, which writes
`<root>/claude/<session-id>/addr.json` (mode 0600) with the session's cwd, pid
and a timestamp, and indexes it under `<root>/claude/_by_cwd/<cwd>/`, so a
producer can resolve "the session working in this directory". `SessionEnd` runs
`agentqueue unregister`, which removes both. `agentqueue targets` lists what is
registered and what each mailbox holds.

### The session inbox socket is not used

Claude Code exports `CLAUDE_CODE_MESSAGING_SOCKET` and
`CLAUDE_CODE_MESSAGING_TOKEN` to hooks and Bash commands, for
[cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging.md).
This library does **not** deliver over that socket, for one measured reason: a
raw write from a process that is not itself a Claude Code session is accepted
and then goes nowhere. There is no response, no early close, and the same
behavior for valid JSON, unknown record types and non-JSON alike; nothing is
delivered and nothing is acknowledged. Delivered peer messages carry the
sender's own socket path as their identity, which a non-session producer cannot
supply. So the socket path is recorded in `addr.json` for diagnostics only, and
the token is never stored anywhere - it is a credential, and there is nothing
this library could do with it.

Hooks, documented at [code.claude.com/docs/en/hooks](https://code.claude.com/docs/en/hooks),
are the supported path, which is what `install` configures.

## Session discovery

`agentqueue sessions` lists where sessions are recorded so a producer can find a
useful target. It reads each agent's own storage and uses its CLI where
available; it does not infer a session location from process information:

```sh
agentqueue sessions
agentqueue sessions --agent claude --cwd /path/to/project --limit 10
```

The output includes each session's id, working directory, label, last activity,
queue status, and a `SOURCE` column naming where the row was found. Claude rows
are the union of `claude agents --json` and a recent transcript scan; transcript
records are always considered because the CLI does not enumerate every
interactive session. In fact, `claude agents --json` reported exactly one
background session on a machine that had several live interactive sessions —
the reason the transcript scan always runs alongside it. The transcript's own
`cwd` and session id are used rather than decoding its lossy project-directory
name. Claude transcript scanning defaults to the last seven days and can be
adjusted with `--max-age 168h`.

This command reports location metadata, not process identity. A discovered row
is not proof that the session is currently running. `STATE` is passed through
from Claude's own CLI when it reports one and is left blank for storage-only
rows; no process or host-pid probe is performed.

## Claude Code (background wait, no hooks)

The pull method still works and needs no configuration. The session runs, as a
**background** bash command:

```sh
agentqueue wait --to claude:reviewer --timeout 3600 --take
```

Run it with `run_in_background`. The command blocks until a message is pending,
claims the oldest one, prints it, and exits - and the harness resumes the
session's turn with that output. Then acknowledge it:

```sh
agentqueue ack --to claude:reviewer <id>
```

Exit codes matter here: `0` means a message arrived, **`3` means the timeout
elapsed with nothing waiting** (not an error - just start another wait), and `1`
is a real failure.

Without `--take`, `wait` prints every pending message in full and leaves them
queued; that is the right choice when several consumers may read the same
mailbox, or when you want to look before claiming.

This is the agent-initiated half of the same queue: an item claimed by a hook
is gone from `pending/`, so a concurrent `wait` will not show it again.

## Codex (push delivery)

A Codex thread has an external injection point, so the notice can be pushed:

```sh
agentqueue push --to codex:1f0a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 "the schema migration landed"
```

That stores the message, then shells out to:

```sh
codex queue --thread 1f0a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 \
  --message "[agentqueue] 1 new message for codex:1f0a... (id 1756800000000-0a1b2c3d4e5f, 1 pending). Fetch it with: agentqueue take --to codex:1f0a... --next"
```

The agent in that thread sees the notice and fetches the body itself:

```sh
agentqueue take --to codex:1f0a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 --next
agentqueue ack  --to codex:1f0a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 <id>
```

## The notice carries no payload

This is deliberate. The notification path — a `codex queue` argv, and whatever
other transports get added — only says *that* something arrived and *how to
fetch it*: the target, the item id, the pending count, and the fetch command.
The body never travels on it.

Three reasons. A message can be large, and an injected prompt is a bad place for
a wall of text. A message can be sensitive, and the notification path is the
part most likely to be logged, echoed into a transcript, or visible in a process
list. And the fetch is the claim: pulling the body through `take` moves the item
out of `pending`, so exactly one consumer gets it even when several are racing —
whereas a notice containing the body would deliver it before anybody claimed
anything.

Hook delivery is the one place a body does travel, and the same reasoning is
why that is sound rather than an exception: the hook claims the item first and
then renders it, so the injection *is* the claim. There is no window in which
the body has been shown to a session that does not own it.

## Commands

These are the commands shipped by the CLI. Run `agentqueue <command> -h` for
syntax and examples without consulting this document.

| Command and flags | What it does |
| --- | --- |
| `push --to <agent>:<name> [--root DIR] [--meta KEY=VALUE]... [TEXT\|-]` | Enqueue a message and notify the target when supported. `-` or no text reads the body from stdin; repeated `--meta` flags attach metadata. Exit 0 when queued, 1 on an error (including notification failure; the item remains queued), 2 for invalid usage. |
| `wait --to <agent>:<name> [--root DIR] [--timeout SECONDS] [--take] [--json]` | Wait for a pending message. `--take` claims and prints the oldest one; without it, all pending bodies are reported without claiming. `--timeout` defaults to 3600 seconds and `0` waits forever. Exit 3 when the timeout expires. |
| `list --to <agent>:<name> [--root DIR] [--state pending\|claimed\|done] [--json]` | List one mailbox state, defaulting to `pending`, without changing messages. |
| `take --to <agent>:<name> [--root DIR] [--json] (--next \| ID)` | Claim exactly one pending message, either the oldest with `--next` or the given ID, and print it. |
| `ack --to <agent>:<name> [--root DIR] ID` | Mark one pending or claimed message done after acting on it. |
| `targets [--agent AGENT] [--root DIR] [--json]` | List mailboxes and their pending, claimed and done counts, recorded cwd and address status. A mailbox is storage metadata, not proof of a running session. |
| `sessions [--agent claude\|codex\|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]` | Discover where sessions are recorded. It reports location metadata only; `SOURCE` identifies the backing CLI/storage source and `STATE` is copied only from Claude's CLI, empty otherwise. Claude's transcript scan always runs alongside `claude agents --json`. |
| `register [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet] [--json]` | Record a session address from an explicit target or a hook payload/environment. `--quiet` suppresses the success line; `--json` prints the stored address. |
| `unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]` | Remove a recorded session address. Removing a missing address succeeds. |
| `hook claude [--to <agent>:<name>] [--root DIR] [--max N] [--log FILE] [--no-block]` | Serve Claude Code's synchronous hook protocol: read stdin, claim pending messages and print injection JSON. It always exits 0, even on internal failure, so it cannot disrupt a session. |
| `install [--agent AGENT] [--scope user\|project\|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff] [--print] [--skip-self-check]` | Install the supported integration with a safety-checked settings diff. Claude owns entries whose command matches the agentqueue predicate and writes new entries without ownership markers; Codex needs no setup, and Pi uses the extension. `--diff`, `--dry-run` and `--print` do not write. A refused safety check exits non-zero. |
| `uninstall [--agent AGENT] [--scope user\|project\|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff]` | Remove only Claude hooks whose command matches agentqueue's ownership predicate, preserving other settings and hooks. `--diff` and `--dry-run` do not write; a refused safety check exits non-zero. |

Queue commands that expose `--root` resolve it from that flag, then
`$AGENTQUEUE_ROOT`, `$XDG_STATE_HOME/agentqueue`, and
`~/.local/state/agentqueue`. `sessions` has no `--root` flag and uses the same
environment/default resolution for queue-status lookups. `install` and
`uninstall` use `--scope` or `--settings` for Claude's settings file instead.

Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Success. For `wait`, a message arrived. `hook claude` always returns 0 at runtime, including internal failures. |
| `1` | An operational error occurred; `install` and `uninstall` also use this for a refused safety check. |
| `2` | A usage problem or command help. |
| `3` | `wait` timed out with no message. Not an error. |

## Storage layout

```
<root>/
└── <agent>/                    # "claude", "codex", ...
    ├── <name>/                 # session name or thread id
    │   ├── pending/<id>.json   # queued, nobody has taken it
    │   ├── claimed/<id>.json   # a consumer took it, not yet acknowledged
    │   ├── done/<id>.json      # acknowledged
    │   ├── addr.json           # where this session is, mode 0600
    │   └── tmp/                # partial writes, never observed by a reader
    └── _by_cwd/<cwd>/<name>    # empty marker: this session runs in this cwd
```

Item ids are `<unix-millis>-<6 random bytes hex>`, so a directory listing sorts
oldest-first and concurrent pushes cannot collide. A push writes into `tmp/` and
renames into `pending/`, so a reader never sees a half-written file. Claiming is
`pending/ -> claimed/`, acknowledging is `-> done/`; a losing racer's rename
simply fails with `ENOENT` and it moves on to the next id.

Both path segments are sanitized to `[A-Za-z0-9._-]`, so no target string can
escape the root or introduce a separator.

The CLI resolves the root from, in order: `--root`, `$AGENTQUEUE_ROOT`,
`$XDG_STATE_HOME/agentqueue`, `~/.local/state/agentqueue`.

## Go API

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/ka2n/agentqueue"
)

func main() {
	q, err := agentqueue.Open("/var/lib/agentqueue")
	if err != nil {
		log.Fatal(err)
	}

	target := agentqueue.Target{Agent: "codex", Name: "my-thread"}

	// Producer: store the item, then notify the agent. A notify failure never
	// loses the message - it stays pending and the item is returned anyway.
	item, err := q.PushAndNotify(context.Background(), target, "build is green", map[string]string{"from": "ci"})
	if err != nil && !errors.Is(err, agentqueue.ErrNotify) {
		log.Fatal(err)
	}
	fmt.Println("queued", item.ID)

	// Consumer: claim the oldest pending item, act on it, acknowledge it.
	got, err := q.TakeNext(target)
	if errors.Is(err, agentqueue.ErrEmpty) {
		return
	} else if err != nil {
		log.Fatal(err)
	}
	fmt.Println(got.Text)
	if err := q.Ack(target, got.ID); err != nil {
		log.Fatal(err)
	}
}
```

`Wait(ctx, target, timeout)` blocks until something is pending and returns the
items *without* claiming them; it reports `ErrTimeout` on deadline and
`ctx.Err()` when the context ends first. A timeout of zero waits forever.

`Open` takes options: `WithFetchCmd(func(Target) string)` to render the fetch
command your own CLI exposes (this is what lands in the notice — the default is
`agentqueue take --to <target> --next`), `WithPollInterval(d)` to change how
often `Wait` rescans (default 250ms), and `WithClock(fn)` to supply the clock
that stamps `CreatedAt`.

Sentinel errors: `ErrEmpty`, `ErrTimeout`, `ErrNotFound`, `ErrInvalidTarget`,
`ErrNotify`.

### Addresses

A session records where it can be reached, so a producer can address it by
session id or by working directory:

```go
type Address struct {
	Agent        string
	Name         string
	Cwd          string
	PID          int
	UpdatedAt    time.Time
	Socket       string // diagnostics only; see the socket note above
	AgentVersion string
}

func (q *Queue) PutAddress(a Address) error
func (q *Queue) Address(t Target) (*Address, error)   // ErrNotFound when unregistered
func (q *Queue) RemoveAddress(t Target) error         // no address is not an error
func (q *Queue) TargetsByCwd(agent, cwd string) ([]Target, error)
```

`PutAddress` writes `addr.json` with mode 0600 and tightens the queue root to
0700. There is deliberately no field for the session's messaging token.

`Mailboxes(agent string) ([]Mailbox, error)` walks the root and reports each
target's pending, claimed and done counts along with its address, if any; an
empty agent covers every agent. That is what `agentqueue targets` prints.

## Adding an agent

A transport is two methods:

```go
type Transport interface {
	Name() string                              // the agent name used in a Target
	Notify(ctx context.Context, n Notice) error // deliver the arrival notice
}
```

Implement it, then `agentqueue.Register(&myTransport{})` — after which
`<myagent>:<name>` targets work everywhere, CLI included. The `Notice` you get
carries `Target`, `ItemID`, `Pending` and `FetchCmd`; keep the body out of it.

If your agent is pull-based, like Claude Code, `Notify` should be a no-op:
enqueuing is complete once the item is stored, and delivery happens when the
session's own `wait` returns. If it is push-based, deliver a single line naming
the item and the fetch command — see `transport_codex.go` for the shape.

## Credits / prior art

- The design comes from [coji's article on sharing artifacts between Claude
  Code, Codex and Cursor](https://zenn.dev/coji/articles/artifactshare-preview-claude-codex-cursor).
- [fujibee/agmsg](https://github.com/fujibee/agmsg) is a related cross-agent
  messaging project with a different scope: agent-to-agent conversation, where
  this library is a queue an *external* producer pushes into.

Extracted from [ka2n/jill](https://github.com/ka2n/jill).

## License

MIT
