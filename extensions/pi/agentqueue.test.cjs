const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { stripTypeScriptTypes } = require('node:module');
const { EventEmitter } = require('node:events');
const { join, resolve } = require('node:path');
const { homedir } = require('node:os');
const vm = require('node:vm');

test('Pi lifecycle refreshes context and unregisters without depending on metadata success', async () => {
  const calls = [];
  let failSpawn = false;
  const spawn = (command, args, options) => {
    if (failSpawn) throw new Error('unavailable');
    const child = new EventEmitter();
    child.stdin = new EventEmitter();
    child.stdin.end = (payload) => {
      calls.push({ command, args, options, payload: JSON.parse(payload) });
      queueMicrotask(() => child.emit('close', 0));
    };
    return child;
  };
  const code = stripTypeScriptTypes(readFileSync(join(__dirname, 'agentqueue.ts'), 'utf8'), { mode: 'strip' })
    .replace(/^import .*;$/gm, '')
    .replace('export default function', 'function install');
  const handlers = {};
  const pi = { registerCommand() {}, on(event, handler) { handlers[event] = handler; } };
  const ctx = { cwd: '/workspace/first', hasUI: false, sessionManager: { getSessionId: () => 'session-123' } };
  const sandbox = { spawn, fs: { watch: () => Object.assign(new EventEmitter(), { close() {} }) },
    mkdir: async () => {}, readdir: async () => [], homedir, join, resolve,
    process: { env: { AGENTQUEUE_ROOT: '/queue root' } },
    setTimeout: () => 1, clearTimeout() {} };
  vm.createContext(sandbox);
  vm.runInContext(`${code}\nthis.install = install`, sandbox);
  sandbox.install(pi);
  await handlers.session_start({}, ctx);
  assert.deepEqual(Array.from(calls[0].args), ['register', '--agent', 'pi', '--to', 'pi:session-123', '--quiet']);
  assert.equal(calls[0].payload.cwd, ctx.cwd);
  assert.equal(calls[0].payload.session_id, 'session-123');
  assert.equal(calls[0].options.env.AGENTQUEUE_ROOT, '/queue root');
  assert.ok(calls[0].options.timeout <= 1000);
  assert.equal(calls[0].options.killSignal, "SIGKILL");
  ctx.cwd = '/workspace/second';
  await handlers.before_agent_start({}, ctx);
  await handlers.agent_settled({}, ctx);
  assert.equal(calls.at(-1).payload.cwd, '/workspace/second');
  assert.equal(calls.at(-1).payload.hook_event_name, 'agent_settled');
  failSpawn = true;
  await handlers.agent_settled({}, ctx);
  failSpawn = false;
  await handlers.session_shutdown();
  assert.equal(calls.at(-1).args[0], 'unregister');
});
