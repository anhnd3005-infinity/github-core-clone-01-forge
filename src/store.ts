import {
  OID_REGEX,
  buildCommit,
  buildTreeFromPaths,
  createGitObject,
  diffTrees,
  flattenTree,
  findMergeBase,
  parseCommit,
  parseTree,
  peelOid,
  threeWayMergeTrees,
  type FileDiffItem,
  type GitObject,
  type PathBlobEntry,
} from './git-core.ts';

export interface ViewerInfo {
  readonly id: number;
  readonly login: string;
  readonly name: string;
  readonly email: string;
}

export interface LabelRecord {
  readonly name: string;
  readonly color: string;
}

export interface CommentRecord {
  readonly id: number;
  readonly globalId: string;
  readonly body: string;
  readonly createdAt: string;
  readonly seq: number;
}

export interface IssueRecord {
  readonly id: number;
  readonly globalId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: LabelRecord[];
  readonly createdAt: string;
  readonly seq: number;
  cachedJson?: Buffer;
}

export interface PullRecord {
  readonly id: number;
  readonly globalId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  readonly headRef: string;
  readonly headSha: string;
  readonly baseRef: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly createdAt: string;
}

export interface RepoRecord {
  readonly id: number;
  readonly globalId: string;
  readonly owner: string;
  readonly name: string;
  readonly fullName: string;
  readonly private: boolean;
  description: string;
  defaultBranch: string;
  readonly createdAt: string;
  readonly objects: Map<string, GitObject>;
  readonly commitParents: Map<string, readonly string[]>;
  readonly refs: Map<string, string>;
  refVersion: number;
  receiveRefsCache?: { version: number; buf: Buffer };
  readonly lsRefsCache: Map<string, { version: number; buf: Buffer }>;
  readonly shallowFetchCache: Map<string, { version: number; buf: Buffer }>;
  readonly labels: Map<string, LabelRecord>;
  nextNumber: number;
  readonly issues: IssueRecord[];
  readonly issuesByNumber: Map<number, IssueRecord>;
  readonly pulls: PullRecord[];
  readonly pullsByNumber: Map<number, PullRecord>;
  readonly commentsByNumber: Map<number, CommentRecord[]>;
}

export class StoreError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'StoreError';
    this.status = status;
  }
}

export function encodeGlobalId(kind: 'Repo' | 'Issue' | 'Pull' | 'Comment', a: number, b?: number): string {
  const raw = b === undefined ? `${kind}:${a}` : `${kind}:${a}:${b}`;
  return Buffer.from(raw, 'utf8').toString('base64url');
}

export function decodeGlobalId(globalId: string): { kind: string; a: number; b?: number } | null {
  try {
    const text = Buffer.from(globalId, 'base64url').toString('utf8');
    const parts = text.split(':');
    if (parts.length < 2 || parts.length > 3) return null;
    const a = Number(parts[1]);
    if (!Number.isSafeInteger(a)) return null;
    if (parts.length === 2) return { kind: parts[0]!, a };
    const b = Number(parts[2]);
    if (!Number.isSafeInteger(b)) return null;
    return { kind: parts[0]!, a, b };
  } catch {
    return null;
  }
}

function parseTrialEpochSec(rawNow: string | undefined): number {
  if (!rawNow) return 1_735_689_600;
  const parsed = Date.parse(rawNow);
  return Number.isFinite(parsed) ? Math.floor(parsed / 1_000) : 1_735_689_600;
}

export function serializeIssue(issue: IssueRecord): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: issue.globalId,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels,
    created_at: issue.createdAt,
  };
}

export function serializeRepo(repo: RepoRecord): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: repo.globalId,
    name: repo.name,
    full_name: repo.fullName,
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    owner: {
      login: repo.owner,
    },
  };
}

export function serializePull(pull: PullRecord): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: pull.globalId,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: {
      ref: pull.headRef,
      sha: pull.headSha,
    },
    base: {
      ref: pull.baseRef,
      sha: pull.baseSha,
    },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    created_at: pull.createdAt,
  };
}

export class ForgeStore {
  readonly viewer: ViewerInfo;
  private readonly baseEpochSec: number;
  private clockSeq = 0;
  private nextRepoId = 50_000;
  private nextIssueEntityId = 100_000;
  private nextPullEntityId = 200_000;
  private nextCommentId = 300_000;
  private readonly reposByName = new Map<string, RepoRecord>();
  private readonly reposById = new Map<number, RepoRecord>();
  private readonly issueByGlobalId = new Map<string, { repo: RepoRecord; issue: IssueRecord }>();
  private readonly pullByGlobalId = new Map<string, { repo: RepoRecord; pull: PullRecord }>();

  constructor(options: { viewerLogin?: string; trialNow?: string } = {}) {
    const login = options.viewerLogin && options.viewerLogin.trim() !== '' ? options.viewerLogin.trim() : 'adaiversen';
    this.viewer = {
      id: 1000,
      login,
      name: login === 'adaiversen' ? 'ada iversen' : login,
      email: `${login}@example.invalid`,
    };
    this.baseEpochSec = parseTrialEpochSec(options.trialNow);
  }

  private nextClock(): { epochSec: number; iso: string; seq: number } {
    this.clockSeq += 1;
    const epochSec = this.baseEpochSec + this.clockSeq;
    return {
      epochSec,
      iso: new Date(epochSec * 1_000).toISOString(),
      seq: this.clockSeq,
    };
  }

  saveGitObject(repo: RepoRecord, obj: GitObject): GitObject {
    const existing = repo.objects.get(obj.oid);
    if (existing) {
      if (!existing.deflated && obj.deflated) existing.deflated = obj.deflated;
      return existing;
    }
    repo.objects.set(obj.oid, obj);
    if (obj.type === 'commit') {
      repo.commitParents.set(obj.oid, parseCommit(obj.data).parents);
    }
    return obj;
  }

  bumpRefVersion(repo: RepoRecord): void {
    repo.refVersion += 1;
    repo.lsRefsCache.clear();
    repo.shallowFetchCache.clear();
  }

  requireRepo(owner: string, name: string): RepoRecord {
    if (owner !== this.viewer.login) {
      throw new StoreError(404, 'Not Found');
    }
    const repo = this.reposByName.get(name);
    if (!repo) {
      throw new StoreError(404, 'Not Found');
    }
    return repo;
  }

  getRepoIfExists(owner: string, name: string): RepoRecord | null {
    if (owner !== this.viewer.login) return null;
    return this.reposByName.get(name) ?? null;
  }

  getRepoByGlobalId(globalId: string): RepoRecord | null {
    const decoded = decodeGlobalId(globalId);
    if (!decoded || decoded.kind !== 'Repo') return null;
    return this.reposById.get(decoded.a) ?? null;
  }

  getIssueByGlobalId(globalId: string): { repo: RepoRecord; issue: IssueRecord } | null {
    return this.issueByGlobalId.get(globalId) ?? null;
  }

  getPullByGlobalId(globalId: string): { repo: RepoRecord; pull: PullRecord } | null {
    return this.pullByGlobalId.get(globalId) ?? null;
  }

  createRepo(input: { name?: unknown; description?: unknown; private?: unknown }): RepoRecord {
    if (typeof input.name !== 'string' || input.name.trim() === '') {
      throw new StoreError(422, 'Repository name is required');
    }
    const name = input.name.trim();
    if (this.reposByName.has(name)) {
      throw new StoreError(422, 'Repository creation failed: name already exists on this account');
    }
    const id = ++this.nextRepoId;
    const globalId = encodeGlobalId('Repo', id);
    const createdAt = this.nextClock().iso;
    const repo: RepoRecord = {
      id,
      globalId,
      owner: this.viewer.login,
      name,
      fullName: `${this.viewer.login}/${name}`,
      private: Boolean(input.private),
      description: typeof input.description === 'string' ? input.description : '',
      defaultBranch: 'main',
      createdAt,
      objects: new Map(),
      commitParents: new Map(),
      refs: new Map(),
      refVersion: 1,
      lsRefsCache: new Map(),
      shallowFetchCache: new Map(),
      labels: new Map(),
      nextNumber: 1,
      issues: [],
      issuesByNumber: new Map(),
      pulls: [],
      pullsByNumber: new Map(),
      commentsByNumber: new Map(),
    };
    this.reposByName.set(name, repo);
    this.reposById.set(id, repo);
    return repo;
  }

  listViewerRepos(): RepoRecord[] {
    return [...this.reposByName.values()].sort((a, b) => a.id - b.id);
  }

  patchRepo(owner: string, name: string, input: { description?: unknown; private?: unknown; default_branch?: unknown }): RepoRecord {
    const repo = this.requireRepo(owner, name);
    if (typeof input.description === 'string') repo.description = input.description;
    if (typeof input.private === 'boolean') repo.private = input.private;
    if (typeof input.default_branch === 'string' && input.default_branch.trim() !== '') {
      repo.defaultBranch = input.default_branch.trim();
    }
    return repo;
  }

  deleteRepo(owner: string, name: string): void {
    const repo = this.requireRepo(owner, name);
    this.reposByName.delete(name);
    this.reposById.delete(repo.id);
    for (const issue of repo.issues) this.issueByGlobalId.delete(issue.globalId);
    for (const pull of repo.pulls) this.pullByGlobalId.delete(pull.globalId);
  }

  private resolveCommitFromRefOrOid(repo: RepoRecord, refSpec?: string): { refName: string | null; commitOid: string | null } {
    const target = refSpec && refSpec.trim() !== '' ? refSpec.trim() : repo.defaultBranch;
    const candidates = target.startsWith('refs/')
      ? [target]
      : [`refs/heads/${target}`, `refs/tags/${target}`];
    for (const candidate of candidates) {
      const oid = repo.refs.get(candidate);
      if (oid) {
        return { refName: candidate, commitOid: peelOid(repo.objects, oid) };
      }
    }
    if (OID_REGEX.test(target) && repo.objects.has(target)) {
      return { refName: null, commitOid: peelOid(repo.objects, target) };
    }
    return {
      refName: target.startsWith('refs/') ? target : `refs/heads/${target}`,
      commitOid: null,
    };
  }

  private authorLine(epochSec: number): string {
    return `${this.viewer.name} <${this.viewer.email}> ${epochSec} +0000`;
  }

  putContent(
    owner: string,
    name: string,
    rawPath: string,
    input: { message?: unknown; content?: unknown; branch?: unknown; sha?: unknown },
  ): { status: 200 | 201; content: Record<string, unknown>; commit: Record<string, unknown> } {
    const repo = this.requireRepo(owner, name);
    const cleanPath = rawPath.replace(/^\/+|\/+$/g, '');
    if (!cleanPath) throw new StoreError(422, 'Invalid file path');
    if (typeof input.content !== 'string') throw new StoreError(422, 'content must be base64 string');
    const message = typeof input.message === 'string' && input.message !== '' ? input.message : `Update ${cleanPath}`;
    const branch = typeof input.branch === 'string' && input.branch.trim() !== '' ? input.branch.trim() : repo.defaultBranch;
    const refName = `refs/heads/${branch}`;

    let parentOid = repo.refs.get(refName) ?? null;
    let implicitParent = parentOid;
    if (!implicitParent && branch !== repo.defaultBranch) {
      implicitParent = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    }

    const pathMap = implicitParent ? flattenTree(repo.objects, implicitParent) : new Map<string, PathBlobEntry>();
    const existing = pathMap.get(cleanPath);
    if (typeof input.sha === 'string') {
      if (!existing || existing.oid !== input.sha.toLowerCase()) {
        throw new StoreError(409, `${cleanPath} does not match ${input.sha}`);
      }
    }

    const bytes = Buffer.from(input.content.replace(/\s+/g, ''), 'base64');
    const blobObj = this.saveGitObject(repo, createGitObject('blob', bytes));
    pathMap.set(cleanPath, { mode: existing?.mode ?? '100644', oid: blobObj.oid });

    const treeOid = buildTreeFromPaths((o) => this.saveGitObject(repo, o), pathMap);
    const clock = this.nextClock();
    const who = this.authorLine(clock.epochSec);
    const parents = implicitParent ? [implicitParent] : [];
    const commitObj = this.saveGitObject(
      repo,
      createGitObject(
        'commit',
        buildCommit({ tree: treeOid, parents, author: who, committer: who, message }),
      ),
    );
    repo.refs.set(refName, commitObj.oid);
    this.bumpRefVersion(repo);

    return {
      status: existing ? 200 : 201,
      content: {
        name: cleanPath.split('/').pop() ?? cleanPath,
        path: cleanPath,
        sha: blobObj.oid,
        size: bytes.length,
        encoding: 'base64',
        type: 'file',
      },
      commit: {
        sha: commitObj.oid,
        message,
      },
    };
  }

  getContent(
    owner: string,
    name: string,
    rawPath: string,
    ref?: string,
  ): Record<string, unknown> | Record<string, unknown>[] {
    const repo = this.requireRepo(owner, name);
    const cleanPath = rawPath.replace(/^\/+|\/+$/g, '');
    const resolved = this.resolveCommitFromRefOrOid(repo, ref);
    if (!resolved.commitOid) throw new StoreError(404, 'Not Found');
    const commitObj = repo.objects.get(resolved.commitOid);
    if (!commitObj || commitObj.type !== 'commit') throw new StoreError(404, 'Not Found');
    let currentTreeOid = parseCommit(commitObj.data).tree;

    if (cleanPath === '') {
      const rootObj = repo.objects.get(currentTreeOid);
      if (!rootObj || rootObj.type !== 'tree') throw new StoreError(404, 'Not Found');
      return this.formatDirectoryEntries(repo, '', rootObj.data);
    }

    const parts = cleanPath.split('/').filter(Boolean);
    for (let i = 0; i < parts.length; i += 1) {
      const segment = parts[i]!;
      const treeObj = repo.objects.get(currentTreeOid);
      if (!treeObj || treeObj.type !== 'tree') throw new StoreError(404, 'Not Found');
      const entry = parseTree(treeObj.data).find((item) => item.name === segment);
      if (!entry) throw new StoreError(404, 'Not Found');

      if (i === parts.length - 1) {
        if (entry.mode === '40000') {
          const dirObj = repo.objects.get(entry.oid);
          if (!dirObj || dirObj.type !== 'tree') throw new StoreError(404, 'Not Found');
          return this.formatDirectoryEntries(repo, cleanPath, dirObj.data);
        }
        const blobObj = repo.objects.get(entry.oid);
        if (!blobObj || blobObj.type !== 'blob') throw new StoreError(404, 'Not Found');
        return {
          type: 'file',
          encoding: 'base64',
          content: blobObj.data.toString('base64'),
          sha: blobObj.oid,
          size: blobObj.data.length,
          name: segment,
          path: cleanPath,
        };
      }

      if (entry.mode !== '40000') throw new StoreError(404, 'Not Found');
      currentTreeOid = entry.oid;
    }
    throw new StoreError(404, 'Not Found');
  }

  private formatDirectoryEntries(repo: RepoRecord, prefix: string, treeData: Buffer): Record<string, unknown>[] {
    return parseTree(treeData).map((entry) => {
      const isDir = entry.mode === '40000';
      const blob = isDir ? undefined : repo.objects.get(entry.oid);
      return {
        type: isDir ? 'dir' : 'file',
        name: entry.name,
        path: prefix ? `${prefix}/${entry.name}` : entry.name,
        sha: entry.oid,
        size: blob ? blob.data.length : 0,
      };
    });
  }

  deleteContent(
    owner: string,
    name: string,
    rawPath: string,
    input: { message?: unknown; sha?: unknown; branch?: unknown },
  ): { commit: Record<string, unknown> } {
    const repo = this.requireRepo(owner, name);
    const cleanPath = rawPath.replace(/^\/+|\/+$/g, '');
    const branch = typeof input.branch === 'string' && input.branch.trim() !== '' ? input.branch.trim() : repo.defaultBranch;
    const refName = `refs/heads/${branch}`;
    const tipOid = repo.refs.get(refName);
    if (!tipOid) throw new StoreError(404, 'Not Found');

    const pathMap = flattenTree(repo.objects, tipOid);
    const existing = pathMap.get(cleanPath);
    if (!existing) throw new StoreError(404, 'Not Found');
    if (typeof input.sha === 'string' && existing.oid !== input.sha.toLowerCase()) {
      throw new StoreError(409, 'sha does not match');
    }

    pathMap.delete(cleanPath);
    const treeOid = buildTreeFromPaths((o) => this.saveGitObject(repo, o), pathMap);
    const clock = this.nextClock();
    const who = this.authorLine(clock.epochSec);
    const message = typeof input.message === 'string' && input.message !== '' ? input.message : `Delete ${cleanPath}`;
    const commitObj = this.saveGitObject(
      repo,
      createGitObject(
        'commit',
        buildCommit({ tree: treeOid, parents: [tipOid], author: who, committer: who, message }),
      ),
    );
    repo.refs.set(refName, commitObj.oid);
    this.bumpRefVersion(repo);
    return { commit: { sha: commitObj.oid, message } };
  }

  createRef(owner: string, name: string, input: { ref?: unknown; sha?: unknown }): { ref: string; object: { sha: string; type: 'commit' } } {
    const repo = this.requireRepo(owner, name);
    if (typeof input.ref !== 'string' || typeof input.sha !== 'string') {
      throw new StoreError(422, 'ref and sha are required');
    }
    const refName = input.ref.startsWith('refs/') ? input.ref : `refs/heads/${input.ref}`;
    const sha = input.sha.toLowerCase();
    if (!OID_REGEX.test(sha) || !repo.objects.has(sha)) {
      throw new StoreError(422, 'Object does not exist');
    }
    if (repo.refs.has(refName)) {
      throw new StoreError(422, 'Reference already exists');
    }
    repo.refs.set(refName, sha);
    this.bumpRefVersion(repo);
    return { ref: refName, object: { sha, type: 'commit' } };
  }

  getRef(owner: string, name: string, refPath: string): { ref: string; object: { sha: string; type: 'commit' } } {
    const repo = this.requireRepo(owner, name);
    const fullRef = refPath.startsWith('refs/') ? refPath : `refs/${refPath}`;
    const oid = repo.refs.get(fullRef);
    if (!oid) throw new StoreError(404, 'Not Found');
    return { ref: fullRef, object: { sha: oid, type: 'commit' } };
  }

  listBranches(owner: string, name: string): Array<{ name: string; commit: { sha: string } }> {
    const repo = this.requireRepo(owner, name);
    const branches: Array<{ name: string; commit: { sha: string } }> = [];
    for (const [ref, oid] of repo.refs.entries()) {
      if (ref.startsWith('refs/heads/')) {
        branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha: oid } });
      }
    }
    branches.sort((a, b) => (a.name === repo.defaultBranch ? -1 : b.name === repo.defaultBranch ? 1 : a.name.localeCompare(b.name)));
    return branches;
  }

  getBranch(owner: string, name: string, branch: string): { name: string; commit: { sha: string } } {
    const repo = this.requireRepo(owner, name);
    const oid = repo.refs.get(`refs/heads/${branch}`);
    if (!oid) throw new StoreError(404, 'Branch not found');
    return { name: branch, commit: { sha: oid } };
  }

  listCommits(owner: string, name: string, shaParam?: string): Array<{ sha: string; commit: { message: string } }> {
    const repo = this.requireRepo(owner, name);
    const resolved = this.resolveCommitFromRefOrOid(repo, shaParam);
    if (!resolved.commitOid) return [];

    const out: Array<{ sha: string; commit: { message: string } }> = [];
    const seen = new Set<string>();
    const queue: string[] = [resolved.commitOid];
    while (queue.length > 0 && out.length < 100) {
      const oid = queue.shift()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const parsed = parseCommit(obj.data);
      out.push({ sha: oid, commit: { message: parsed.message } });
      for (const parent of parsed.parents) {
        if (!seen.has(parent)) queue.push(parent);
      }
    }
    return out;
  }

  createLabel(owner: string, name: string, input: { name?: unknown; color?: unknown }): LabelRecord {
    const repo = this.requireRepo(owner, name);
    if (typeof input.name !== 'string' || input.name.trim() === '') {
      throw new StoreError(422, 'Label name is required');
    }
    const labelName = input.name.trim();
    const color = typeof input.color === 'string' && input.color.trim() !== '' ? input.color.trim() : 'ededed';
    const label: LabelRecord = { name: labelName, color };
    repo.labels.set(labelName, label);
    return label;
  }

  listLabels(owner: string, name: string): LabelRecord[] {
    const repo = this.requireRepo(owner, name);
    return [...repo.labels.values()];
  }

  createIssue(
    owner: string,
    name: string,
    input: { title?: unknown; body?: unknown; labels?: unknown },
  ): IssueRecord {
    const repo = this.requireRepo(owner, name);
    if (typeof input.title !== 'string' || input.title.trim() === '') {
      throw new StoreError(422, 'Issue title is required');
    }
    const labelRecords: LabelRecord[] = [];
    if (input.labels !== undefined) {
      if (!Array.isArray(input.labels)) throw new StoreError(422, 'labels must be an array');
      for (const item of input.labels) {
        const labelName = typeof item === 'string' ? item : '';
        const found = repo.labels.get(labelName);
        if (!found) {
          throw new StoreError(422, `Validation Failed: unknown label ${JSON.stringify(labelName)}`);
        }
        labelRecords.push(found);
      }
    }

    const num = repo.nextNumber++;
    const entityId = ++this.nextIssueEntityId;
    const globalId = encodeGlobalId('Issue', repo.id, num);
    const clock = this.nextClock();
    const issue: IssueRecord = {
      id: entityId,
      globalId,
      number: num,
      title: input.title,
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      labels: labelRecords,
      createdAt: clock.iso,
      seq: clock.seq,
    };
    issue.cachedJson = Buffer.from(JSON.stringify(serializeIssue(issue)), 'utf8');
    repo.issues.push(issue);
    repo.issuesByNumber.set(num, issue);
    this.issueByGlobalId.set(globalId, { repo, issue });
    return issue;
  }

  listIssues(
    owner: string,
    name: string,
    opts: { state?: string; direction?: string } = {},
  ): IssueRecord[] {
    const repo = this.requireRepo(owner, name);
    const state = opts.state ?? 'open';
    const filtered = state === 'all' ? [...repo.issues] : repo.issues.filter((issue) => issue.state === state);
    const asc = opts.direction === 'asc';
    filtered.sort((a, b) => (asc ? a.seq - b.seq : b.seq - a.seq));
    return filtered;
  }

  getIssue(owner: string, name: string, number: number): IssueRecord {
    const repo = this.requireRepo(owner, name);
    const issue = repo.issuesByNumber.get(number);
    if (!issue) throw new StoreError(404, 'Not Found');
    return issue;
  }

  updateIssue(
    owner: string,
    name: string,
    number: number,
    input: { state?: unknown; title?: unknown; body?: unknown },
  ): IssueRecord {
    const repo = this.requireRepo(owner, name);
    const issue = repo.issuesByNumber.get(number);
    if (!issue) throw new StoreError(404, 'Not Found');
    if (input.state === 'open' || input.state === 'closed') issue.state = input.state;
    if (typeof input.title === 'string') issue.title = input.title;
    if (typeof input.body === 'string') issue.body = input.body;
    issue.cachedJson = Buffer.from(JSON.stringify(serializeIssue(issue)), 'utf8');
    return issue;
  }

  addComment(owner: string, name: string, number: number, body: unknown): CommentRecord {
    const repo = this.requireRepo(owner, name);
    if (!repo.issuesByNumber.has(number) && !repo.pullsByNumber.has(number)) {
      throw new StoreError(404, 'Not Found');
    }
    if (typeof body !== 'string' || body === '') {
      throw new StoreError(422, 'Comment body is required');
    }
    const id = ++this.nextCommentId;
    const globalId = encodeGlobalId('Comment', id);
    const clock = this.nextClock();
    const record: CommentRecord = {
      id,
      globalId,
      body,
      createdAt: clock.iso,
      seq: clock.seq,
    };
    let list = repo.commentsByNumber.get(number);
    if (!list) {
      list = [];
      repo.commentsByNumber.set(number, list);
    }
    list.push(record);
    return record;
  }

  listComments(owner: string, name: string, number: number): CommentRecord[] {
    const repo = this.requireRepo(owner, name);
    if (!repo.issuesByNumber.has(number) && !repo.pullsByNumber.has(number)) {
      throw new StoreError(404, 'Not Found');
    }
    return repo.commentsByNumber.get(number) ?? [];
  }

  createPull(
    owner: string,
    name: string,
    input: { title?: unknown; body?: unknown; head?: unknown; base?: unknown },
  ): PullRecord {
    const repo = this.requireRepo(owner, name);
    if (typeof input.title !== 'string' || typeof input.head !== 'string' || typeof input.base !== 'string') {
      throw new StoreError(422, 'title, head, and base are required');
    }
    const headRef = input.head.replace(/^refs\/heads\//, '');
    const baseRef = input.base.replace(/^refs\/heads\//, '');
    const headSha = repo.refs.get(`refs/heads/${headRef}`);
    const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
    if (!headSha || !baseSha) {
      throw new StoreError(422, 'head or base branch does not exist');
    }
    const num = repo.nextNumber++;
    const id = ++this.nextPullEntityId;
    const globalId = encodeGlobalId('Pull', repo.id, num);
    const clock = this.nextClock();
    const pull: PullRecord = {
      id,
      globalId,
      number: num,
      title: input.title,
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      headRef,
      headSha,
      baseRef,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      createdAt: clock.iso,
    };
    repo.pulls.push(pull);
    repo.pullsByNumber.set(num, pull);
    this.pullByGlobalId.set(globalId, { repo, pull });
    return pull;
  }

  listPulls(owner: string, name: string, state = 'open'): PullRecord[] {
    const repo = this.requireRepo(owner, name);
    if (state === 'all') return [...repo.pulls];
    return repo.pulls.filter((pull) => pull.state === state);
  }

  getPull(owner: string, name: string, number: number): PullRecord {
    const repo = this.requireRepo(owner, name);
    const pull = repo.pullsByNumber.get(number);
    if (!pull) throw new StoreError(404, 'Not Found');
    return pull;
  }

  getPullFiles(owner: string, name: string, number: number): FileDiffItem[] {
    const repo = this.requireRepo(owner, name);
    const pull = this.getPull(owner, name, number);
    const currentHead = repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha;
    const currentBase = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
    const ancOid = findMergeBase((oid) => repo.commitParents.get(oid), currentBase, currentHead);
    const ancMap = ancOid ? flattenTree(repo.objects, ancOid) : flattenTree(repo.objects, pull.baseSha);
    const headMap = flattenTree(repo.objects, currentHead);
    return diffTrees(repo.objects, ancMap, headMap);
  }

  computePullMerge(
    repo: RepoRecord,
    pull: PullRecord,
  ): { ok: true; treeOid: string; baseTip: string; headTip: string } | { ok: false } {
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
    const headTip = repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha;
    const ancOid = findMergeBase((oid) => repo.commitParents.get(oid), baseTip, headTip);
    const ancMap = ancOid ? flattenTree(repo.objects, ancOid) : new Map<string, PathBlobEntry>();
    const baseMap = flattenTree(repo.objects, baseTip);
    const headMap = flattenTree(repo.objects, headTip);
    const merged = threeWayMergeTrees((o) => this.saveGitObject(repo, o), ancMap, baseMap, headMap);
    if (!merged.ok) return { ok: false };
    return { ok: true, treeOid: merged.treeOid, baseTip, headTip };
  }

  mergePull(
    owner: string,
    name: string,
    number: number,
    mergeMethod = 'squash',
  ): { sha: string; merged: true; message: string } {
    const repo = this.requireRepo(owner, name);
    const pull = repo.pullsByNumber.get(number);
    if (!pull) throw new StoreError(404, 'Not Found');
    if (pull.merged) throw new StoreError(405, 'Pull Request is already merged');

    const check = this.computePullMerge(repo, pull);
    if (!check.ok) {
      throw new StoreError(405, 'Pull Request is not mergeable');
    }

    const clock = this.nextClock();
    const who = this.authorLine(clock.epochSec);
    const normalized = mergeMethod.toLowerCase();
    const parents = normalized === 'merge' ? [check.baseTip, check.headTip] : [check.baseTip];
    const commitObj = this.saveGitObject(
      repo,
      createGitObject(
        'commit',
        buildCommit({
          tree: check.treeOid,
          parents,
          author: who,
          committer: who,
          message: `Merge pull request #${pull.number}: ${pull.title}`,
        }),
      ),
    );
    repo.refs.set(`refs/heads/${pull.baseRef}`, commitObj.oid);
    this.bumpRefVersion(repo);

    pull.baseSha = commitObj.oid;
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = clock.iso;
    pull.mergeCommitSha = commitObj.oid;

    return {
      sha: commitObj.oid,
      merged: true,
      message: 'Pull Request successfully merged',
    };
  }
}
