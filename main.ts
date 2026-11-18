// The forge's entrypoint: start the HTTP server, tell the suite where it
// listens, then wait until the suite says the trial is over.
//
// The suite (the "env") lives at MAKEFUN_ENV_URL. The lifecycle is:
//   1. bind an ephemeral port (listen(0); never 8080, which the env owns),
//   2. POST {"port": <bound port>} to <env>/register,
//   3. poll GET <env>/done until it answers {"done": true},
//   4. shut the server down and exit 0.
// Without MAKEFUN_ENV_URL there is nothing to register with, so exit 1.
// Everything the server serves comes from createApp() in app.ts; this file
// only owns the lifecycle. No wall clock or randomness is used anywhere
// here, so two runs of the same trial behave identically.

import { createApp } from './app.ts';

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

async function main(): Promise<void> {
  const envUrl = process.env.MAKEFUN_ENV_URL;
  if (envUrl === undefined || envUrl === '') {
    console.error('MAKEFUN_ENV_URL is not set; nothing to register with');
    process.exit(1);
  }

  const server = createApp();
  server.on('error', (err: Error) => {
    console.error(`server failed: ${err.message}`);
    process.exit(1);
  });

  const port = await new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        reject(new Error('server bound to no TCP port'));
        return;
      }
      resolve(address.port);
    });
  });

  const registered = await register(envUrl, port);
  if (!registered) {
    console.error(`could not register port ${port} with ${envUrl}`);
    server.close();
    process.exit(1);
  }

  await waitForDone(envUrl);
  await new Promise<void>((resolve) => {
    server.close(() => resolve());
  });
  process.exit(0);
}

await main();
