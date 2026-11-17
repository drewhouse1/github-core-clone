// In-memory store: one shared object store per repo, plus REST resources.
// Everything is deterministic: ids and timestamps come from monotonic counters
// over a fixed base time (MAKEFUN_TRIAL_NOW when set, else the scenario epoch).

import { createHash } from 'node:crypto';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const SCENARIO_EPOCH_SEC = 1735689600;

export function sha1(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: string, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return sha1(Buffer.concat([head, data]));
}

export type ObjType = 'blob' | 'tree' | 'commit' | 'tag';
export interface GitObject {
  type: ObjType;
  data: Buffer;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  seq: number;
  comments: Comment[];
}

export interface Comment {
  id: number;
  body: string;
  seq: number;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  mergeBaseSha: string | null;
  files: PullFile[];
  seq: number;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdSeq: number;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  issues: Map<number, Issue>;
  issueSeq: number;
  pulls: Map<number, Pull>;
  pullSeq: number;
  labels: Map<string, { name: string; color: string }>;
  commentSeq: number;
}

export class Store {
  viewerLogin: string;
  viewerName: string;
  viewerEmail: string;
  viewerId: number;
  baseEpochSec: number;
  seq = 0;
  repoSeq = 0;
  issueIdSeq = 0;
  commentIdSeq = 0;
  pullIdSeq = 0;
  repos = new Map<string, Repo>(); // key owner + '/' + name
  reqSeq = 0;

  constructor(viewer: { login: string; name: string; email: string; id: number }) {
    this.viewerLogin = viewer.login;
    this.viewerName = viewer.name;
    this.viewerEmail = viewer.email;
    this.viewerId = viewer.id;
    const now = process.env.MAKEFUN_TRIAL_NOW ?? '';
    let epoch = SCENARIO_EPOCH_SEC;
    if (now !== '') {
      const ms = Date.parse(now);
      if (!Number.isNaN(ms)) epoch = Math.floor(ms / 1000);
      else {
        const n = Number(now);
        if (!Number.isNaN(n) && n > 0) epoch = n > 1e12 ? Math.floor(n / 1000) : n;
      }
    }
    this.baseEpochSec = epoch;
  }

  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  /** ISO timestamp for creation sequence n (deterministic). */
  tsIso(seq: number): string {
    return new Date((this.baseEpochSec + seq) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
  }

  /** Epoch seconds for creation sequence n (deterministic, for commit objects). */
  tsEpoch(seq: number): number {
    return this.baseEpochSec + seq;
  }

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    this.repoSeq += 1;
    const repo: Repo = {
      id: 5000 + this.repoSeq - 1,
      owner,
      name,
      description,
      private: isPrivate,
      defaultBranch: 'main',
      createdSeq: this.nextSeq(),
      objects: new Map(),
      refs: new Map(),
      issues: new Map(),
      issueSeq: 0,
      pulls: new Map(),
      pullSeq: 0,
      labels: new Map(),
      commentSeq: 0,
    };
    this.repos.set(this.repoKey(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === this.viewerLogin) out.push(repo);
    }
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  // ---- git objects -------------------------------------------------------

  putObject(repo: Repo, type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  /** Parse a tree object into entries [{mode, name, oid}]. */
  parseTree(data: Buffer): { mode: string; name: string; oid: string }[] {
    const entries: { mode: string; name: string; oid: string }[] = [];
    let i = 0;
    while (i < data.length) {
      const sp = data.indexOf(0x20, i);
      const nul = data.indexOf(0, sp);
      const mode = data.subarray(i, sp).toString('ascii');
      const name = data.subarray(sp + 1, nul).toString('utf8');
      const oid = data.subarray(nul + 1, nul + 21).toString('hex');
      entries.push({ mode, name, oid });
      i = nul + 21;
    }
    return entries;
  }

  buildTree(repo: Repo, entries: { mode: string; name: string; oid: string }[]): string {
    const sorted = entries.slice().sort((a, b) => {
      const an = a.mode === '40000' ? `${a.name}/` : a.name;
      const bn = b.mode === '40000' ? `${b.name}/` : b.name;
      return an < bn ? -1 : an > bn ? 1 : 0;
    });
    const parts: Buffer[] = [];
    for (const e of sorted) {
      parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
      parts.push(Buffer.from(e.oid, 'hex'));
    }
    return this.putObject(repo, 'tree', Buffer.concat(parts));
  }

  /** Flatten a tree (by oid) to path -> {oid, mode}. Missing tree = empty. */
  flattenTree(repo: Repo, treeOid: string | null): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    if (treeOid === null) return out;
    const walk = (oid: string, prefix: string): void => {
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'tree') return;
      for (const e of this.parseTree(obj.data)) {
        const p = prefix === '' ? e.name : `${prefix}/${e.name}`;
        if (e.mode === '40000') walk(e.oid, p);
        else out.set(p, { oid: e.oid, mode: e.mode });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** Build nested trees from a flat path map; returns root tree oid. */
  buildTreeFromFiles(repo: Repo, files: Map<string, { oid: string; mode: string }>): string {
    const build = (prefix: string): string => {
      const entries: { mode: string; name: string; oid: string }[] = [];
      const dirs = new Map<string, Map<string, { oid: string; mode: string }>>();
      for (const [path, v] of files) {
        if (prefix !== '' && !path.startsWith(`${prefix}/`)) continue;
        const rest = prefix === '' ? path : path.slice(prefix.length + 1);
        const slash = rest.indexOf('/');
        if (slash === -1) {
          entries.push({ mode: v.mode, name: rest, oid: v.oid });
        } else {
          const dir = rest.slice(0, slash);
          let sub = dirs.get(dir);
          if (!sub) {
            sub = new Map();
            dirs.set(dir, sub);
          }
          sub.set(path, v);
        }
      }
      for (const [dir, sub] of dirs) {
        const subPrefix = prefix === '' ? dir : `${prefix}/${dir}`;
        void sub;
        entries.push({ mode: '40000', name: dir, oid: build(subPrefix) });
      }
      return this.buildTree(repo, entries);
    };
    return build('');
  }

  parseCommit(data: Buffer): { tree: string; parents: string[]; author: string; committer: string; message: string } {
    const text = data.toString('utf8');
    const headEnd = text.indexOf('\n\n');
    const head = headEnd === -1 ? text : text.slice(0, headEnd);
    const message = headEnd === -1 ? '' : text.slice(headEnd + 2);
    let tree = '';
    const parents: string[] = [];
    let author = '';
    let committer = '';
    for (const line of head.split('\n')) {
      if (line.startsWith('tree ')) tree = line.slice(5).trim();
      else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
      else if (line.startsWith('author ')) author = line.slice(7);
      else if (line.startsWith('committer ')) committer = line.slice(10);
    }
    return { tree, parents, author, committer, message };
  }

  /** Create a commit object; returns its oid. */
  createCommit(repo: Repo, treeOid: string, parents: string[], message: string, epochSec: number): string {
    const msg = message.endsWith('\n') ? message : `${message}\n`;
    const ident = `${this.viewerName} <${this.viewerEmail}> ${epochSec} +0000`;
    let text = `tree ${treeOid}\n`;
    for (const p of parents) text += `parent ${p}\n`;
    text += `author ${ident}\ncommitter ${ident}\n\n${msg}`;
    return this.putObject(repo, 'commit', Buffer.from(text, 'utf8'));
  }

  /** All commit oids reachable from `roots` (following parents + tag targets + trees). */
  reachableCommits(repo: Repo, roots: string[]): Set<string> {
    const seen = new Set<string>();
    const stack = roots.slice();
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      if (obj.type === 'commit') {
        seen.add(oid);
        const c = this.parseCommit(obj.data);
        for (const p of c.parents) stack.push(p);
      } else if (obj.type === 'tag') {
        const text = obj.data.toString('utf8');
        const m = /^object ([0-9a-f]{40})$/m.exec(text);
        if (m) stack.push(m[1]);
      }
    }
    return seen;
  }

  /** Full reachable object set from commit/tag roots (commits, trees, blobs, tags). */
  /**
   * All objects reachable from roots. `stopAt` (shallow boundaries): a commit
   * in it contributes itself + its tree, but its parents are NOT walked —
   * a shallow clone holds the boundary commit without its history.
   */
  reachableObjects(repo: Repo, roots: string[], stopAt: Set<string> = new Set()): Map<string, GitObject> {
    const out = new Map<string, GitObject>();
    const stack = roots.slice();
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (out.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      out.set(oid, obj);
      if (obj.type === 'commit') {
        const c = this.parseCommit(obj.data);
        stack.push(c.tree);
        if (!stopAt.has(oid)) for (const p of c.parents) stack.push(p);
      } else if (obj.type === 'tree') {
        for (const e of this.parseTree(obj.data)) stack.push(e.oid);
      } else if (obj.type === 'tag') {
        const m = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
        if (m) stack.push(m[1]);
      }
    }
    return out;
  }

  /** Is `ancestor` an ancestor of (or equal to) `oid`? */
  isAncestor(repo: Repo, ancestor: string, oid: string): boolean {
    if (ancestor === oid) return true;
    const seen = new Set<string>();
    const stack = [oid];
    while (stack.length > 0) {
      const cur = stack.pop() as string;
      if (cur === ancestor) return true;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const obj = repo.objects.get(cur);
      if (!obj || obj.type !== 'commit') continue;
      for (const p of this.parseCommit(obj.data).parents) stack.push(p);
    }
    return false;
  }

  /** Merge-base: first common ancestor of a and b (newest-first walk). */
  mergeBase(repo: Repo, a: string, b: string): string | null {
    const aSet = this.reachableCommits(repo, [a]);
    // Walk b's ancestry newest-first (by insertion order = reverse chronological
    // for linear histories); return the first commit also in aSet.
    const seen = new Set<string>();
    const queue: string[] = [b];
    while (queue.length > 0) {
      const cur = queue.shift() as string;
      if (aSet.has(cur)) return cur;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const obj = repo.objects.get(cur);
      if (!obj || obj.type !== 'commit') continue;
      for (const p of this.parseCommit(obj.data).parents) queue.push(p);
    }
    return null;
  }

  /** Diff two trees (flat maps) into PullFile list. */
  diffFiles(oldFiles: Map<string, { oid: string; mode: string }>, newFiles: Map<string, { oid: string; mode: string }>, repo: Repo): PullFile[] {
    const out: PullFile[] = [];
    const paths = new Set([...oldFiles.keys(), ...newFiles.keys()]);
    for (const path of paths) {
      const o = oldFiles.get(path);
      const n = newFiles.get(path);
      if (o && n && o.oid === n.oid) continue;
      const oldText = o ? (repo.objects.get(o.oid)?.data.toString('utf8') ?? '') : '';
      const newText = n ? (repo.objects.get(n.oid)?.data.toString('utf8') ?? '') : '';
      const oldLines = oldText === '' ? [] : oldText.split('\n');
      const newLines = newText === '' ? [] : newText.split('\n');
      const oldSet = new Map<string, number>();
      for (const l of oldLines) oldSet.set(l, (oldSet.get(l) ?? 0) + 1);
      let additions = 0;
      for (const l of newLines) {
        const c = oldSet.get(l) ?? 0;
        if (c > 0) oldSet.set(l, c - 1);
        else additions += 1;
      }
      const newSet = new Map<string, number>();
      for (const l of newLines) newSet.set(l, (newSet.get(l) ?? 0) + 1);
      let deletions = 0;
      for (const l of oldLines) {
        const c = newSet.get(l) ?? 0;
        if (c > 0) newSet.set(l, c - 1);
        else deletions += 1;
      }
      out.push({
        filename: path,
        status: !o ? 'added' : !n ? 'removed' : 'modified',
        additions,
        deletions,
        changes: additions + deletions,
      });
    }
    out.sort((x, y) => (x.filename < y.filename ? -1 : 1));
    return out;
  }

  /** Compute PR files + mergeability from head/base tips. */
  computePull(repo: Repo, headSha: string, baseSha: string): { files: PullFile[]; mergeBaseSha: string | null; conflict: boolean } {
    const mb = baseSha === ZERO_OID ? null : this.mergeBase(repo, headSha, baseSha);
    const headCommit = repo.objects.get(headSha);
    const headTree = headCommit && headCommit.type === 'commit' ? this.parseCommit(headCommit.data).tree : null;
    const headFiles = this.flattenTree(repo, headTree);
    const mbFiles = mb === null ? new Map() : this.flattenTree(repo, this.parseCommit((repo.objects.get(mb) as GitObject).data).tree);
    const files = this.diffFiles(mbFiles, headFiles, repo);
    // Conflict: a file changed head-vs-mergebase AND changed baseTip-vs-mergebase
    // to a different value.
    let conflict = false;
    if (mb !== null) {
      const baseCommit = repo.objects.get(baseSha);
      const baseTree = baseCommit && baseCommit.type === 'commit' ? this.parseCommit(baseCommit.data).tree : null;
      const baseFiles = this.flattenTree(repo, baseTree);
      for (const f of files) {
        const mbV = mbFiles.get(f.filename);
        const baseV = baseFiles.get(f.filename);
        const headV = headFiles.get(f.filename);
        const baseChanged = (baseV?.oid ?? null) !== (mbV?.oid ?? null);
        const headChanged = (headV?.oid ?? null) !== (mbV?.oid ?? null);
        if (baseChanged && headChanged && (baseV?.oid ?? null) !== (headV?.oid ?? null)) {
          conflict = true;
          break;
        }
      }
    }
    return { files, mergeBaseSha: mb, conflict };
  }
}
