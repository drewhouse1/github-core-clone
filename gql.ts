// GitHub GraphQL v4: a small parser + executor for exactly the documents the
// suite sends (gh / github-mcp-server shapes). No dependencies.

import { type Repo, type Store } from './store.ts';

// ---------------------------------------------------------------- parser

type ArgValue =
  | { kind: 'var'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'bool'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; items: ArgValue[] }
  | { kind: 'object'; fields: Record<string, ArgValue> };

export interface Selection {
  alias: string | null;
  name: string;
  args: Record<string, ArgValue>;
  selections: Selection[] | null;
  fragmentType: string | null;
}

export interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, unknown>;
  selections: Selection[];
}

interface Token {
  kind: 'name' | 'string' | 'int' | 'float' | 'punct' | 'spread';
  value: string;
}

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === ';') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@&|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1];
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      while (j < src.length && /[0-9.eE+-]/.test(src[j])) j += 1;
      const text = src.slice(i, j);
      tokens.push({ kind: text.includes('.') || text.includes('e') || text.includes('E') ? 'float' : 'int', value: text });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in GraphQL document`);
  }
  return tokens;
}

class Parser {
  tokens: Token[];
  pos = 0;
  fragments = new Map<string, { type: string; selections: Selection[] }>();

  constructor(src: string) {
    this.tokens = tokenize(src);
  }

  peek(): Token | null {
    return this.tokens[this.pos] ?? null;
  }

  next(): Token {
    const t = this.tokens[this.pos];
    if (!t) throw new Error('unexpected end of GraphQL document');
    this.pos += 1;
    return t;
  }

  expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`expected '${v}' in GraphQL document, got ${t.value}`);
  }

  expectName(v?: string): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error(`expected a name in GraphQL document, got ${t.value}`);
    if (v !== undefined && t.value !== v) throw new Error(`expected name '${v}', got ${t.value}`);
    return t.value;
  }

  parseValue(): ArgValue {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') return { kind: 'var', name: this.expectName() };
    if (t.kind === 'string') return { kind: 'string', value: t.value };
    if (t.kind === 'int') return { kind: 'int', value: parseInt(t.value, 10) };
    if (t.kind === 'float') return { kind: 'float', value: parseFloat(t.value) };
    if (t.kind === 'punct' && t.value === '[') {
      const items: ArgValue[] = [];
      while (!(this.peek()?.kind === 'punct' && this.peek()?.value === ']')) items.push(this.parseValue());
      this.expectPunct(']');
      return { kind: 'list', items };
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: Record<string, ArgValue> = {};
      while (!(this.peek()?.kind === 'punct' && this.peek()?.value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields[name] = this.parseValue();
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'bool', value: true };
      if (t.value === 'false') return { kind: 'bool', value: false };
      if (t.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: t.value };
    }
    throw new Error(`unexpected token ${t.value} in GraphQL value`);
  }

  parseArgs(): Record<string, ArgValue> {
    const args: Record<string, ArgValue> = {};
    if (this.peek()?.kind === 'punct' && this.peek()?.value === '(') {
      this.next();
      while (!(this.peek()?.kind === 'punct' && this.peek()?.value === ')')) {
        const name = this.expectName();
        this.expectPunct(':');
        args[name] = this.parseValue();
      }
      this.expectPunct(')');
    }
    return args;
  }

  parseSelections(): Selection[] {
    this.expectPunct('{');
    const out: Selection[] = [];
    while (!(this.peek()?.kind === 'punct' && this.peek()?.value === '}')) {
      const t = this.peek();
      if (t?.kind === 'spread') {
        this.next();
        const n = this.peek();
        if (n?.kind === 'name' && n.value === 'on') {
          this.next();
          const typeName = this.expectName();
          const sels = this.parseSelections();
          out.push({ alias: null, name: '__fragment', args: {}, selections: sels, fragmentType: typeName });
        } else {
          const fragName = this.expectName();
          const frag = this.fragments.get(fragName);
          if (frag) out.push({ alias: null, name: '__fragment', args: {}, selections: frag.selections, fragmentType: frag.type });
        }
        continue;
      }
      const first = this.expectName();
      let alias: string | null = null;
      let name = first;
      if (this.peek()?.kind === 'punct' && this.peek()?.value === ':') {
        this.next();
        alias = first;
        name = this.expectName();
      }
      const args = this.parseArgs();
      let selections: Selection[] | null = null;
      if (this.peek()?.kind === 'punct' && this.peek()?.value === '{') selections = this.parseSelections();
      out.push({ alias, name, args, selections, fragmentType: null });
    }
    this.expectPunct('}');
    return out;
  }

  parseVariableDefinitions(): void {
    if (this.peek()?.kind === 'punct' && this.peek()?.value === '(') {
      this.next();
      let depth = 1;
      while (depth > 0) {
        const t = this.next();
        if (t.kind === 'punct' && t.value === '(') depth += 1;
        if (t.kind === 'punct' && t.value === ')') depth -= 1;
      }
    }
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.pos < this.tokens.length) {
      const t = this.peek() as Token;
      if (t.kind === 'name' && t.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName('on');
        const type = this.expectName();
        const sels = this.parseSelections();
        this.fragments.set(name, { type, selections: sels });
        continue;
      }
      let kind: 'query' | 'mutation' = 'query';
      let name: string | null = null;
      if (t.kind === 'punct' && t.value === '{') {
        const sels = this.parseSelections();
        ops.push({ kind, name, variables: {}, selections: sels });
        continue;
      }
      if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
        kind = t.value as 'query' | 'mutation';
        this.next();
        const n = this.peek();
        if (n?.kind === 'name') name = this.next().value;
        this.parseVariableDefinitions();
        const sels = this.parseSelections();
        ops.push({ kind, name, variables: {}, selections: sels });
        continue;
      }
      throw new Error(`unexpected token ${t.value} in GraphQL document`);
    }
    return ops;
  }
}

// ---------------------------------------------------------------- execution

export interface GqlError {
  message: string;
  path?: (string | number)[];
}

interface Obj {
  __type: string;
  [key: string]: unknown;
}

const TYPE_FIELDS: Record<string, string[]> = {
  User: ['login', 'id', 'name', 'email'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Issue: ['id', 'number', 'title', 'state', 'body'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'body'],
  IssueConnection: ['nodes', 'pageInfo'],
  PullRequestConnection: ['nodes'],
  PullRequestChangedFile: ['path'],
  PageInfo: ['hasNextPage', 'endCursor'],
  Comment: ['id', 'body'],
  CommentEdge: ['node'],
  Ref: ['name'],
  __Type: ['name', 'fields', 'enumValues'],
  __Field: ['name'],
  __EnumValue: ['name'],
};

const SEARCH_TYPE_VALUES = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'];

function resolveArg(v: ArgValue, vars: Record<string, unknown>): unknown {
  switch (v.kind) {
    case 'var':
      return vars[v.name];
    case 'string':
    case 'int':
    case 'float':
    case 'bool':
      return v.value;
    case 'null':
      return null;
    case 'enum':
      return v.value;
    case 'list':
      return v.items.map((x) => resolveArg(x, vars));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [k, val] of Object.entries(v.fields)) out[k] = resolveArg(val, vars);
      return out;
    }
  }
}

function resolveArgs(sel: Selection, vars: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(sel.args)) out[k] = resolveArg(v, vars);
  return out;
}

function cursorForNumber(n: number): string {
  return Buffer.from(`c${n}`).toString('base64');
}

function numberFromCursor(c: string | null | undefined): number | null {
  if (typeof c !== 'string' || c === '') return null;
  try {
    const text = Buffer.from(c, 'base64').toString('utf8');
    const m = /^c(\d+)$/.exec(text);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

export class GraphQLExecutor {
  store: Store;
  errors: GqlError[] = [];

  constructor(store: Store) {
    this.store = store;
  }

  repoByGlobalId(id: string): Repo | undefined {
    const m = /^R(\d+)$/.exec(id);
    if (!m) return undefined;
    const dbid = Number(m[1]);
    for (const r of this.store.repos.values()) {
      if (r.id === dbid) return r;
    }
    return undefined;
  }

  issueByGlobalId(id: string): { repo: Repo; issue: { id: number; number: number; title: string; body: string; state: string; labels: string[]; seq: number; comments: { id: number; body: string; seq: number }[] } } | undefined {
    const m = /^I(\d+)_(\d+)$/.exec(id);
    if (!m) return undefined;
    const repo = this.repoByGlobalId(`R${m[1]}`);
    if (!repo) return undefined;
    const issue = repo.issues.get(Number(m[2]));
    if (!issue) return undefined;
    return { repo, issue };
  }

  pullByGlobalId(id: string): { repo: Repo; pull: import('./store.ts').Pull } | undefined {
    const m = /^P(\d+)_(\d+)$/.exec(id);
    if (!m) return undefined;
    const repo = this.repoByGlobalId(`R${m[1]}`);
    if (!repo) return undefined;
    const pull = repo.pulls.get(Number(m[2]));
    if (!pull) return undefined;
    return { repo, pull };
  }

  repoObj(repo: Repo): Obj {
    return { __type: 'Repository', __repo: repo };
  }

  issueObj(repo: Repo, issue: { id: number; number: number; title: string; body: string; state: string; labels: string[]; seq: number; comments: { id: number; body: string; seq: number }[] }): Obj {
    return { __type: 'Issue', __repo: repo, __issue: issue };
  }

  pullObj(repo: Repo, pull: import('./store.ts').Pull): Obj {
    return { __type: 'PullRequest', __repo: repo, __pull: pull };
  }

  executeOperation(op: Operation, variables: Record<string, unknown>): { data: Record<string, unknown>; errors?: GqlError[] } {
    this.errors = [];
    const rootType = op.kind === 'mutation' ? 'Mutation' : 'Query';
    const data: Record<string, unknown> = {};
    for (const sel of op.selections) {
      const key = sel.alias ?? sel.name;
      data[key] = this.resolveRoot(sel, op.kind, variables);
    }
    void rootType;
    const result: { data: Record<string, unknown>; errors?: GqlError[] } = { data };
    if (this.errors.length > 0) result.errors = this.errors;
    return result;
  }

  resolveRoot(sel: Selection, kind: 'query' | 'mutation', vars: Record<string, unknown>): unknown {
    const args = resolveArgs(sel, vars);
    if (kind === 'query') {
      if (sel.name === 'viewer') {
        const user: Obj = { __type: 'User', __user: { login: this.store.viewerLogin, id: this.store.viewerId, name: this.store.viewerName, email: this.store.viewerEmail } };
        return this.resolveSelections(user, sel.selections, vars);
      }
      if (sel.name === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        const repo = owner === this.store.viewerLogin ? this.store.getRepo(owner, name) : undefined;
        if (!repo) return null;
        return this.resolveSelections(this.repoObj(repo), sel.selections, vars);
      }
      if (sel.name === 'node') {
        const id = String(args.id ?? '');
        const repo = this.repoByGlobalId(id);
        if (repo) return this.resolveSelections(this.repoObj(repo), sel.selections, vars);
        const issue = this.issueByGlobalId(id);
        if (issue) return this.resolveSelections(this.issueObj(issue.repo, issue.issue), sel.selections, vars);
        const pull = this.pullByGlobalId(id);
        if (pull) return this.resolveSelections(this.pullObj(pull.repo, pull.pull), sel.selections, vars);
        return null;
      }
      if (sel.name === '__type') {
        const name = String(args.name ?? '');
        const typeObj: Obj = { __type: '__Type', __typeName: name };
        return this.resolveSelections(typeObj, sel.selections, vars);
      }
      if (sel.name === '__schema') return null;
      this.errors.push({ message: `Cannot query field "${sel.name}" on type "Query".` });
      return null;
    }
    // Mutations
    if (sel.name === 'createIssue') return this.mutateCreateIssue(sel, args, vars);
    if (sel.name === 'closeIssue') return this.mutateCloseIssue(sel, args, vars);
    if (sel.name === 'createPullRequest') return this.mutateCreatePullRequest(sel, args, vars);
    if (sel.name === 'mergePullRequest') return this.mutateMergePullRequest(sel, args, vars);
    if (sel.name === 'addComment') return this.mutateAddComment(sel, args, vars);
    this.errors.push({ message: `Cannot query field "${sel.name}" on type "Mutation".` });
    return null;
  }

  mutateCreateIssue(sel: Selection, args: Record<string, unknown>, vars: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const repo = this.repoByGlobalId(String(input.repositoryId ?? ''));
    if (!repo) {
      this.errors.push({ message: 'Repository not found' });
      return null;
    }
    this.store.issueIdSeq += 1;
    repo.issueSeq += 1;
    const seq = this.store.nextSeq();
    const issue = { id: this.store.issueIdSeq, number: repo.issueSeq, title: String(input.title ?? ''), body: String(input.body ?? ''), state: 'open' as const, labels: [] as string[], seq, comments: [] as { id: number; body: string; seq: number }[] };
    repo.issues.set(issue.number, issue);
    return this.resolveSelections({ __type: 'CreateIssuePayload', __issue: issue, __repo: repo }, sel.selections, vars);
  }

  mutateCloseIssue(sel: Selection, args: Record<string, unknown>, vars: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.issueByGlobalId(String(input.issueId ?? ''));
    if (!found) {
      this.errors.push({ message: 'Issue not found' });
      return null;
    }
    found.issue.state = 'closed';
    return this.resolveSelections({ __type: 'CloseIssuePayload', __issue: found.issue, __repo: found.repo }, sel.selections, vars);
  }

  mutateCreatePullRequest(sel: Selection, args: Record<string, unknown>, vars: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const repo = this.repoByGlobalId(String(input.repositoryId ?? ''));
    if (!repo) {
      this.errors.push({ message: 'Repository not found' });
      return null;
    }
    const headRef = String(input.headRefName ?? '');
    const baseRef = String(input.baseRefName ?? repo.defaultBranch);
    const headSha = repo.refs.get(`refs/heads/${headRef}`);
    const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
    if (headSha === undefined || baseSha === undefined) {
      this.errors.push({ message: 'head or base branch not found' });
      return null;
    }
    const { files, mergeBaseSha } = this.store.computePull(repo, headSha, baseSha);
    this.store.pullIdSeq += 1;
    repo.pullSeq += 1;
    const seq = this.store.nextSeq();
    const pull = {
      id: this.store.pullIdSeq,
      number: repo.pullSeq,
      title: String(input.title ?? ''),
      body: '',
      state: 'open' as const,
      merged: false,
      mergedAt: null as string | null,
      mergeCommitSha: null as string | null,
      headRef,
      baseRef,
      headSha,
      baseSha,
      mergeBaseSha,
      files,
      seq,
    };
    repo.pulls.set(pull.number, pull);
    return this.resolveSelections({ __type: 'CreatePullRequestPayload', __pull: pull, __repo: repo }, sel.selections, vars);
  }

  mutateMergePullRequest(sel: Selection, args: Record<string, unknown>, vars: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.pullByGlobalId(String(input.pullRequestId ?? ''));
    if (!found) {
      this.errors.push({ message: 'Pull request not found' });
      return null;
    }
    const { repo, pull } = found;
    if (pull.merged || pull.state !== 'open') {
      this.errors.push({ message: 'Pull request is not open' });
      return null;
    }
    const headSha = repo.refs.get(`refs/heads/${pull.headRef}`);
    const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (headSha === undefined || baseSha === undefined) {
      this.errors.push({ message: 'head or base branch not found' });
      return null;
    }
    const { conflict } = this.store.computePull(repo, headSha, baseSha);
    if (conflict) {
      this.errors.push({ message: 'Merge conflict' });
      return null;
    }
    const headCommit = repo.objects.get(headSha);
    const headTree = headCommit && headCommit.type === 'commit' ? this.store.parseCommit(headCommit.data).tree : null;
    if (headTree === null) {
      this.errors.push({ message: 'head has no tree' });
      return null;
    }
    const seq = this.store.nextSeq();
    const mergeSha = this.store.createCommit(repo, headTree, [baseSha], `Merge pull request #${pull.number}: ${pull.title}`, this.store.tsEpoch(seq));
    repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = this.store.tsIso(seq);
    pull.mergeCommitSha = mergeSha;
    pull.baseSha = mergeSha;
    return this.resolveSelections({ __type: 'MergePullRequestPayload', __pull: pull, __repo: repo }, sel.selections, vars);
  }

  mutateAddComment(sel: Selection, args: Record<string, unknown>, vars: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.issueByGlobalId(String(input.subjectId ?? ''));
    if (!found) {
      this.errors.push({ message: 'Subject not found' });
      return null;
    }
    this.store.commentIdSeq += 1;
    const seq = this.store.nextSeq();
    const comment = { id: this.store.commentIdSeq, body: String(input.body ?? ''), seq };
    found.issue.comments.push(comment);
    const edge: Obj = { __type: 'CommentEdge', __comment: comment, __repo: found.repo };
    return this.resolveSelections({ __type: 'AddCommentPayload', __edge: edge }, sel.selections, vars);
  }

  resolveSelections(obj: Obj, selections: Selection[] | null, vars: Record<string, unknown>): Record<string, unknown> | null {
    if (selections === null) return null;
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      if (sel.name === '__fragment') {
        if (sel.fragmentType !== null && sel.fragmentType !== obj.__type && sel.fragmentType !== 'Node') continue;
        const sub = this.resolveSelections(obj, sel.selections, vars);
        if (sub) Object.assign(out, sub);
        continue;
      }
      const key = sel.alias ?? sel.name;
      out[key] = this.resolveField(obj, sel, vars);
    }
    return out;
  }

  resolveField(obj: Obj, sel: Selection, vars: Record<string, unknown>): unknown {
    const type = obj.__type;
    const known = TYPE_FIELDS[type] ?? [];
    // Payload types expose their nested object under its field name.
    if (type.endsWith('Payload')) {
      if (sel.name === 'issue' && obj.__issue) return this.resolveSelections(this.issueObj(obj.__repo as Repo, obj.__issue as never), sel.selections, vars);
      if (sel.name === 'pullRequest' && obj.__pull) return this.resolveSelections(this.pullObj(obj.__repo as Repo, obj.__pull as never), sel.selections, vars);
      if (sel.name === 'commentEdge' && obj.__edge) return this.resolveSelections(obj.__edge as Obj, sel.selections, vars);
      this.errors.push({ message: `Cannot query field "${sel.name}" on type "${type}".` });
      return null;
    }
    if (!known.includes(sel.name)) {
      this.errors.push({ message: `Cannot query field "${sel.name}" on type "${type}".` });
      return null;
    }
    const args = resolveArgs(sel, vars);
    switch (type) {
      case 'User': {
        const u = obj.__user as { login: string; id: number; name: string; email: string };
        if (sel.name === 'login') return u.login;
        if (sel.name === 'id') return `U${u.id}`;
        if (sel.name === 'name') return u.name;
        if (sel.name === 'email') return u.email;
        return null;
      }
      case 'Repository': {
        const repo = obj.__repo as Repo;
        switch (sel.name) {
          case 'id':
            return `R${repo.id}`;
          case 'databaseId':
            return repo.id;
          case 'name':
            return repo.name;
          case 'nameWithOwner':
            return `${repo.owner}/${repo.name}`;
          case 'owner':
            return this.resolveSelections({ __type: 'User', __user: { login: repo.owner, id: this.store.viewerId, name: this.store.viewerName, email: this.store.viewerEmail } }, sel.selections, vars);
          case 'defaultBranchRef':
            return this.resolveSelections({ __type: 'Ref', __name: repo.defaultBranch }, sel.selections, vars);
          case 'viewerPermission':
            return 'ADMIN';
          case 'hasIssuesEnabled':
            return true;
          case 'issue': {
            const issue = repo.issues.get(Number(args.number));
            if (!issue) return null;
            return this.resolveSelections(this.issueObj(repo, issue), sel.selections, vars);
          }
          case 'issues': {
            return this.resolveIssueConnection(repo, args, sel, vars);
          }
          case 'pullRequests': {
            const first = typeof args.first === 'number' ? args.first : 100;
            const list = [...repo.pulls.values()].sort((a, b) => b.seq - a.seq).slice(0, first);
            const nodesSel = sel.selections?.find((s) => s.name === 'nodes');
            const nodes = list.map((p) => this.resolveSelections(this.pullObj(repo, p), nodesSel?.selections ?? null, vars));
            return this.resolveConnectionNodes(sel, nodes, vars);
          }
          case 'pullRequest': {
            const pull = repo.pulls.get(Number(args.number));
            if (!pull) return null;
            return this.resolveSelections(this.pullObj(repo, pull), sel.selections, vars);
          }
          default:
            return null;
        }
      }
      case 'Issue': {
        const issue = obj.__issue as { id: number; number: number; title: string; body: string; state: string };
        const repo = obj.__repo as Repo;
        switch (sel.name) {
          case 'id':
            return `I${repo.id}_${issue.number}`;
          case 'number':
            return issue.number;
          case 'title':
            return issue.title;
          case 'state':
            return issue.state === 'open' ? 'OPEN' : 'CLOSED';
          case 'body':
            return issue.body;
          default:
            return null;
        }
      }
      case 'CommentEdge': {
        if (sel.name === 'node') {
          const c = obj.__comment as { id: number; body: string };
          return this.resolveSelections({ __type: 'Comment', __comment: c, __repo: obj.__repo as Repo }, sel.selections, vars);
        }
        return null;
      }
      case 'Comment': {
        const c = obj.__comment as { id: number; body: string };
        const repo = obj.__repo as Repo;
        if (sel.name === 'id') return `C${repo.id}_${c.id}`;
        if (sel.name === 'body') return c.body;
        return null;
      }
      case 'PullRequest': {
        const pull = obj.__pull as import('./store.ts').Pull;
        const repo = obj.__repo as Repo;
        switch (sel.name) {
          case 'id':
            return `P${repo.id}_${pull.number}`;
          case 'number':
            return pull.number;
          case 'title':
            return pull.title;
          case 'body':
            return pull.body;
          case 'state':
            return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
          case 'merged':
            return pull.merged;
          case 'headRefName':
            return pull.headRef;
          case 'baseRefName':
            return pull.baseRef;
          case 'mergeable': {
            if (pull.merged) return 'MERGEABLE';
            const headSha = repo.refs.get(`refs/heads/${pull.headRef}`);
            const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`);
            if (headSha === undefined || baseSha === undefined) return 'UNKNOWN';
            return this.store.computePull(repo, headSha, baseSha).conflict ? 'CONFLICTING' : 'MERGEABLE';
          }
          case 'files': {
            const first = typeof args.first === 'number' ? args.first : 100;
            const nodes = pull.files.slice(0, first).map((f) => ({ path: f.filename }));
            const nodesSel = sel.selections?.[0]?.selections ?? null;
            const resolvedNodes = nodes.map((n) => {
              const o: Record<string, unknown> = {};
              for (const s of nodesSel ?? [{ alias: null, name: 'path', args: {}, selections: null, fragmentType: null }]) {
                o[s.alias ?? s.name] = (n as Record<string, unknown>)[s.name] ?? null;
              }
              return o;
            });
            return this.resolveConnectionNodes(sel, resolvedNodes, vars);
          }
          default:
            return null;
        }
      }
      case 'Ref': {
        if (sel.name === 'name') return obj.__name;
        return null;
      }
      case '__Type': {
        const name = obj.__typeName as string;
        if (sel.name === 'name') return name;
        if (sel.name === 'fields') {
          const fields = TYPE_FIELDS[name] ?? [];
          return fields.map((f) => ({ name: f }));
        }
        if (sel.name === 'enumValues') {
          if (name === 'SearchType') return SEARCH_TYPE_VALUES.map((v) => ({ name: v }));
          return [];
        }
        return null;
      }
      default:
        return null;
    }
  }

  resolveIssueConnection(repo: Repo, args: Record<string, unknown>, sel: Selection, vars: Record<string, unknown>): Record<string, unknown> {
    const states = Array.isArray(args.states) ? (args.states as string[]) : null;
    const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
    const direction = orderBy.direction === 'ASC' ? 'ASC' : 'DESC';
    const afterNum = numberFromCursor(typeof args.after === 'string' ? args.after : null);
    let list = [...repo.issues.values()];
    if (states !== null) {
      const wanted = new Set(states.map((s) => String(s).toLowerCase()));
      list = list.filter((i) => wanted.has(i.state));
    }
    list.sort((a, b) => (direction === 'ASC' ? a.seq - b.seq : b.seq - a.seq));
    if (afterNum !== null) {
      list = list.filter((i) => (direction === 'ASC' ? i.number > afterNum : i.number < afterNum));
    }
    const first = typeof args.first === 'number' ? args.first : list.length;
    const page = list.slice(0, first);
    const hasNext = list.length > first;
    const nodesSel = sel.selections?.find((s) => s.name === 'nodes');
    const pageInfoSel = sel.selections?.find((s) => s.name === 'pageInfo');
    const nodes = page.map((issue) => this.resolveSelections(this.issueObj(repo, issue), nodesSel?.selections ?? null, vars));
    const endCursor = page.length > 0 ? cursorForNumber(page[page.length - 1].number) : null;
    const pageInfo: Record<string, unknown> = {};
    if (pageInfoSel?.selections) {
      for (const s of pageInfoSel.selections) {
        if (s.name === 'hasNextPage') pageInfo[s.alias ?? s.name] = hasNext;
        else if (s.name === 'endCursor') pageInfo[s.alias ?? s.name] = endCursor;
      }
    }
    return { nodes, pageInfo };
  }

  /** Generic connection wrapper for pullRequests/files: {nodes}. */
  resolveConnectionNodes(sel: Selection, nodes: unknown[], vars: Record<string, unknown>): Record<string, unknown> {
    void vars;
    const out: Record<string, unknown> = {};
    if (!sel.selections) return { nodes };
    for (const s of sel.selections) {
      if (s.name === 'nodes') out[s.alias ?? s.name] = nodes;
      else if (s.name === 'pageInfo') out[s.alias ?? s.name] = { hasNextPage: false, endCursor: null };
      else out[s.alias ?? s.name] = null;
    }
    return out;
  }
}

/** Execute one GraphQL request body (single operation or batch). */
export function executeGraphQL(store: Store, body: unknown): unknown {
  const runOne = (doc: { query?: unknown; variables?: unknown; operationName?: unknown }): Record<string, unknown> => {
    if (typeof doc.query !== 'string') return { data: null, errors: [{ message: 'query is required' }] };
    let ops: Operation[];
    try {
      ops = new Parser(doc.query).parseDocument();
    } catch (e) {
      return { data: null, errors: [{ message: e instanceof Error ? e.message : 'parse error' }] };
    }
    if (ops.length === 0) return { data: {}, errors: [{ message: 'empty document' }] };
    const vars = (doc.variables ?? {}) as Record<string, unknown>;
    let op = ops[0];
    if (typeof doc.operationName === 'string') {
      const named = ops.find((o) => o.name === doc.operationName);
      if (named) op = named;
    }
    const exec = new GraphQLExecutor(store);
    return exec.executeOperation(op, vars);
  };
  if (Array.isArray(body)) return body.map((d) => runOne(d as { query?: unknown; variables?: unknown; operationName?: unknown }));
  return runOne(body as { query?: unknown; variables?: unknown; operationName?: unknown });
}
