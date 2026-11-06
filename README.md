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
| `claude` | native inbox signal or hook delivery | A privately registered native messaging socket receives a hint when available. Otherwise hooks installed by `agentqueue install` claim what is pending and inject it after the next tool completes (including failures), at session start, at the next prompt, or at the end of a turn. Optionally the session can also pull, by blocking on `agentqueue wait` as a background command. |
| `codex` | inbox signal | Calls `codex queue` with a hint, verifies the thread/queue receipt and retains the pending body. |
| `pi` | push | Supported by the [Pi extension](extensions/pi/README.md), which watches the queue, steers active turns at the next tool boundary, and starts a turn while idle. It claims each item through `agentqueue take`, so competing consumers cannot acquire the same body. |
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
configure - and asks which to set up. Claude Code and Codex use hooks; Pi uses the extension described in
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
- a count of what changes, in hook entries: `+6 hook entries, -0 removed, 0 modified`;
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

Claude ownership is recorded in the known command field: installed commands
end with ` #crossagent:v1:<base64url(tool)>:<base64url(id)>`. The executable
basename predicate is still used to recognize and converge commands when a
binary moves or a hand edit has lost its suffix. Newly written entries have
only Claude's known `type` and `command` fields, so there
are no separate ownership keys for Claude Code to strip when it rewrites
settings. Other tools' commands and metadata remain untouched.

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

### Delivery points

Each one fills a hole the others cannot:

| Hook | Fires | What it covers |
| --- | --- | --- |
| `SessionStart` | a session starts, resumes, is cleared or forked | Drains everything queued while no session was running. Skipped on `source: "compact"`, where the context is being rebuilt rather than a turn beginning. |
| `UserPromptSubmit` | the user submits a prompt | Catches what arrived while the session sat idle, delivered alongside the prompt. |
| `PostToolUse` / `PostToolUseFailure` | a tool succeeds or fails | Injects queued messages as context for the next model request after tool results. |
| `Stop` | the turn is about to end | Catches messages when no further tool runs: when something was delivered it also returns `decision: "block"` with a reason, so the turn continues and the agent works on the message. |

Tool completion hooks stay synchronous and return only additional context. They do
not interrupt running tools; parallel tool calls may finish before the next model
request. While Claude is idle, messages wait for a prompt or session start.
Rerun `agentqueue install --agent claude` to add these hooks to an existing install.

`SessionEnd` gets a hook too, but only to drop the address record: `SessionEnd`
has no decision control, and nothing it prints reaches the model.

**Why all delivery hooks can be installed at once:** delivery *claims* the item. The
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
itself. Claude delivery hooks refresh the session address stored at
`<root>/claude/<session-id>/addr.json` (mode 0600) with the session's cwd, pid
and a timestamp, and indexes it under `<root>/claude/_by_cwd/<cwd>/`, so a
producer can resolve "the session working in this directory". `SessionEnd` runs
`agentqueue unregister`, which removes both. `agentqueue targets` lists what is
registered and what each mailbox holds.

### Claude receive integration

The current adapter uses the installed hooks. These inject claimed message
bodies at supported session boundaries. They cannot on their own promise an
idle wake, so a background wait remains an optional fallback when no automatic
receiving integration is active.

Current Claude Code also documents a per-session inbox socket and MCP Channels.
Those are not implemented by this adapter. Their write completion is not a
receiving acknowledgement, and receiver policy may hold or refuse a message;
agentqueue does not bypass that policy. The socket token is never persisted.
See the official [socket contract](https://code.claude.com/docs/en/cross-session-messaging#the-sessions-inbox-socket).

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

## Notification and receive contract

`push` persists the body before signalling the receiver. Notifications contain
an inbox hint, notice ID and timestamp, never the body. Codex submissions use the
registered receiving profile (`CODEX_HOME`) and require a matching thread UUID
and queue ID receipt. Claude can receive the same hint through its registered
native messaging socket; the token lives only in a private mode-600 file.
A successful socket write is recorded as `written`, not receiver acceptance.
Without native routing, existing hooks/extensions deliver pending bodies.

On a hint, use `agentqueue receive --to TARGET --json`. It atomically claims up
to 20 pending items (configurable through `--limit`, at most 100). Handle each
body and `ack` its ID, then receive again until `[]`. Empty is successful: a
hook or another receiver may already have consumed the hinted items. If an
integration already supplied the body, act on that body and its receipt
instructions without fetching it again. Blocking wait remains an explicit
fallback when automatic receiving is unavailable.

Notification receipts are stored independently of body pending/claimed/done
state. Repeating `Queue.Notify` for an already notified ID does not send another
hint merely because the body is unread. Wake coalescing is enabled only for receivers registered with
`Address.WakeProtocol = agentqueue.WakeProtocolVersion`; the updated hooks and
register command advertise this automatically. Older or unknown receivers retain
per-new-message notification behavior, so an old receive command cannot leave
new wakes permanently suppressed. Upgrade both producers and receivers.

While a verified queued hint is outstanding, new IDs join that same flight
instead of queuing more hints. Hooks and `wait --take` share its OS lock, but
can still deliver bodies before the host delivers that hint. Automatic body
consumption does not release the outstanding wake: otherwise every subsequent
message would queue another stale hint. If an automatic receiver claims first,
no native hint is submitted for its body. An empty explicit receive ends the
flight; a receiving flight has a five-minute renewable drain lease for recovery.
Written-only socket receipts retain existing hook fallback.

An already accepted host hint cannot be retracted, so one stale empty receive
can remain. The change prevents a burst of redundant wakes without delaying
body delivery. Explicit take/receive remains available for manual recovery.
An OS lock serializes native submissions; a persisted reservation bounds crash
or ambiguous acceptance retries to a 30-second uncertainty interval. Retried
signals can duplicate, but atomic claims prevent duplicate body acquisition.
Notification acceptance, body ACK, application read state and Task approval
are distinct. `DirectDelivery` remains source-compatible but is no longer used.

## Commands

These are the commands shipped by the CLI. Run `agentqueue <command> -h` for
syntax and examples without consulting this document.

| Command and flags | What it does |
| --- | --- |
| `push --to <agent>:<name> [--root DIR] [--meta KEY=VALUE]... [TEXT\|-]` | Enqueue a message and notify the target when supported. `-` or no text reads the body from stdin; repeated `--meta` flags attach metadata. Exit 0 when queued, 1 on an error (including notification failure; the item remains queued), 2 for invalid usage. |
| `wait --to <agent>:<name> [--root DIR] [--timeout SECONDS] [--take] [--json]` | Wait for a pending message. `--take` claims and prints the oldest one; without it, all pending bodies are reported without claiming. `--timeout` defaults to 3600 seconds and `0` waits forever. Exit 3 when the timeout expires. |
| `list --to <agent>:<name> [--root DIR] [--state pending\|claimed\|done] [--json]` | List one mailbox state, defaulting to `pending`, without changing messages. |
| `receive --to <agent>:<name> [--limit N] [--json]` | Claim a batch of unread bodies. Empty is a successful `[]`; handle and ACK each ID, then drain until empty. |
| `take --to <agent>:<name> [--root DIR] [--json] (--next \| ID)` | Claim exactly one pending message, either the oldest with `--next` or the given ID, and print it. |
| `ack --to <agent>:<name> [--root DIR] ID` | Mark one pending or claimed message done after acting on it. |
| `targets [--agent AGENT] [--root DIR] [--json]` | List mailboxes and their pending, claimed and done counts, recorded cwd and address status. A mailbox is storage metadata, not proof of a running session. |
| `sessions [--agent claude\|codex\|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]` | Discover where sessions are recorded. It reports location metadata only; `SOURCE` identifies the backing CLI/storage source and `STATE` is copied only from Claude's CLI, empty otherwise. Claude's transcript scan always runs alongside `claude agents --json`. |
| `register [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet] [--json]` | Record a session address from an explicit target or a hook payload/environment. `--quiet` suppresses the success line; `--json` prints the stored address. |
| `unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]` | Remove a recorded session address. Removing a missing address succeeds. |
| `hook claude [--to <agent>:<name>] [--root DIR] [--max N] [--log FILE] [--no-block]` | Serve Claude Code's synchronous hook protocol: read stdin, claim pending messages and print injection JSON. It always exits 0, even on internal failure, so it cannot disrupt a session. |
| `install [--agent AGENT] [--scope user\|project\|local] [--settings FILE] [--command INVOCATION] [--yes] [--dry-run] [--diff] [--print] [--skip-self-check]` | Install the supported integration with a safety-checked settings diff. Claude writes suffix-marked commands with no extra JSON ownership fields; Codex writes `hooks.json` and requires review through `/hooks`; Pi uses the extension. `--settings FILE` requires an explicit `--agent` so each file contains only that agent's hooks. `--diff`, `--dry-run` and `--print` do not write. A refused safety check exits non-zero. |
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
	Notify(ctx context.Context, n Notice) error // hand off through the receiver integration
}
```

Implement it, then `agentqueue.Register(&myTransport{})` — after which
`<myagent>:<name>` targets work everywhere, CLI included. The `Notice` you get
carries `Target`, `ItemID`, `Pending`, `FetchCmd` and the full `Item`.

Pull-only integrations can keep `Notify` as a no-op. Native hint adapters can
implement `ReceiptTransport.NotifyWithReceipt`, returning queued (verifiable
host queue receipt), written (socket write only), or deferred (existing receiver
must deliver). Use `Queue.Notify` for persisted items so reservations and
notification deduplication remain centralized; it never claims or ACKs bodies.

## Credits / prior art

- The design comes from [coji's article on sharing artifacts between Claude
  Code, Codex and Cursor](https://zenn.dev/coji/articles/artifactshare-preview-claude-codex-cursor).
- [fujibee/agmsg](https://github.com/fujibee/agmsg) is a related cross-agent
  messaging project with a different scope: agent-to-agent conversation, where
  this library is a queue an *external* producer pushes into.

Extracted from [ka2n/jill](https://github.com/ka2n/jill).

## License

MIT

Session addresses include optional hook-time `context`: the observed working
directory, Git worktree root, branch and full HEAD, plus a terminal title when
Herdr is available and the pane identity matches. `sessions` displays these
facts from storage; it never refreshes Git or terminal metadata while listing.
`observed_at` identifies when the snapshot was collected, so it can differ
from the current checkout. A later registration or supported delivery hook
refreshes it. Address-only sessions use source `queue-address` and do not
imply a running process or a new `last_activity` time.

### Calling session reply endpoint

Go applications can use `ResolveCaller(CallerOptions{})` to resolve a reply
endpoint without guessing from a working directory or listing sessions.
Explicit `Target`/`Root` options override environment defaults independently.
Target resolution delegates to crossagent's `processcontext.Resolve`: native
`CODEX_THREAD_ID`/`CLAUDE_CODE_SESSION_ID` or integration-provided
`CROSSAGENT_AGENT`/`CROSSAGENT_SESSION_ID`.
Missing or ambiguous identity returns an error. Root resolution uses
`AGENTQUEUE_ROOT`, then `XDG_STATE_HOME/agentqueue`, then
`HOME/.local/state/agentqueue`; applications may pass their own root override.

The Claude SessionStart hook writes the current target and queue root to
`CLAUDE_ENV_FILE` when supplied. The Pi extension exports the active target
and root before commands run, and updates them when a new session starts.
Custom host launchers should export `CROSSAGENT_AGENT`, `CROSSAGENT_SESSION_ID`
and `AGENTQUEUE_ROOT` for their own session. Conflicting inherited native identity
must be removed by the launcher or callers must supply an explicit target.
A resolved endpoint does not guarantee an active consumer or host wake-up.

The inbox notification state-machine property test runs its fixed corpus with
`go test ./...`. For generated operation sequences, run
`go test -run '^$' -fuzz FuzzInboxFlightStateMachine -fuzztime=30s`.
It checks body conservation/content, unique acquisition and notification counts
across push, notify, automatic/explicit receive, ACK and time advancement.
Concurrent lock interleavings are covered separately by race-enabled tests.
