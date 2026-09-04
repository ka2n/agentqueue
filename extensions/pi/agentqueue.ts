import * as fs from "node:fs";
import { mkdir, readdir } from "node:fs/promises";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

const MAX_ITEMS_PER_DRAIN = 5;
const DRAIN_DEBOUNCE_MS = 50;
const WATCH_RETRY_MS = 1000;
const TAKE_TIMEOUT_MS = 5000;
const MESSAGE_TYPE = "agentqueue";

type QueueItem = {
	id: string;
	text: string;
	created_at?: string;
	meta?: Record<string, string>;
};

type Runtime = {
	active: boolean;
	pi: ExtensionAPI;
	ctx: ExtensionContext;
	target: string;
	root: string;
	pendingDir: string;
	watcher?: fs.FSWatcher;
	drainTimer?: ReturnType<typeof setTimeout>;
	watchRetryTimer?: ReturnType<typeof setTimeout>;
	draining: boolean;
	drainAgain: boolean;
};

function queueRoot(): string {
	const configured = process.env.AGENTQUEUE_ROOT?.trim();
	if (configured) return resolve(configured);

	const xdgStateHome = process.env.XDG_STATE_HOME?.trim();
	if (xdgStateHome) return resolve(xdgStateHome, "agentqueue");

	return resolve(homedir(), ".local", "state", "agentqueue");
}

function notify(ctx: ExtensionContext, message: string, level: "info" | "warning" | "error" = "info"): void {
	if (ctx.hasUI) ctx.ui.notify(message, level);
}

function closeWatcher(runtime: Runtime): void {
	if (!runtime.watcher) return;
	try {
		runtime.watcher.close();
	} catch {
		// The watcher may already have closed after its directory disappeared.
	}
	runtime.watcher = undefined;
}

function scheduleWatchRetry(runtime: Runtime): void {
	if (!runtime.active || runtime.watchRetryTimer !== undefined) return;
	runtime.watchRetryTimer = setTimeout(() => {
		runtime.watchRetryTimer = undefined;
		void ensureWatcher(runtime);
	}, WATCH_RETRY_MS);
}

async function ensureWatcher(runtime: Runtime): Promise<void> {
	if (!runtime.active || runtime.watcher) return;
	try {
		await mkdir(runtime.pendingDir, { recursive: true });
		if (!runtime.active) return;
		const watcher = fs.watch(runtime.pendingDir, () => {
			try {
				requestDrain(runtime);
			} catch {
				// A watcher callback must never escape into fs.FSWatcher.
			}
		});
		watcher.on("error", () => {
			closeWatcher(runtime);
			scheduleWatchRetry(runtime);
		});
		watcher.on("close", () => {
			if (runtime.watcher === watcher) {
				runtime.watcher = undefined;
				scheduleWatchRetry(runtime);
			}
		});
		runtime.watcher = watcher;
		// A retry may find items that arrived while the directory was absent.
		requestDrain(runtime);
	} catch {
		// The queue root may not exist or may be temporarily unavailable.
		scheduleWatchRetry(runtime);
	}
}

function requestDrain(runtime: Runtime, delay = DRAIN_DEBOUNCE_MS): void {
	if (!runtime.active) return;
	if (runtime.draining) {
		runtime.drainAgain = true;
		return;
	}
	if (runtime.drainTimer !== undefined) return;
	runtime.drainTimer = setTimeout(() => {
		runtime.drainTimer = undefined;
		void drain(runtime, runtime.pi).catch(() => {
			// Delivery is best effort here; a later filesystem event can retry.
		});
	}, delay);
}

async function takeNext(runtime: Runtime, pi: ExtensionAPI): Promise<QueueItem | undefined> {
	const result = await pi.exec(
		"agentqueue",
		["take", "--to", runtime.target, "--next", "--json"],
		{ timeout: TAKE_TIMEOUT_MS },
	);
	if (result.code !== 0 || !result.stdout.trim()) return undefined;

	try {
		const value = JSON.parse(result.stdout) as Partial<QueueItem>;
		if (typeof value.id !== "string" || typeof value.text !== "string") return undefined;
		return value as QueueItem;
	} catch {
		return undefined;
	}
}

function renderDelivery(runtime: Runtime, item: QueueItem): string {
	const ack = `agentqueue ack --to ${runtime.target} ${item.id}`;
	const created = item.created_at ? `  ${item.created_at}` : "";
	return [
		`agentqueue delivered a queued message addressed to this Pi session (${runtime.target}).`,
		"The item is already claimed, so no other consumer will see it.",
		`Acknowledge it after acting on it: ${ack}`,
		"",
		`--- id ${item.id}${created}`,
		item.text.trimEnd(),
	].join("\n");
}

async function drain(runtime: Runtime, pi: ExtensionAPI): Promise<void> {
	if (!runtime.active || runtime.draining) {
		runtime.drainAgain = true;
		return;
	}
	runtime.draining = true;
	let delivered = 0;
	try {
		while (runtime.active && delivered < MAX_ITEMS_PER_DRAIN) {
			const item = await takeNext(runtime, pi);
			if (!item || !runtime.active) break;
			const sendOptions = runtime.ctx.isIdle()
				? { triggerTurn: true }
				: { triggerTurn: true, deliverAs: "followUp" as const };
			pi.sendMessage(
				{
					customType: MESSAGE_TYPE,
					content: renderDelivery(runtime, item),
					display: true,
					details: { id: item.id, target: runtime.target },
				},
				sendOptions,
			);
			delivered++;
		}
	} finally {
		runtime.draining = false;
		const again = runtime.drainAgain;
		runtime.drainAgain = false;
		if (runtime.active && (again || delivered === MAX_ITEMS_PER_DRAIN)) requestDrain(runtime);
	}
}

async function pendingCount(pendingDir: string): Promise<number> {
	try {
		const entries = await readdir(pendingDir, { withFileTypes: true });
		return entries.filter((entry) => !entry.isDirectory() && entry.name.endsWith(".json")).length;
	} catch {
		return 0;
	}
}

async function stopRuntime(runtime: Runtime | undefined): Promise<void> {
	if (!runtime) return;
	runtime.active = false;
	closeWatcher(runtime);
	if (runtime.drainTimer !== undefined) clearTimeout(runtime.drainTimer);
	if (runtime.watchRetryTimer !== undefined) clearTimeout(runtime.watchRetryTimer);
	runtime.drainTimer = undefined;
	runtime.watchRetryTimer = undefined;
}

export default function (pi: ExtensionAPI): void {
	let runtime: Runtime | undefined;

	pi.registerCommand("agentqueue", {
		description: "Show the agentqueue target, root, and pending count",
		handler: async (_args, ctx) => {
			const current = runtime;
			const target = current?.target ?? `pi:${ctx.sessionManager.getSessionId()}`;
			const root = current?.root ?? queueRoot();
			const pendingDir = current?.pendingDir ?? join(root, "pi", ctx.sessionManager.getSessionId(), "pending");
			const pending = await pendingCount(pendingDir);
			notify(ctx, `agentqueue target=${target} root=${root} pending=${pending}`);
		},
	});

	pi.on("session_start", async (_event, ctx) => {
		await stopRuntime(runtime);
		const sessionID = ctx.sessionManager.getSessionId();
		if (!sessionID) {
			runtime = undefined;
			notify(ctx, "agentqueue could not determine the Pi session id", "warning");
			return;
		}

		const root = queueRoot();
		runtime = {
			active: true,
			pi,
			ctx,
			target: `pi:${sessionID}`,
			root,
			pendingDir: join(root, "pi", sessionID, "pending"),
			draining: false,
			drainAgain: false,
		};
		await ensureWatcher(runtime);
		// Let startup finish before triggering a turn. In print mode the initial
		// prompt may already be starting when session_start returns.
		requestDrain(runtime, 0);
		notify(ctx, `agentqueue watching ${runtime.pendingDir}`);
	});

	pi.on("session_shutdown", async () => {
		await stopRuntime(runtime);
		runtime = undefined;
	});
}
