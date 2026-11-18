// The forge's HTTP server: routing, auth, JSON plumbing. Surfaces:
//   GET  /healthz                     200 {"ok":true} (no auth)
//   /{owner}/{repo}.git/...           git smart HTTP (no auth check)
//   /api/v3/...                       GitHub REST v3 (Bearer required)
//   POST /api/graphql                 GraphQL v4 (Bearer required)
//   POST /mcp                         MCP streamable HTTP (Bearer required)
//   anything else                     404 {"message":"Not Found"}
//
// The router works on plain request values and returns a finished response
// value, so the same routing can sit behind either transport: adapters only
// move bytes (node:http below; the raw-socket server in createRawApp).
// GET answers under /api/v3 are memoized in dispatch() and keyed by the
// store version, so one mutation retires every cached answer at once.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { createServer as createNetServer, type Socket } from 'node:net';
import { Store } from './store.ts';
import { scenarioViewer } from './scenario.ts';
import { advertiseV0, advertiseV2, handleReceivePack, handleUploadPack } from './git.ts';
import { handleRest } from './rest.ts';
import { executeGraphQL } from './gql.ts';
import { handleMcp } from './mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

const store = new Store(scenarioViewer());

/** The server surface main.ts uses: listen on an ephemeral port, report the
 * bound port, shut down. Both transports below satisfy it. */
export interface ForgeServer {
  on(event: 'error', fn: (err: Error) => void): unknown;
  once(event: 'error', fn: (err: Error) => void): unknown;
  listen(port: number, cb: () => void): void;
  address(): { port: number } | string | null;
  close(cb?: () => void): void;
}

/** One request, already fully read: method, raw target, headers, body. */
export interface ForgeRequest {
  method: string;
  url: string;
  headers: Record<string, string | string[] | undefined>;
  body: Buffer;
}

/** One finished response: status, exact bytes, content type, extra headers.
 * The request id and content length are added by the transport at send time,
 * never stored here, so a memoized answer can be re-sent with a fresh id. */
export interface RouteResult {
  status: number;
  body: Buffer;
  contentType: string;
  headers?: Record<string, string>;
}

function jsonResult(status: number, body: unknown, headers: Record<string, string> = {}): RouteResult {
  const text = body === null ? '' : `${JSON.stringify(body)}\n`;
  return { status, body: Buffer.from(text, 'utf8'), contentType: JSON_TYPE, headers };
}

function textResult(status: number, text: string, headers: Record<string, string> = {}): RouteResult {
  return { status, body: Buffer.from(`${text}\n`, 'utf8'), contentType: JSON_TYPE, headers };
}

function rawResult(status: number, body: Buffer, contentType: string, headers: Record<string, string> = {}): RouteResult {
  return { status, body, contentType, headers };
}

function emptyResult(status: number, headers: Record<string, string> = {}): RouteResult {
  return { status, body: Buffer.alloc(0), contentType: JSON_TYPE, headers };
}

function expectedToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function requestId(): string {
  store.reqSeq += 1;
  return `req-${store.reqSeq}`;
}

function authOk(headers: Record<string, string | string[] | undefined>): boolean {
  const expected = expectedToken();
  if (expected === '') return true;
  const got = headers.authorization ?? '';
  return got === `Bearer ${expected}`;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

// ---------------------------------------------------------------- GET memo

const GET_MEMO_MAX_ENTRIES = 1024;
const GET_MEMO_MAX_BODY = 1024 * 1024;

interface MemoEntry {
  status: number;
  body: Buffer;
  contentType: string;
  headers: Record<string, string>;
}

const getMemo = new Map<string, MemoEntry>();

function memoKey(req: ForgeRequest, authorized: boolean): string {
  return `${store.version}\n${authorized ? '1' : '0'}\n${req.headers.host ?? ''}\n${req.url}`;
}

/** Route one request, memoizing GET answers under /api/v3. A memo hit returns
 * the exact stored bytes; the transport mints a fresh request id on send.
 * Every non-GET request bumps the store version when it settles, which
 * retires all memoized answers at once (over-invalidation is safe: a stale
 * answer can never be served, it just costs one re-render). */
async function dispatch(req: ForgeRequest): Promise<RouteResult> {
  const isGet = req.method === 'GET';
  const memoable = isGet && req.url.startsWith('/api/v3/');
  let key = '';
  if (memoable) {
    key = memoKey(req, authOk(req.headers));
    const hit = getMemo.get(key);
    if (hit !== undefined) {
      return { status: hit.status, body: hit.body, contentType: hit.contentType, headers: hit.headers };
    }
  }
  try {
    const result = await handle(req);
    if (memoable && result.body.length <= GET_MEMO_MAX_BODY) {
      if (getMemo.size >= GET_MEMO_MAX_ENTRIES) {
        const oldest = getMemo.keys().next().value as string | undefined;
        if (oldest !== undefined) getMemo.delete(oldest);
      }
      getMemo.set(key, { status: result.status, body: result.body, contentType: result.contentType, headers: result.headers ?? {} });
    }
    return result;
  } finally {
    if (!isGet) store.bumpVersion();
  }
}

// ---------------------------------------------------------------- routing

async function handle(req: ForgeRequest): Promise<RouteResult> {
  const method = req.method;
  let url: URL;
  try {
    url = new URL(req.url, 'http://entry');
  } catch {
    return jsonResult(404, { message: 'Not Found' });
  }
  const path = url.pathname;
  const segs = path.split('/').filter((s) => s !== '');

  // Health probe: no auth.
  if (method === 'GET' && path === '/healthz') {
    return jsonResult(200, { ok: true });
  }

  // Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack
  if (segs.length >= 2 && segs[1].endsWith('.git')) {
    const owner = decodeURIComponent(segs[0]);
    const repoName = decodeURIComponent(segs[1].slice(0, -'.git'.length));
    const sub = segs.slice(2).join('/');
    const repo = store.getRepo(owner, repoName);
    if (sub === 'info/refs') {
      const service = url.searchParams.get('service') ?? '';
      if (method !== 'GET' || !repo || (service !== 'git-upload-pack' && service !== 'git-receive-pack')) {
        return jsonResult(404, { message: 'Not Found' });
      }
      const isV2 = req.headers['git-protocol'] === 'version=2';
      if (service === 'git-upload-pack' && isV2) {
        return rawResult(200, advertiseV2(), 'application/x-git-upload-pack-advertisement');
      }
      return rawResult(200, advertiseV0(store, repo, service as 'git-upload-pack' | 'git-receive-pack'), `application/x-${service}-advertisement`);
    }
    if (sub === 'git-upload-pack' && method === 'POST') {
      if (!repo) {
        return jsonResult(404, { message: 'Not Found' });
      }
      const out = handleUploadPack(store, repo, req.body, req.headers['git-protocol'] === 'version=2');
      return rawResult(out.status, out.body, out.contentType);
    }
    if (sub === 'git-receive-pack' && method === 'POST') {
      if (!repo) {
        return jsonResult(404, { message: 'Not Found' });
      }
      const out = handleReceivePack(store, repo, req.body);
      return rawResult(out.status, out.body, out.contentType);
    }
    return jsonResult(404, { message: 'Not Found' });
  }

  // Authenticated surfaces.
  if (path === '/api/graphql' || path === '/mcp' || path === '/api/v3' || path.startsWith('/api/v3/')) {
    if (!authOk(req.headers)) {
      return jsonResult(401, { message: 'Requires authentication' });
    }
  }

  // API root.
  if (method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) {
    return jsonResult(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }

  // GraphQL.
  if (path === '/api/graphql') {
    if (method !== 'POST') {
      return jsonResult(404, { message: 'Not Found' });
    }
    const raw = req.body;
    let parsed: unknown;
    try {
      parsed = raw.length === 0 ? {} : JSON.parse(raw.toString('utf8'));
    } catch {
      return jsonResult(400, { errors: [{ message: 'Parse error' }] });
    }
    const result = executeGraphQL(store, parsed);
    return jsonResult(200, result);
  }

  // MCP.
  if (path === '/mcp') {
    if (method === 'DELETE') {
      return emptyResult(204);
    }
    if (method !== 'POST') {
      return jsonResult(404, { message: 'Not Found' });
    }
    const out = handleMcp(store, req.body);
    if (out.body === null) {
      return emptyResult(out.status, out.headers ?? {});
    }
    return jsonResult(out.status, out.body, out.headers ?? {});
  }

  // REST v3.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    const restSegs = path === '/api/v3' ? [] : path.slice('/api/v3/'.length).split('/').filter((s) => s !== '');
    let body: unknown = {};
    if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
      const raw = req.body;
      if (raw.length > 0) {
        try {
          body = JSON.parse(raw.toString('utf8'));
        } catch {
          return jsonResult(400, { message: 'Invalid JSON' });
        }
      }
    }
    const baseUrl = `http://${req.headers.host ?? 'entry'}`;
    const out = handleRest({ store, query: url.searchParams, body, baseUrl }, method, restSegs);
    if (out.status === 204) {
      return emptyResult(204, out.headers ?? {});
    }
    if (out.text !== undefined) {
      return textResult(out.status, out.text, out.headers ?? {});
    }
    return jsonResult(out.status, out.body, out.headers ?? {});
  }

  return jsonResult(404, { message: 'Not Found' });
}

// ---------------------------------------------------------------- node:http transport

// ---------------------------------------------------------------- node:http transport

function sendResult(res: ServerResponse, result: RouteResult): void {
  if (result.status === 204) {
    res.writeHead(204, { 'x-github-request-id': requestId(), ...(result.headers ?? {}) });
    res.end();
    return;
  }
  res.writeHead(result.status, {
    'content-type': result.contentType,
    'content-length': result.body.length,
    'x-github-request-id': requestId(),
    ...(result.headers ?? {}),
  });
  res.end(result.body);
}

function createHttpApp(): ForgeServer {
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const body = await readBody(req);
        const result = await dispatch({
          method: req.method ?? 'GET',
          url: req.url ?? '/',
          headers: req.headers,
          body,
        });
        sendResult(res, result);
      } catch (err) {
        if (!res.headersSent) {
          sendResult(res, jsonResult(500, { message: err instanceof Error ? err.message : 'Internal Server Error' }));
        } else {
          res.destroy();
        }
      }
    })();
  });
  return server as unknown as ForgeServer;
}

// ---------------------------------------------------------------- raw-socket HTTP/1.1 transport

const REASONS: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  409: 'Conflict',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

interface RawConn {
  socket: Socket;
  buf: Buffer;
  busy: boolean;
  closed: boolean;
  continueSent: boolean;
}

type ParseOut =
  | { kind: 'need-more'; headersDone: boolean; expectContinue: boolean }
  | { kind: 'bad' }
  | { kind: 'request'; req: ForgeRequest; consumed: number; keepAlive: boolean };

function keepAliveFor(version: string, headers: Record<string, string>): boolean {
  const conn = (headers.connection ?? '').toLowerCase();
  if (version === 'HTTP/1.0') return conn === 'keep-alive';
  return conn !== 'close';
}

/** Parse one chunked body starting at bodyStart; null when incomplete. */
function parseChunked(buf: Buffer, bodyStart: number): { body: Buffer; consumed: number } | 'bad' | null {
  const chunks: Buffer[] = [];
  let pos = bodyStart;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', pos);
    if (lineEnd === -1) return null;
    const sizeText = buf.subarray(pos, lineEnd).toString('ascii').trim().split(';')[0];
    const size = parseInt(sizeText, 16);
    if (Number.isNaN(size) || size < 0) return 'bad';
    const dataStart = lineEnd + 2;
    if (size === 0) {
      // Trailers, if any, run to the first empty line.
      let t = dataStart;
      for (;;) {
        const e = buf.indexOf('\r\n', t);
        if (e === -1) return null;
        if (e === t) return { body: Buffer.concat(chunks), consumed: e + 2 };
        t = e + 2;
      }
    }
    if (buf.length < dataStart + size + 2) return null;
    chunks.push(Buffer.from(buf.subarray(dataStart, dataStart + size)));
    pos = dataStart + size + 2;
  }
}

/** Parse the first request out of buf, if complete. */
function parseRequest(buf: Buffer): ParseOut {
  const headEnd = buf.indexOf('\r\n\r\n');
  if (headEnd === -1) {
    if (buf.length > 65536) return { kind: 'bad' };
    return { kind: 'need-more', headersDone: false, expectContinue: false };
  }
  const lines = buf.subarray(0, headEnd).toString('ascii').split('\r\n');
  const first = lines[0].split(' ');
  if (first.length !== 3 || !first[2].startsWith('HTTP/')) return { kind: 'bad' };
  const method = first[0];
  const url = first[1];
  const version = first[2];
  const headers: Record<string, string> = {};
  for (let i = 1; i < lines.length; i += 1) {
    const colon = lines[i].indexOf(':');
    if (colon === -1) return { kind: 'bad' };
    const name = lines[i].slice(0, colon).trim().toLowerCase();
    const value = lines[i].slice(colon + 1).trim();
    headers[name] = headers[name] === undefined ? value : `${headers[name]}, ${value}`;
  }
  const expectContinue = (headers.expect ?? '').toLowerCase() === '100-continue';
  const bodyStart = headEnd + 4;
  const te = headers['transfer-encoding'] ?? '';
  if (te.toLowerCase().includes('chunked')) {
    const parsed = parseChunked(buf, bodyStart);
    if (parsed === null) return { kind: 'need-more', headersDone: true, expectContinue };
    if (parsed === 'bad') return { kind: 'bad' };
    return { kind: 'request', req: { method, url, headers, body: parsed.body }, consumed: parsed.consumed, keepAlive: keepAliveFor(version, headers) };
  }
  const cl = headers['content-length'];
  if (cl !== undefined) {
    const len = parseInt(cl, 10);
    if (Number.isNaN(len) || len < 0) return { kind: 'bad' };
    if (buf.length < bodyStart + len) return { kind: 'need-more', headersDone: true, expectContinue };
    return { kind: 'request', req: { method, url, headers, body: Buffer.from(buf.subarray(bodyStart, bodyStart + len)) }, consumed: bodyStart + len, keepAlive: keepAliveFor(version, headers) };
  }
  return { kind: 'request', req: { method, url, headers, body: Buffer.alloc(0) }, consumed: bodyStart, keepAlive: keepAliveFor(version, headers) };
}

/** Send one finished response as a single socket write: status line +
 * headers + body in one Buffer.concat. A 204 carries no body, so it sends
 * no content-type or content-length. */
function writeRawResult(socket: Socket, result: RouteResult, keepAlive: boolean): void {
  const reason = REASONS[result.status] ?? 'OK';
  const lines: string[] = [`HTTP/1.1 ${result.status} ${reason}`];
  if (result.status !== 204) {
    lines.push(`content-type: ${result.contentType}`);
    lines.push(`content-length: ${result.body.length}`);
  }
  lines.push(`x-github-request-id: ${requestId()}`);
  lines.push(`connection: ${keepAlive ? 'keep-alive' : 'close'}`);
  for (const [k, v] of Object.entries(result.headers ?? {})) lines.push(`${k}: ${v}`);
  const head = Buffer.from(`${lines.join('\r\n')}\r\n\r\n`, 'ascii');
  socket.write(result.body.length === 0 ? head : Buffer.concat([head, result.body]));
}

/** Drain buffered requests on one connection, strictly in order: one
 * request in flight at a time, pipelined bytes handled after each reply. */
async function pump(conn: RawConn): Promise<void> {
  if (conn.busy || conn.closed) return;
  for (;;) {
    const parsed = parseRequest(conn.buf);
    if (parsed.kind === 'need-more') {
      // A client waiting on "100 Continue" gets it the moment the headers
      // are in — git waits up to a second for it before sending a pack body.
      if (parsed.headersDone && parsed.expectContinue && !conn.continueSent) {
        conn.socket.write(Buffer.from('HTTP/1.1 100 Continue\r\n\r\n', 'ascii'));
        conn.continueSent = true;
      }
      return;
    }
    if (parsed.kind === 'bad') {
      conn.socket.write(Buffer.from('HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n', 'ascii'));
      conn.socket.end();
      conn.closed = true;
      return;
    }
    conn.buf = conn.buf.subarray(parsed.consumed);
    conn.continueSent = false;
    conn.busy = true;
    let result: RouteResult;
    try {
      result = await dispatch(parsed.req);
    } catch (err) {
      result = jsonResult(500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
    }
    if (conn.closed) return;
    writeRawResult(conn.socket, result, parsed.keepAlive);
    conn.busy = false;
    if (!parsed.keepAlive) {
      conn.socket.end();
      conn.closed = true;
      return;
    }
    if (conn.buf.length === 0) return;
  }
}

function createRawApp(): ForgeServer {
  const sockets = new Set<Socket>();
  const server = createNetServer((socket) => {
    socket.setNoDelay(true);
    sockets.add(socket);
    const conn: RawConn = { socket, buf: Buffer.alloc(0), busy: false, closed: false, continueSent: false };
    socket.on('data', (chunk: Buffer) => {
      conn.buf = conn.buf.length === 0 ? chunk : Buffer.concat([conn.buf, chunk]);
      void pump(conn);
    });
    socket.on('error', () => {
      conn.closed = true;
      sockets.delete(socket);
    });
    socket.on('close', () => {
      conn.closed = true;
      sockets.delete(socket);
    });
  });
  // main.ts closes the server and then exits; destroy any idle keep-alive
  // sockets first so close() actually fires instead of waiting on them.
  const origClose = server.close.bind(server);
  (server as unknown as { close: (cb?: () => void) => void }).close = (cb?: () => void): void => {
    for (const s of sockets) s.destroy();
    origClose(cb);
  };
  return server as unknown as ForgeServer;
}

export function createApp(): ForgeServer {
  // FORGE_NETHTTP=1 selects the node:http transport (A/B comparison);
  // the raw-socket transport is the default.
  if (process.env.FORGE_NETHTTP === '1') return createHttpApp();
  return createRawApp();
}
