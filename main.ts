// The forge's entrypoint: launch the vendored Go core, tell the suite
// where it listens, then wait until the suite says the trial is over.
//
// The suite (the "env") lives at MAKEFUN_ENV_URL. The lifecycle is:
//   1. copy the Go core (go/forge) to a fresh temp dir and make it
//      executable — /bot is read-only and the shipped binary may have lost
//      its exec bit in transit — then spawn it with this process's
//      environment (so it sees MAKEFUN_SHOP_TOKEN),
//   2. read the core's `FORGE_LISTEN <port>` line from its stdout,
//   3. POST {"port": <bound port>} to <env>/register,
//   4. poll GET <env>/done until it answers {"done": true},
//   5. kill the core and exit 0.
// Without MAKEFUN_ENV_URL there is nothing to register with, so the core
// keeps serving locally until this process is stopped.
// The core serves every surface; this file only owns the lifecycle and
// does nothing on the hot path. No wall clock or randomness is used
// anywhere here, so two runs of the same trial behave identically.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmod, copyFile, mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// First pause between retries, in milliseconds. The pause doubles after
// each failed attempt up to the caps below, so a briefly-unready env is
// given room without hammering it.
const FIRST_PAUSE_MS = 100;
const REGISTER_PAUSE_CAP_MS = 2000;
const DONE_PAUSE_CAP_MS = 1000;

// How many times registration is attempted before giving up. At the cap
// this covers several minutes of env start-up time.
const REGISTER_ATTEMPTS = 120;

function pause(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

function nextPause(current: number, cap: number): number {
  const doubled = current * 2;
  return doubled > cap ? cap : doubled;
}

// POST a JSON body and return the response status, or -1 when the request
// itself could not be made (connection refused, env not up yet, ...).
async function postJson(url: string, body: unknown): Promise<number> {
  try {
    const response = await fetch(url, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    return response.status;
  } catch {
    return -1;
  }
}

// GET a URL and parse its JSON body. Returns null when the request fails
// or the body is not JSON, so the caller can retry.
async function getJson(url: string): Promise<Record<string, unknown> | null> {
  try {
    const response = await fetch(url, { method: 'GET' });
    if (!response.ok) return null;
    const parsed: unknown = await response.json();
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null;
    return parsed as Record<string, unknown>;
  } catch {
    return null;
  }
}

// Register the bound port with the env, retrying with backoff. A 200
// registers us; a 409 means a registration already stands, which is also
// fine because this process registers only once. Returns true on success.
async function register(envUrl: string, port: number): Promise<boolean> {
  let wait = FIRST_PAUSE_MS;
  let lastStatus = -1;
  for (let attempt = 0; attempt < REGISTER_ATTEMPTS; attempt += 1) {
    const status = await postJson(`${envUrl}/register`, { port });
    lastStatus = status;
    if (status === 200 || status === 201 || status === 409) return true;
    await pause(wait);
    wait = nextPause(wait, REGISTER_PAUSE_CAP_MS);
  }
  // -1 means the request itself never reached the env (connection refused
  // or the env not up yet); any other value is the last HTTP status seen.
  console.error(`registration failed after ${REGISTER_ATTEMPTS} attempts (last status: ${lastStatus})`);
  return false;
}

// Poll the env's done flag until it turns true. Each poll is a short,
// self-contained request; nothing is held open while waiting.
async function waitForDone(envUrl: string): Promise<void> {
  let wait = FIRST_PAUSE_MS;
  for (;;) {
    const body = await getJson(`${envUrl}/done`);
    if (body !== null && body.done === true) return;
    await pause(wait);
    wait = nextPause(wait, DONE_PAUSE_CAP_MS);
  }
}

// Start the Go core and return the child process plus the port it bound.
async function launchGoCore(): Promise<{ child: ChildProcess; port: number }> {
  const dir = await mkdtemp(join(tmpdir(), 'forge-'));
  const binPath = join(dir, 'forge');
  await copyFile(new URL('./go/forge', import.meta.url), binPath);
  await chmod(binPath, 0o755);
  const child = spawn(binPath, [], { stdio: ['ignore', 'pipe', 'inherit'], env: process.env });
  const port = await new Promise<number>((resolve, reject) => {
    let buffer = '';
    let settled = false;
    child.stdout!.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const newline = buffer.indexOf('\n');
      if (newline < 0) return;
      const line = buffer.slice(0, newline).trim();
      const match = /^FORGE_LISTEN (\d+)$/.exec(line);
      if (match === null) {
        if (!settled) {
          settled = true;
          reject(new Error(`unexpected Go core output: ${line}`));
        }
        return;
      }
      if (!settled) {
        settled = true;
        resolve(Number(match[1]));
      }
    });
    child.once('close', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`Go core exited before listening (code ${code})`));
      }
    });
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
  });
  return { child, port };
}

async function main(): Promise<void> {
  const envUrl = process.env.MAKEFUN_ENV_URL;
  const { child, port } = await launchGoCore();
  // If this launcher is killed, take the core down with it instead of
  // orphaning it.
  process.once('SIGINT', () => {
    child.kill();
    process.exit(130);
  });
  process.once('SIGTERM', () => {
    child.kill();
    process.exit(143);
  });

  if (envUrl === undefined || envUrl === '') {
    // Local run: serve on the core's port for testing, no env to register with.
    process.stdout.write(`[forge] listening on http://127.0.0.1:${port} (no MAKEFUN_ENV_URL; local mode)\n`);
    await new Promise<void>(() => {});
    return;
  }

  const registered = await register(envUrl, port);
  if (!registered) {
    console.error(`could not register port ${port} with ${envUrl}`);
    child.kill();
    process.exit(1);
  }

  await waitForDone(envUrl);
  child.kill();
  await new Promise<void>((resolve) => child.once('close', () => resolve()));
  process.exit(0);
}

await main();
