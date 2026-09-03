# agentqueue

A dependency-free, filesystem-backed message queue for AI coding-agent sessions.

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
| `claude` | pull | The session blocks on `agentqueue wait` as a background command; its exit resumes the turn. |
| `codex` | push | `codex queue --thread <name> --message <notice>` injects an arrival notice into the running thread. |
| `pi` | planned | A pi extension watches the queue directory and injects the notice in-process. Not shipped in this release. |
| anything else | — | Write a `Transport` (see below). |

### pi

[pi](https://github.com/earendil-works/pi) needs a third shape. It has no
background-bash equivalent, so the pull model does not apply, and there is no
external CLI or socket to inject into a running interactive session. Its
maintainer resolved that request — issue #145, "Add in-agent-loop event
messaging" — by adding *in-process* injection instead: an extension calls
`pi.sendMessage(..., { triggerTurn: true })`, which starts a new agent loop. So
the supported path for pi is a small pi extension that watches the queue
directory and injects an arrival notice, following pi's own official example
`examples/extensions/file-trigger.ts` ("Watches a trigger file and injects its
contents into the conversation. Useful for external systems to send messages to
the agent."). pi exposes `PI_SESSION_ID` to its bash tool, so the target name
can be the pi session id.

That extension is **not included in this release** — it is planned. It needs no
Go-side `Transport` either: the pending directory is the whole interface the
extension has to read.

## Install

```sh
go get github.com/ka2n/agentqueue                            # library
go install github.com/ka2n/agentqueue/cmd/agentqueue@latest  # CLI
```

## Claude Code (pull delivery)

Claude Code sessions cannot be pushed to, but they can run a background bash
command and get woken when it exits. So the session waits, and the wait *is* the
delivery.

The producer, from anywhere:

```sh
agentqueue push --to claude:reviewer "PR 42 is green, please review it"
```

The Claude Code session, as a **background** bash command:

```sh
agentqueue wait --to claude:reviewer --timeout 3600 --take
```

Run it with `run_in_background`. The command blocks until a message is pending,
claims the oldest one, prints it, and exits — and the harness resumes the
session's turn with that output. Then acknowledge it:

```sh
agentqueue ack --to claude:reviewer <id>
```

Exit codes matter here: `0` means a message arrived, **`3` means the timeout
elapsed with nothing waiting** (not an error — just start another wait), and `1`
is a real failure.

Without `--take`, `wait` prints every pending message in full and leaves them
queued; that is the right choice when several consumers may read the same
mailbox, or when you want to look before claiming.

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

## Storage layout

```
<root>/
└── <agent>/                    # "claude", "codex", ...
    └── <name>/                 # session name or thread id
        ├── pending/<id>.json   # queued, nobody has taken it
        ├── claimed/<id>.json   # a consumer took it, not yet acknowledged
        ├── done/<id>.json      # acknowledged
        └── tmp/                # partial writes, never observed by a reader
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
