# agentqueue Pi extension

This extension delivers filesystem-backed `agentqueue` messages to a Pi session.
It watches the current session's `pending/` mailbox, claims each item through the
CLI, and injects the claimed text into the conversation.

## Install

Copy the extension into Pi's global extension directory:

```bash
mkdir -p ~/.pi/agent/extensions
cp extensions/pi/agentqueue.ts ~/.pi/agent/extensions/
```

For a one-off test, load it directly:

```bash
pi --extension /path/to/agentqueue/extensions/pi/agentqueue.ts
```

The `agentqueue` executable must be available on Pi's `PATH`. The extension uses
the same queue-root resolution as the CLI:

1. `$AGENTQUEUE_ROOT`
2. `$XDG_STATE_HOME/agentqueue`
3. `~/.local/state/agentqueue`

A message addressed to a Pi session uses its session id as the name:

```bash
agentqueue push --to pi:<session-id> "Please inspect the new queue integration"
```

The message includes its item id and an acknowledgement command. Acknowledge it
after acting on it:

```bash
agentqueue ack --to pi:<session-id> <item-id>
```

## Check Wiring

Run `/agentqueue` inside Pi. It reports the current `pi:<session-id>` target, the
queue root, and the number of pending item files.

The extension performs an initial drain at session start, then watches the
session mailbox for new files. It never reads item bodies directly from the
filesystem: `agentqueue take --next --json` claims them first, which prevents
multiple consumers from receiving the same item. At most five items are injected
per drain, and bursts are debounced.
