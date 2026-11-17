// GitHub REST v3 handlers. Pure functions over the Store; routing lives in app.ts.

import { type Repo, type Store, ZERO_OID, hashObject } from './store.ts';

export interface Resp {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

export function err(status: number, message: string): Resp {
  return { status, body: { message } };
}

export function ok(status: number, body: unknown, headers?: Record<string, string>): Resp {
  return { status, body, headers };
}

export function repoJson(store: Store, repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: store.viewerId, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: store.tsIso(repo.createdSeq),
    updated_at: store.tsIso(repo.createdSeq),
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    clone_url: `https://github.com/${repo.owner}/${repo.name}.git`,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

export function issueJson(store: Store, repo: Repo, issue: { id: number; number: number; title: string; body: string; state: string; labels: string[]; seq: number; comments: { id: number }[] }): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I${repo.id}_${issue.number}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const l = repo.labels.get(name);
      return { name, color: l ? l.color : 'ededed' };
    }),
    user: { login: store.viewerLogin, id: store.viewerId, type: 'User' },
    created_at: store.tsIso(issue.seq),
    updated_at: store.tsIso(issue.seq),
    comments: issue.comments.length,
    comments_url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments`,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

export function pullJson(store: Store, repo: Repo, pull: { id: number; number: number; title: string; body: string; state: string; merged: boolean; mergedAt: string | null; mergeCommitSha: string | null; headRef: string; baseRef: string; headSha: string; baseSha: string; seq: number }): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: `P${repo.id}_${pull.number}`,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}` },
    user: { login: store.viewerLogin, id: store.viewerId, type: 'User' },
    created_at: store.tsIso(pull.seq),
    updated_at: store.tsIso(pull.seq),
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

/** Resolve a ref name or commit oid to a commit oid, or null. */
export function resolveRef(store: Store, repo: Repo, ref: string | undefined | null): string | null {
  if (ref === undefined || ref === null || ref === '') {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }
  const direct = repo.refs.get(`refs/heads/${ref}`);
  if (direct !== undefined) return direct;
  const full = repo.refs.get(ref);
  if (full !== undefined) return full;
  const obj = repo.objects.get(ref);
  if (obj && obj.type === 'commit') return ref;
  return null;
}

function commitInfo(store: Store, repo: Repo, oid: string): { sha: string; message: string; author: { name: string; email: string; date: string }; committer: { name: string; email: string; date: string }; parents: { sha: string }[] } {
  const obj = repo.objects.get(oid);
  const c = obj ? store.parseCommit(obj.data) : { tree: '', parents: [], author: '', committer: '', message: '' };
  const parseIdent = (ident: string): { name: string; email: string; date: string } => {
    const m = /^(.*?) <([^>]*)> (\d+) \+0000$/.exec(ident);
    if (!m) return { name: store.viewerName, email: store.viewerEmail, date: store.tsIso(1) };
    return { name: m[1], email: m[2], date: new Date(Number(m[3]) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z') };
  };
  return {
    sha: oid,
    message: c.message.replace(/\n$/, ''),
    author: parseIdent(c.author),
    committer: parseIdent(c.committer),
    parents: c.parents.map((p) => ({ sha: p })),
  };
}

/** Shared squash-merge: returns the merge commit sha, or an error message. */
export function mergePullRequest(store: Store, repo: Repo, pull: import('./store.ts').Pull): { ok: boolean; sha?: string; message?: string } {
  if (pull.merged) return { ok: false, message: 'Pull request already merged' };
  if (pull.state !== 'open') return { ok: false, message: 'Pull request is not open' };
  const headSha = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (headSha === undefined || baseSha === undefined) return { ok: false, message: 'head or base branch not found' };
  const { conflict } = store.computePull(repo, headSha, baseSha);
  if (conflict) return { ok: false, message: 'Merge conflict' };
  const headCommit = repo.objects.get(headSha);
  const headTree = headCommit && headCommit.type === 'commit' ? store.parseCommit(headCommit.data).tree : null;
  if (headTree === null) return { ok: false, message: 'head has no tree' };
  const seq = store.nextSeq();
  const mergeSha = store.createCommit(repo, headTree, [baseSha], `Merge pull request #${pull.number}: ${pull.title}`, store.tsEpoch(seq));
  repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = store.tsIso(seq);
  pull.mergeCommitSha = mergeSha;
  pull.baseSha = mergeSha;
  return { ok: true, sha: mergeSha };
}

/** Commit a file change on a branch; returns the new commit oid. */
export function commitFile(store: Store, repo: Repo, branch: string, path: string, content: Buffer, message: string, remove: boolean): string {
  const refName = `refs/heads/${branch}`;
  const tip = repo.refs.get(refName);
  const files = tip !== undefined ? store.flattenTree(repo, store.parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree) : new Map<string, { oid: string; mode: string }>();
  if (remove) {
    files.delete(path);
  } else {
    const blobOid = store.putObject(repo, 'blob', content);
    files.set(path, { oid: blobOid, mode: '100644' });
  }
  const treeOid = store.buildTreeFromFiles(repo, files);
  const seq = store.nextSeq();
  const commitOid = store.createCommit(repo, treeOid, tip !== undefined ? [tip] : [], message, store.tsEpoch(seq));
  repo.refs.set(refName, commitOid);
  return commitOid;
}

function paginate<T>(items: T[], query: URLSearchParams, baseUrl: string, path: string): { page: T[]; link: string | null } {
  const perPageRaw = Number(query.get('per_page') ?? '30');
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(Math.trunc(perPageRaw), 100) : 30;
  const pageRaw = Number(query.get('page') ?? '1');
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.trunc(pageRaw) : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  if (start + perPage < items.length) {
    const next = new URL(path, baseUrl);
    next.searchParams.set('per_page', String(perPage));
    next.searchParams.set('page', String(page + 1));
    // Preserve other query params.
    for (const [k, v] of query) {
      if (k !== 'per_page' && k !== 'page') next.searchParams.set(k, v);
    }
    link = `<${next.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}

export interface RestCtx {
  store: Store;
  query: URLSearchParams;
  body: unknown;
  baseUrl: string;
}

/** Dispatch a REST request. `segs` are the path segments after /api/v3. */
export function handleRest(ctx: RestCtx, method: string, segs: string[]): Resp {
  const { store, query } = ctx;
  const s = segs;

  // GET /user, GET /user/repos, POST /user/repos
  if (s[0] === 'user') {
    if (s.length === 1 && method === 'GET') {
      return ok(200, { login: store.viewerLogin, id: store.viewerId, type: 'User', name: store.viewerName, email: store.viewerEmail });
    }
    if (s.length === 2 && s[1] === 'repos') {
      if (method === 'GET') {
        const repos = store.listViewerRepos();
        const { page, link } = paginate(repos.map((r) => repoJson(store, r)), query, ctx.baseUrl, '/api/v3/user/repos');
        return ok(200, page, link ? { link } : undefined);
      }
      if (method === 'POST') {
        const b = (ctx.body ?? {}) as Record<string, unknown>;
        const name = typeof b.name === 'string' ? b.name : '';
        if (name === '') return err(422, 'name is required');
        if (store.getRepo(store.viewerLogin, name)) return err(422, `Repository ${name} already exists`);
        const repo = store.createRepo(store.viewerLogin, name, typeof b.description === 'string' ? b.description : '', b.private === true);
        return ok(201, repoJson(store, repo));
      }
    }
    return err(404, 'Not Found');
  }

  if (s[0] === 'meta' && s.length === 1 && method === 'GET') {
    return ok(200, { installed_version: '1.0.0', verifiable_password_authentication: true });
  }

  if (s[0] !== 'repos' || s.length < 3) return err(404, 'Not Found');
  const owner = decodeURIComponent(s[1]);
  const name = decodeURIComponent(s[2]);
  if (owner !== store.viewerLogin) return err(404, 'Not Found');
  const repo = store.getRepo(owner, name);
  const rest = s.slice(3);

  // GET/PATCH/DELETE /repos/{o}/{r}
  if (rest.length === 0) {
    if (method === 'GET') {
      if (!repo) return err(404, 'Not Found');
      return ok(200, repoJson(store, repo));
    }
    if (method === 'PATCH') {
      if (!repo) return err(404, 'Not Found');
      const b = (ctx.body ?? {}) as Record<string, unknown>;
      if (typeof b.description === 'string') repo.description = b.description;
      return ok(200, repoJson(store, repo));
    }
    if (method === 'DELETE') {
      if (!repo) return err(404, 'Not Found');
      store.deleteRepo(owner, name);
      return { status: 204, body: null };
    }
    return err(404, 'Not Found');
  }
  if (!repo) return err(404, 'Not Found');

  // /git/refs
  if (rest[0] === 'git' && rest[1] === 'refs') {
    if (method === 'POST' && rest.length === 2) {
      const b = (ctx.body ?? {}) as Record<string, unknown>;
      const ref = typeof b.ref === 'string' ? b.ref : '';
      const sha = typeof b.sha === 'string' ? b.sha : '';
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha)) return err(422, 'invalid ref or sha');
      if (repo.refs.has(ref)) return err(422, 'Reference already exists');
      const obj = repo.objects.get(sha);
      if (!obj) return err(422, 'sha does not exist');
      repo.refs.set(ref, sha);
      return ok(201, { ref, object: { sha, type: obj.type }, url: `/api/v3/repos/${owner}/${name}/git/refs/${ref}` });
    }
    if (method === 'GET' && rest.length >= 3) {
      const refPath = rest.slice(2).map(decodeURIComponent).join('/');
      const sha = repo.refs.get(refPath) ?? repo.refs.get(`refs/${refPath}`);
      if (sha === undefined) return err(404, 'Not Found');
      return ok(200, { ref: `refs/${refPath}`, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
    }
    return err(404, 'Not Found');
  }

  // /branches
  if (rest[0] === 'branches') {
    if (method === 'GET' && rest.length === 1) {
      const list: unknown[] = [];
      for (const [ref, sha] of [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
        if (!ref.startsWith('refs/heads/')) continue;
        list.push({ name: ref.slice('refs/heads/'.length), commit: { sha }, protected: false });
      }
      return ok(200, list);
    }
    if (method === 'GET' && rest.length === 2) {
      const sha = repo.refs.get(`refs/heads/${decodeURIComponent(rest[1])}`);
      if (sha === undefined) return err(404, 'Not Found');
      return ok(200, { name: decodeURIComponent(rest[1]), commit: { sha }, protected: false });
    }
    return err(404, 'Not Found');
  }

  // /commits
  if (rest[0] === 'commits' && method === 'GET' && rest.length === 1) {
    const tip = resolveRef(store, repo, query.get('sha') ?? undefined);
    if (tip === null) {
      // Unborn repo: empty list.
      if (repo.refs.size === 0) return ok(200, []);
      return err(404, 'Not Found');
    }
    const out: unknown[] = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.shift() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const c = store.parseCommit(obj.data);
      out.push({ sha: oid, commit: { message: c.message.replace(/\n$/, ''), author: identOf(store, c.author), committer: identOf(store, c.committer) }, parents: c.parents.map((p) => ({ sha: p })) });
      for (const p of c.parents) stack.push(p);
    }
    return ok(200, out);
  }

  // /contents/*
  if (rest[0] === 'contents') {
    const path = rest.slice(1).map(decodeURIComponent).join('/');
    if (method === 'PUT') {
      const b = (ctx.body ?? {}) as Record<string, unknown>;
      const message = typeof b.message === 'string' ? b.message : `Update ${path}`;
      const contentB64 = typeof b.content === 'string' ? b.content : '';
      const branch = typeof b.branch === 'string' ? b.branch : repo.defaultBranch;
      const content = Buffer.from(contentB64.replace(/[\r\n\t ]/g, ''), 'base64');
      // Ensure the branch exists (create from default tip when missing).
      if (!repo.refs.has(`refs/heads/${branch}`)) {
        const baseTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        if (baseTip !== undefined) repo.refs.set(`refs/heads/${branch}`, baseTip);
      }
      const tip = repo.refs.get(`refs/heads/${branch}`);
      const files = tip !== undefined ? store.flattenTree(repo, store.parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree) : new Map<string, { oid: string; mode: string }>();
      const existing = files.get(path);
      const sha = typeof b.sha === 'string' ? b.sha : undefined;
      if (existing !== undefined) {
        if (sha === undefined) return err(422, 'sha is required to update a file');
        if (sha.toLowerCase() !== existing.oid.toLowerCase()) return err(409, 'sha does not match the current blob');
      }
      const newBlobOid = store.putObject(repo, 'blob', content);
      const commitOid = commitFile(store, repo, branch, path, content, message, false);
      return ok(existing !== undefined ? 200 : 201, {
        content: { name: path.split('/').pop() ?? path, path, sha: newBlobOid, size: content.length, encoding: 'base64', type: 'file' },
        commit: { sha: commitOid },
      });
    }
    if (method === 'GET') {
      const tip = resolveRef(store, repo, query.get('ref') ?? undefined);
      if (tip === null) return err(404, 'Not Found');
      const files = store.flattenTree(repo, store.parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree);
      const file = files.get(path);
      if (file) {
        const data = (repo.objects.get(file.oid) as { data: Buffer }).data;
        return ok(200, {
          type: 'file',
          encoding: 'base64',
          size: data.length,
          name: path.split('/').pop() ?? path,
          path,
          content: data.toString('base64'),
          sha: file.oid,
        });
      }
      // Directory listing.
      const prefix = path === '' ? '' : `${path}/`;
      const entries = new Map<string, { type: string; name: string; path: string; sha: string }>();
      for (const [p, v] of files) {
        if (prefix !== '' && !p.startsWith(prefix)) continue;
        const restPath = prefix === '' ? p : p.slice(prefix.length);
        const slash = restPath.indexOf('/');
        if (slash === -1) {
          entries.set(restPath, { type: 'file', name: restPath, path: p, sha: v.oid });
        } else {
          const dir = restPath.slice(0, slash);
          if (!entries.has(dir)) {
            // Directory sha: the tree oid of that directory.
            entries.set(dir, { type: 'dir', name: dir, path: `${prefix}${dir}`, sha: dirTreeSha(store, repo, tip, `${prefix}${dir}`) });
          }
        }
      }
      if (entries.size === 0) return err(404, 'Not Found');
      return ok(200, [...entries.values()].sort((a, b) => (a.name < b.name ? -1 : 1)));
    }
    if (method === 'DELETE') {
      const b = (ctx.body ?? {}) as Record<string, unknown>;
      const message = typeof b.message === 'string' ? b.message : `Delete ${path}`;
      const branch = typeof b.branch === 'string' ? b.branch : repo.defaultBranch;
      const tip = repo.refs.get(`refs/heads/${branch}`);
      if (tip === undefined) return err(404, 'Not Found');
      const files = store.flattenTree(repo, store.parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree);
      const existing = files.get(path);
      if (!existing) return err(404, 'Not Found');
      const sha = typeof b.sha === 'string' ? b.sha : undefined;
      if (sha !== undefined && sha.toLowerCase() !== existing.oid.toLowerCase()) return err(409, 'sha does not match the current blob');
      const commitOid = commitFile(store, repo, branch, path, Buffer.alloc(0), message, true);
      return ok(200, { commit: { sha: commitOid } });
    }
    return err(404, 'Not Found');
  }

  // /labels
  if (rest[0] === 'labels') {
    if (method === 'POST' && rest.length === 1) {
      const b = (ctx.body ?? {}) as Record<string, unknown>;
      const name = typeof b.name === 'string' ? b.name : '';
      if (name === '') return err(422, 'name is required');
      const color = typeof b.color === 'string' ? b.color : 'ededed';
      repo.labels.set(name, { name, color });
      return ok(201, { id: repo.labels.size, name, color, url: `/api/v3/repos/${owner}/${repo.name}/labels/${encodeURIComponent(name)}` });
    }
    if (method === 'GET' && rest.length === 1) {
      return ok(200, [...repo.labels.values()].map((l, i) => ({ id: i + 1, name: l.name, color: l.color })));
    }
    return err(404, 'Not Found');
  }

  // /issues
  if (rest[0] === 'issues') {
    if (rest.length === 1) {
      if (method === 'POST') {
        const b = (ctx.body ?? {}) as Record<string, unknown>;
        const title = typeof b.title === 'string' ? b.title : '';
        const body = typeof b.body === 'string' ? b.body : '';
        const labels = Array.isArray(b.labels) ? (b.labels as unknown[]).filter((x): x is string => typeof x === 'string') : [];
        for (const l of labels) {
          if (!repo.labels.has(l)) return err(422, `Label '${l}' does not exist`);
        }
        store.issueIdSeq += 1;
        repo.issueSeq += 1;
        const seq = store.nextSeq();
        const issue = { id: store.issueIdSeq, number: repo.issueSeq, title, body, state: 'open' as const, labels: labels.slice(), seq, comments: [] as { id: number; body: string; seq: number }[] };
        repo.issues.set(issue.number, issue);
        return ok(201, issueJson(store, repo, issue));
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        const sort = query.get('sort') ?? 'created';
        const direction = query.get('direction') ?? 'desc';
        let list = [...repo.issues.values()];
        if (state !== 'all') list = list.filter((i) => i.state === state);
        list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
        void sort;
        const { page, link } = paginate(list.map((i) => issueJson(store, repo, i)), query, ctx.baseUrl, `/api/v3/repos/${owner}/${name}/issues`);
        return ok(200, page, link ? { link } : undefined);
      }
      return err(404, 'Not Found');
    }
    const num = Number(rest[1]);
    if (!Number.isInteger(num)) return err(404, 'Not Found');
    const issue = repo.issues.get(num);
    if (rest.length === 2) {
      if (method === 'GET') {
        if (!issue) return err(404, 'Not Found');
        return ok(200, issueJson(store, repo, issue));
      }
      if (method === 'PATCH') {
        if (!issue) return err(404, 'Not Found');
        const b = (ctx.body ?? {}) as Record<string, unknown>;
        if (typeof b.title === 'string') issue.title = b.title;
        if (typeof b.body === 'string') issue.body = b.body;
        if (b.state === 'open' || b.state === 'closed') issue.state = b.state;
        if (Array.isArray(b.labels)) {
          const labels = (b.labels as unknown[]).filter((x): x is string => typeof x === 'string');
          for (const l of labels) {
            if (!repo.labels.has(l)) return err(422, `Label '${l}' does not exist`);
          }
          issue.labels = labels.slice();
        }
        return ok(200, issueJson(store, repo, issue));
      }
      return err(404, 'Not Found');
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      if (!issue) return err(404, 'Not Found');
      if (method === 'POST') {
        const b = (ctx.body ?? {}) as Record<string, unknown>;
        const body = typeof b.body === 'string' ? b.body : '';
        store.commentIdSeq += 1;
        const seq = store.nextSeq();
        const comment = { id: store.commentIdSeq, body, seq };
        issue.comments.push(comment);
        return ok(201, { id: comment.id, node_id: `C${comment.id}`, body, user: { login: store.viewerLogin, id: store.viewerId, type: 'User' }, created_at: store.tsIso(seq), updated_at: store.tsIso(seq) });
      }
      if (method === 'GET') {
        return ok(200, issue.comments.map((c) => ({ id: c.id, node_id: `C${c.id}`, body: c.body, user: { login: store.viewerLogin, id: store.viewerId, type: 'User' }, created_at: store.tsIso(c.seq), updated_at: store.tsIso(c.seq) })));
      }
      return err(404, 'Not Found');
    }
    return err(404, 'Not Found');
  }

  // /pulls
  if (rest[0] === 'pulls') {
    if (rest.length === 1) {
      if (method === 'POST') {
        const b = (ctx.body ?? {}) as Record<string, unknown>;
        const head = typeof b.head === 'string' ? b.head : '';
        const base = typeof b.base === 'string' ? b.base : repo.defaultBranch;
        const title = typeof b.title === 'string' ? b.title : '';
        const body = typeof b.body === 'string' ? b.body : '';
        const headSha = repo.refs.get(`refs/heads/${head}`);
        const baseSha = repo.refs.get(`refs/heads/${base}`);
        if (headSha === undefined) return err(422, 'head branch not found');
        if (baseSha === undefined) return err(422, 'base branch not found');
        const { files, mergeBaseSha } = store.computePull(repo, headSha, baseSha);
        store.pullIdSeq += 1;
        repo.pullSeq += 1;
        const seq = store.nextSeq();
        const pull = {
          id: store.pullIdSeq,
          number: repo.pullSeq,
          title,
          body,
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
        return ok(201, pullJson(store, repo, pull));
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        let list = [...repo.pulls.values()];
        if (state !== 'all') list = list.filter((p) => p.state === state);
        list.sort((a, b) => b.seq - a.seq);
        const { page, link } = paginate(list.map((p) => pullJson(store, repo, p)), query, ctx.baseUrl, `/api/v3/repos/${owner}/${name}/pulls`);
        return ok(200, page, link ? { link } : undefined);
      }
      return err(404, 'Not Found');
    }
    const num = Number(rest[1]);
    if (!Number.isInteger(num)) return err(404, 'Not Found');
    const pull = repo.pulls.get(num);
    if (rest.length === 2) {
      if (method === 'GET') {
        if (!pull) return err(404, 'Not Found');
        return ok(200, pullJson(store, repo, pull));
      }
      return err(404, 'Not Found');
    }
    if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
      if (!pull) return err(404, 'Not Found');
      return ok(200, pull.files.map((f) => ({ filename: f.filename, status: f.status, additions: f.additions, deletions: f.deletions, changes: f.changes })));
    }
    if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
      if (!pull) return err(404, 'Not Found');
      const result = mergePullRequest(store, repo, pull);
      if (!result.ok) {
        if (result.message === 'Pull request already merged' || result.message === 'Pull request is not open' || result.message === 'Merge conflict') {
          return err(405, result.message);
        }
        return err(404, result.message ?? 'Not Found');
      }
      return ok(200, { sha: result.sha, merged: true, message: 'Pull request successfully merged' });
    }
    return err(404, 'Not Found');
  }

  return err(404, 'Not Found');
}

function identOf(store: Store, ident: string): { name: string; email: string; date: string } {
  const m = /^(.*?) <([^>]*)> (\d+) \+0000$/.exec(ident);
  if (!m) return { name: store.viewerName, email: store.viewerEmail, date: store.tsIso(1) };
  return { name: m[1], email: m[2], date: new Date(Number(m[3]) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z') };
}

function dirTreeSha(store: Store, repo: Repo, tip: string, dirPath: string): string {
  const c = store.parseCommit((repo.objects.get(tip) as { data: Buffer }).data);
  let treeOid = c.tree;
  for (const seg of dirPath.split('/')) {
    const obj = repo.objects.get(treeOid);
    if (!obj) return ZERO_OID;
    const entry = store.parseTree(obj.data).find((e) => e.name === seg);
    if (!entry) return ZERO_OID;
    treeOid = entry.oid;
  }
  return treeOid;
}

export { hashObject };
