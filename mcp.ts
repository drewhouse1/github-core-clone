// Model Context Protocol over streamable HTTP (JSON-RPC 2.0). Supports the
// 2026-07-28 era via server/discover and the legacy handshake via initialize.

import { issueJson, mergePullRequest, pullJson, repoJson } from './rest.ts';
import { type Store } from './store.ts';

export interface McpTool {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, unknown>; required: string[] };
}

const TOOLS: McpTool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        name: { type: 'string', description: 'The name of the repository.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        full_name: { type: 'string', description: 'The full name owner/repo of the repository.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the issue.' },
        body: { type: 'string', description: 'The body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        state: { type: 'string', description: 'Issue state filter: open, closed, or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the pull request.' },
        body: { type: 'string', description: 'The body of the pull request.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner login of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

// The tools list is built once so two tools/list calls are byte-identical.
const TOOLS_LIST_JSON = JSON.stringify({ tools: TOOLS });

function norm(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]/g, '');
}

function findTool(name: string): McpTool | undefined {
  const n = norm(name);
  for (const t of TOOLS) {
    if (norm(t.name) === n) return t;
  }
  for (const t of TOOLS) {
    if (n.endsWith(norm(t.name))) return t;
  }
  return undefined;
}

/** Map the suite's documented argument names onto the tool's schema. */
function mapArgs(tool: McpTool, args: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const props = Object.keys(tool.inputSchema.properties);
  const byNorm = new Map<string, string>();
  for (const p of props) byNorm.set(norm(p), p);
  const synonyms: Record<string, string[]> = {
    owner: ['owner', 'owner_login', 'org', 'organization', 'organisation', 'user', 'user_login', 'login', 'account', 'namespace', 'owner_name'],
    repo: ['repo', 'repository', 'repository_name', 'repo_name', 'repo_slug', 'project', 'project_name'],
    title: ['title', 'issue_title', 'pr_title', 'pull_request_title', 'subject'],
    body: ['body', 'issue_body', 'description', 'content', 'text', 'comment'],
    head: ['head', 'head_ref', 'head_branch', 'pull_request_head', 'pr_head', 'source', 'source_ref', 'source_branch', 'from', 'from_branch', 'branch'],
    base: ['base', 'base_ref', 'base_branch', 'pull_request_base', 'pr_base', 'target', 'target_ref', 'target_branch', 'into', 'into_branch'],
    pull_number: ['pull_number', 'pull_request_number', 'pr_number', 'number', 'issue_number', 'index'],
    name: ['name', 'repo', 'repository', 'repository_name', 'repo_name'],
    description: ['description', 'about', 'summary', 'body'],
    private: ['private', 'is_private'],
    state: ['state', 'status'],
  };
  // First pass: exact normalized matches.
  for (const [k, v] of Object.entries(args)) {
    const prop = byNorm.get(norm(k));
    if (prop !== undefined) out[prop] = v;
  }
  // Second pass: synonyms for properties still missing.
  for (const prop of props) {
    if (out[prop] !== undefined) continue;
    const syns = synonyms[prop] ?? [prop];
    for (const s of syns) {
      const from = Object.keys(args).find((k) => norm(k) === norm(s));
      if (from !== undefined) {
        out[prop] = args[from];
        break;
      }
    }
  }
  // full_name fallback for owner/repo.
  if ((out.owner === undefined || out.repo === undefined) && typeof args.full_name === 'string') {
    const parts = (args.full_name as string).split('/');
    if (parts.length === 2) {
      if (out.owner === undefined) out.owner = parts[0];
      if (out.repo === undefined) out.repo = parts[1];
    }
  }
  if ((out.owner === undefined || out.repo === undefined) && typeof args.repo_full_name === 'string') {
    const parts = (args.repo_full_name as string).split('/');
    if (parts.length === 2) {
      if (out.owner === undefined) out.owner = parts[0];
      if (out.repo === undefined) out.repo = parts[1];
    }
  }
  return out;
}

interface RpcResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

function rpcOk(id: unknown, result: unknown): RpcResult {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}

function rpcErr(id: unknown, code: number, message: string, status = 200): RpcResult {
  return { status, body: { jsonrpc: '2.0', id, error: { code, message } } };
}

function toolResult(data: unknown): { content: { type: string; text: string }[]; structuredContent: unknown; isError: boolean } {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data, isError: false };
}

function toolError(message: string): { content: { type: string; text: string }[]; isError: boolean } {
  return { content: [{ type: 'text', text: message }], isError: true };
}

/** Execute one tools/call. Returns an RpcResult (200 with result, or error). */
function callTool(store: Store, tool: McpTool, rawArgs: Record<string, unknown>): RpcResult | { status: number; body: unknown } {
  const args = mapArgs(tool, rawArgs);
  for (const req of tool.inputSchema.required) {
    if (args[req] === undefined || args[req] === null || args[req] === '') {
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Missing required argument: ${req}`) } };
    }
  }
  const owner = typeof args.owner === 'string' && args.owner !== '' ? args.owner : store.viewerLogin;
  const repoName = typeof args.repo === 'string' ? args.repo : typeof args.name === 'string' ? args.name : '';
  switch (tool.name) {
    case 'create_repo': {
      const name = String(args.name ?? '');
      if (name === '') return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError('Missing required argument: name') } };
      const existing = store.getRepo(owner, name);
      const repo = existing ?? store.createRepo(owner, name, typeof args.description === 'string' ? args.description : '', args.private === true);
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult(repoJson(store, repo)) } };
    }
    case 'get_repo': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Repository ${owner}/${repoName} not found`) } };
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult(repoJson(store, repo)) } };
    }
    case 'create_issue': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Repository ${owner}/${repoName} not found`) } };
      store.issueIdSeq += 1;
      repo.issueSeq += 1;
      const seq = store.nextSeq();
      const issue = { id: store.issueIdSeq, number: repo.issueSeq, title: String(args.title ?? ''), body: typeof args.body === 'string' ? args.body : '', state: 'open' as const, labels: [] as string[], seq, comments: [] as { id: number; body: string; seq: number }[] };
      repo.issues.set(issue.number, issue);
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult(issueJson(store, repo, issue)) } };
    }
    case 'list_issues': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Repository ${owner}/${repoName} not found`) } };
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = [...repo.issues.values()];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list.sort((a, b) => b.seq - a.seq);
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult(list.map((i) => issueJson(store, repo, i))) } };
    }
    case 'create_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Repository ${owner}/${repoName} not found`) } };
      const head = String(args.head ?? '');
      const base = String(args.base ?? repo.defaultBranch);
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError('head or base branch not found') } };
      const { files, mergeBaseSha } = store.computePull(repo, headSha, baseSha);
      store.pullIdSeq += 1;
      repo.pullSeq += 1;
      const seq = store.nextSeq();
      const pull = {
        id: store.pullIdSeq,
        number: repo.pullSeq,
        title: String(args.title ?? ''),
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open' as const,
        merged: false,
        mergedAt: null as string | null,
        mergeCommitSha: null as string | null,
        headRef: head,
        baseRef: base,
        headSha,
        baseSha,
        mergeBaseSha,
        files,
        seq,
      };
      repo.pulls.set(pull.number, pull);
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult(pullJson(store, repo, pull)) } };
    }
    case 'merge_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Repository ${owner}/${repoName} not found`) } };
      const pull = repo.pulls.get(Number(args.pull_number));
      if (!pull) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError('Pull request not found') } };
      const result = mergePullRequest(store, repo, pull);
      if (!result.ok) return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(result.message ?? 'merge failed') } };
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolResult({ number: pull.number, merged: true, sha: result.sha, state: 'closed' }) } };
    }
    default:
      return { status: 200, body: { jsonrpc: '2.0', id: 0, result: toolError(`Unknown tool ${tool.name}`) } };
  }
}

/**
 * Handle one MCP POST. `rawBody` is the request bytes; returns status + body
 * (body is a JSON value, or null for a 202 notification ack).
 */
export function handleMcp(store: Store, rawBody: Buffer): RpcResult {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8'));
  } catch {
    // Try to recover the id for the parse-error response.
    const text = rawBody.toString('utf8');
    const m = /"id"\s*:\s*("([^"]*)"|(-?\d+))/.exec(text);
    let id: unknown = null;
    if (m) id = m[2] !== undefined ? m[2] : Number(m[3]);
    return { status: 400, body: { jsonrpc: '2.0', id, error: { code: -32700, message: 'Parse error' } } };
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } };
  }
  const req = parsed as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
  const id = req.id === undefined ? null : req.id;
  const method = typeof req.method === 'string' ? req.method : '';
  const params = (req.params ?? {}) as Record<string, unknown>;

  // Notification (no id): acknowledge, no body.
  if (req.id === undefined) {
    return { status: 202, body: null };
  }

  if (method === 'server/discover') {
    const result = {
      supportedVersions: ['2026-07-28'],
      capabilities: { tools: {} },
      serverInfo: { name: 'github-forge', version: '1.0.0' },
      instructions: 'A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.',
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'github-forge', version: '1.0.0' } },
    };
    return { ...rpcOk(id, result), headers: { 'mcp-session-id': 'forge-1' } };
  }

  if (method === 'initialize') {
    const offered = typeof params.protocolVersion === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(params.protocolVersion) ? params.protocolVersion : '2026-07-28';
    const result = {
      protocolVersion: offered,
      capabilities: { tools: {} },
      serverInfo: { name: 'github-forge', version: '1.0.0' },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'github-forge', version: '1.0.0' } },
    };
    return { ...rpcOk(id, result), headers: { 'mcp-session-id': 'forge-1' } };
  }

  if (method === 'tools/list') {
    // Byte-identical every call: parse the prebuilt JSON once.
    return { status: 200, body: { jsonrpc: '2.0', id, result: JSON.parse(TOOLS_LIST_JSON) } };
  }

  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    if (typeof args !== 'object' || args === null || Array.isArray(args)) {
      return rpcErr(id, -32602, 'Invalid params: arguments must be an object');
    }
    const tool = findTool(name);
    if (!tool) return rpcErr(id, -32602, `Unknown tool: ${name}`);
    const res = callTool(store, tool, args);
    // callTool stamps id 0; rewrite to the caller's id.
    const body = res.body as { jsonrpc: string; id: unknown; result?: unknown; error?: unknown };
    return { status: res.status, body: { jsonrpc: '2.0', id, ...(body.result !== undefined ? { result: body.result } : {}), ...(body.error !== undefined ? { error: body.error } : {}) } };
  }

  if (method === 'ping') return rpcOk(id, {});
  if (method === 'resources/list') return rpcOk(id, { resources: [] });
  if (method === 'prompts/list') return rpcOk(id, { prompts: [] });

  return rpcErr(id, -32601, `Method not found: ${method}`);
}
