// The forge's HTTP server: routing, auth, JSON plumbing. Surfaces:
//   GET  /healthz                     200 {"ok":true} (no auth)
//   /{owner}/{repo}.git/...           git smart HTTP (no auth check)
//   /api/v3/...                       GitHub REST v3 (Bearer required)
//   POST /api/graphql                 GraphQL v4 (Bearer required)
//   POST /mcp                         MCP streamable HTTP (Bearer required)
//   anything else                     404 {"message":"Not Found"}

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { scenarioViewer } from './scenario.ts';
import { advertiseV0, advertiseV2, handleReceivePack, handleUploadPack } from './git.ts';
import { handleRest } from './rest.ts';
import { executeGraphQL } from './gql.ts';
import { handleMcp } from './mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

const store = new Store(scenarioViewer());

function expectedToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function requestId(): string {
  store.reqSeq += 1;
  return `req-${store.reqSeq}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const text = body === null ? '' : `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': requestId(),
    ...headers,
  });
  res.end(text);
}

function sendRaw(res: ServerResponse, status: number, body: Buffer, contentType: string, headers: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': requestId(),
    ...headers,
  });
  res.end(body);
}

function authOk(req: IncomingMessage): boolean {
  const expected = expectedToken();
  if (expected === '') return true;
  const got = req.headers.authorization ?? '';
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

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        await handle(req, res);
      } catch (err) {
        if (!res.headersSent) {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
        } else {
          res.destroy();
        }
      }
    })();
  });
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const method = req.method ?? 'GET';
  let url: URL;
  try {
    url = new URL(req.url ?? '/', 'http://entry');
  } catch {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const path = url.pathname;
  const segs = path.split('/').filter((s) => s !== '');

  // Health probe: no auth.
  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
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
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      const isV2 = req.headers['git-protocol'] === 'version=2';
      if (service === 'git-upload-pack' && isV2) {
        sendRaw(res, 200, advertiseV2(), 'application/x-git-upload-pack-advertisement');
      } else {
        sendRaw(res, 200, advertiseV0(store, repo, service as 'git-upload-pack' | 'git-receive-pack'), `application/x-${service}-advertisement`);
      }
      return;
    }
    if (sub === 'git-upload-pack' && method === 'POST') {
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      const body = await readBody(req);
      const isV2 = req.headers['git-protocol'] === 'version=2';
      const out = handleUploadPack(store, repo, body, isV2);
      sendRaw(res, out.status, out.body, out.contentType);
      return;
    }
    if (sub === 'git-receive-pack' && method === 'POST') {
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      const body = await readBody(req);
      const out = handleReceivePack(store, repo, body);
      sendRaw(res, out.status, out.body, out.contentType);
      return;
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }

  // Authenticated surfaces.
  if (path === '/api/graphql' || path === '/mcp' || path === '/api/v3' || path.startsWith('/api/v3/')) {
    if (!authOk(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
  }

  // API root.
  if (method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) {
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }

  // GraphQL.
  if (path === '/api/graphql') {
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    const raw = await readBody(req);
    let parsed: unknown;
    try {
      parsed = raw.length === 0 ? {} : JSON.parse(raw.toString('utf8'));
    } catch {
      sendJson(res, 400, { errors: [{ message: 'Parse error' }] });
      return;
    }
    const result = executeGraphQL(store, parsed);
    sendJson(res, 200, result);
    return;
  }

  // MCP.
  if (path === '/mcp') {
    if (method === 'DELETE') {
      res.writeHead(204, { 'x-github-request-id': requestId() });
      res.end();
      return;
    }
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    const raw = await readBody(req);
    const out = handleMcp(store, raw);
    if (out.body === null) {
      res.writeHead(out.status, { 'x-github-request-id': requestId(), ...(out.headers ?? {}) });
      res.end();
      return;
    }
    sendJson(res, out.status, out.body, out.headers ?? {});
    return;
  }

  // REST v3.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    const restSegs = path === '/api/v3' ? [] : path.slice('/api/v3/'.length).split('/').filter((s) => s !== '');
    let body: unknown = {};
    if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
      const raw = await readBody(req);
      if (raw.length > 0) {
        try {
          body = JSON.parse(raw.toString('utf8'));
        } catch {
          sendJson(res, 400, { message: 'Invalid JSON' });
          return;
        }
      }
    }
    const baseUrl = `http://${req.headers.host ?? 'entry'}`;
    const out = handleRest({ store, query: url.searchParams, body, baseUrl }, method, restSegs);
    if (out.status === 204) {
      res.writeHead(204, { 'x-github-request-id': requestId(), ...(out.headers ?? {}) });
      res.end();
      return;
    }
    sendJson(res, out.status, out.body, out.headers ?? {});
    return;
  }

  sendJson(res, 404, { message: 'Not Found' });
}
