import type { IncomingMessage, ServerResponse } from 'node:http';
import { isAuthorizedRequest } from './rest.ts';
import {
  ForgeStore,
  StoreError,
  serializeIssue,
  serializePull,
  serializeRepo,
  type IssueRecord,
  type PullRecord,
  type RepoRecord,
} from './store.ts';

interface GqlArg {
  readonly name: string;
  readonly value: GqlValue;
}

type GqlValue =
  | { readonly kind: 'var'; readonly name: string }
  | { readonly kind: 'int'; readonly value: number }
  | { readonly kind: 'float'; readonly value: number }
  | { readonly kind: 'string'; readonly value: string }
  | { readonly kind: 'bool'; readonly value: boolean }
  | { readonly kind: 'null' }
  | { readonly kind: 'enum'; readonly value: string }
  | { readonly kind: 'list'; readonly items: readonly GqlValue[] }
  | { readonly kind: 'object'; readonly fields: ReadonlyMap<string, GqlValue> };

type GqlSelection =
  | {
      readonly kind: 'field';
      readonly name: string;
      readonly alias?: string;
      readonly args: readonly GqlArg[];
      readonly selectionSet?: readonly GqlSelection[];
    }
  | {
      readonly kind: 'inlineFragment';
      readonly typeCondition?: string;
      readonly selectionSet: readonly GqlSelection[];
    }
  | {
      readonly kind: 'fragmentSpread';
      readonly name: string;
    };

interface GqlOperation {
  readonly opType: 'query' | 'mutation';
  readonly name?: string;
  readonly selectionSet: readonly GqlSelection[];
}

interface GqlFragment {
  readonly name: string;
  readonly typeCondition: string;
  readonly selectionSet: readonly GqlSelection[];
}

interface Token {
  readonly kind: 'name' | 'int' | 'float' | 'string' | 'punct' | 'spread';
  readonly value: string;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const n = source.length;

  while (i < n) {
    const ch = source[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '\ufeff') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < n && source[i] !== '\n' && source[i] !== '\r') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}():=@$&![]|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      if (source.slice(i, i + 3) === '"""') {
        i += 3;
        let val = '';
        while (i < n && source.slice(i, i + 3) !== '"""') {
          if (source[i] === '\\' && source.slice(i, i + 4) === '\\"""') {
            val += '"""';
            i += 4;
          } else {
            val += source[i++];
          }
        }
        if (source.slice(i, i + 3) !== '"""') throw new Error('Unterminated block string');
        i += 3;
        tokens.push({ kind: 'string', value: val });
        continue;
      }
      i += 1;
      let val = '';
      while (i < n && source[i] !== '"') {
        if (source[i] === '\\') {
          i += 1;
          const esc = source[i++];
          if (esc === 'n') val += '\n';
          else if (esc === 'r') val += '\r';
          else if (esc === 't') val += '\t';
          else if (esc === 'b') val += '\b';
          else if (esc === 'f') val += '\f';
          else if (esc === 'u') {
            const hex = source.slice(i, i + 4);
            val += String.fromCharCode(parseInt(hex, 16));
            i += 4;
          } else if (esc !== undefined) {
            val += esc;
          }
        } else {
          val += source[i++];
        }
      }
      if (source[i] !== '"') throw new Error('Unterminated string literal');
      i += 1;
      tokens.push({ kind: 'string', value: val });
      continue;
    }
    if (ch === '-' || (ch >= '0' && ch <= '9')) {
      const start = i;
      if (source[i] === '-') i += 1;
      while (i < n && source[i]! >= '0' && source[i]! <= '9') i += 1;
      let isFloat = false;
      if (source[i] === '.' || source[i] === 'e' || source[i] === 'E') {
        isFloat = true;
        if (source[i] === '.') {
          i += 1;
          while (i < n && source[i]! >= '0' && source[i]! <= '9') i += 1;
        }
        if (source[i] === 'e' || source[i] === 'E') {
          i += 1;
          if (source[i] === '+' || source[i] === '-') i += 1;
          while (i < n && source[i]! >= '0' && source[i]! <= '9') i += 1;
        }
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: source.slice(start, i) });
      continue;
    }
    if ((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch === '_') {
      const start = i;
      i += 1;
      while (i < n) {
        const c = source[i]!;
        if ((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c === '_') {
          i += 1;
        } else {
          break;
        }
      }
      tokens.push({ kind: 'name', value: source.slice(start, i) });
      continue;
    }
    throw new Error(`Unexpected character in GraphQL document: ${JSON.stringify(ch)}`);
  }
  return tokens;
}

class Parser {
  private readonly tokens: readonly Token[];
  private pos = 0;

  constructor(tokens: readonly Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const tok = this.tokens[this.pos++];
    if (!tok) throw new Error('Unexpected end of GraphQL document');
    return tok;
  }

  private expectPunct(p: string): void {
    const tok = this.next();
    if (tok.kind !== 'punct' || tok.value !== p) {
      throw new Error(`Expected ${JSON.stringify(p)}, got ${JSON.stringify(tok.value)}`);
    }
  }

  private skipTypeRef(): void {
    const t = this.peek();
    if (t?.kind === 'punct' && t.value === '[') {
      this.next();
      this.skipTypeRef();
      this.expectPunct(']');
    } else {
      this.next();
    }
    while (this.peek()?.kind === 'punct' && this.peek()!.value === '!') {
      this.next();
    }
  }

  private parseValue(): GqlValue {
    const tok = this.next();
    if (tok.kind === 'punct' && tok.value === '$') {
      return { kind: 'var', name: this.next().value };
    }
    if (tok.kind === 'int') return { kind: 'int', value: Number(tok.value) };
    if (tok.kind === 'float') return { kind: 'float', value: Number(tok.value) };
    if (tok.kind === 'string') return { kind: 'string', value: tok.value };
    if (tok.kind === 'name') {
      if (tok.value === 'true') return { kind: 'bool', value: true };
      if (tok.value === 'false') return { kind: 'bool', value: false };
      if (tok.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: tok.value };
    }
    if (tok.kind === 'punct' && tok.value === '[') {
      const items: GqlValue[] = [];
      while (!(this.peek()?.kind === 'punct' && this.peek()!.value === ']')) {
        items.push(this.parseValue());
      }
      this.expectPunct(']');
      return { kind: 'list', items };
    }
    if (tok.kind === 'punct' && tok.value === '{') {
      const fields = new Map<string, GqlValue>();
      while (!(this.peek()?.kind === 'punct' && this.peek()!.value === '}')) {
        const key = this.next().value;
        this.expectPunct(':');
        fields.set(key, this.parseValue());
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    throw new Error(`Unexpected value token ${JSON.stringify(tok.value)}`);
  }

  private parseArgs(): GqlArg[] {
    if (!(this.peek()?.kind === 'punct' && this.peek()!.value === '(')) return [];
    this.next();
    const args: GqlArg[] = [];
    while (!(this.peek()?.kind === 'punct' && this.peek()!.value === ')')) {
      const name = this.next().value;
      this.expectPunct(':');
      const value = this.parseValue();
      args.push({ name, value });
    }
    this.expectPunct(')');
    return args;
  }

  private skipDirectives(): void {
    while (this.peek()?.kind === 'punct' && this.peek()!.value === '@') {
      this.next();
      this.next();
      this.parseArgs();
    }
  }

  private parseSelectionSet(): GqlSelection[] {
    this.expectPunct('{');
    const out: GqlSelection[] = [];
    while (!(this.peek()?.kind === 'punct' && this.peek()!.value === '}')) {
      const tok = this.peek();
      if (!tok) throw new Error('Unterminated selection set');
      if (tok.kind === 'spread') {
        this.next();
        const after = this.peek();
        if (after?.kind === 'name' && after.value === 'on') {
          this.next();
          const typeCondition = this.next().value;
          this.skipDirectives();
          out.push({ kind: 'inlineFragment', typeCondition, selectionSet: this.parseSelectionSet() });
        } else if (after?.kind === 'punct' && after.value === '{') {
          out.push({ kind: 'inlineFragment', selectionSet: this.parseSelectionSet() });
        } else {
          const fragName = this.next().value;
          this.skipDirectives();
          out.push({ kind: 'fragmentSpread', name: fragName });
        }
        continue;
      }
      const first = this.next().value;
      let alias: string | undefined;
      let name = first;
      if (this.peek()?.kind === 'punct' && this.peek()!.value === ':') {
        this.next();
        alias = first;
        name = this.next().value;
      }
      const args = this.parseArgs();
      this.skipDirectives();
      const selectionSet =
        this.peek()?.kind === 'punct' && this.peek()!.value === '{' ? this.parseSelectionSet() : undefined;
      out.push({ kind: 'field', name, alias, args, selectionSet });
    }
    this.expectPunct('}');
    return out;
  }

  parseDocument(): { operations: GqlOperation[]; fragments: Map<string, GqlFragment> } {
    const operations: GqlOperation[] = [];
    const fragments = new Map<string, GqlFragment>();

    while (this.pos < this.tokens.length) {
      const tok = this.peek()!;
      if (tok.kind === 'punct' && tok.value === '{') {
        operations.push({ opType: 'query', selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (tok.kind === 'name' && tok.value === 'fragment') {
        this.next();
        const name = this.next().value;
        const onTok = this.next();
        if (onTok.value !== 'on') throw new Error('Expected "on" in fragment definition');
        const typeCondition = this.next().value;
        this.skipDirectives();
        const selectionSet = this.parseSelectionSet();
        fragments.set(name, { name, typeCondition, selectionSet });
        continue;
      }
      if (tok.kind === 'name' && (tok.value === 'query' || tok.value === 'mutation' || tok.value === 'subscription')) {
        const opType = this.next().value === 'mutation' ? 'mutation' : 'query';
        let name: string | undefined;
        if (this.peek()?.kind === 'name') {
          name = this.next().value;
        }
        if (this.peek()?.kind === 'punct' && this.peek()!.value === '(') {
          this.next();
          while (!(this.peek()?.kind === 'punct' && this.peek()!.value === ')')) {
            this.expectPunct('$');
            this.next();
            this.expectPunct(':');
            this.skipTypeRef();
            if (this.peek()?.kind === 'punct' && this.peek()!.value === '=') {
              this.next();
              this.parseValue();
            }
          }
          this.expectPunct(')');
        }
        this.skipDirectives();
        const selectionSet = this.parseSelectionSet();
        operations.push({ opType, name, selectionSet });
        continue;
      }
      throw new Error(`Unexpected top-level token ${JSON.stringify(tok.value)}`);
    }

    return { operations, fragments };
  }
}

const TYPE_FIELDS_MAP: Readonly<Record<string, readonly string[]>> = {
  Query: ['viewer', 'repository', 'node', '__type'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['login', 'name', 'id'],
  Ref: ['name'],
  PageInfo: ['hasNextPage', 'endCursor'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'databaseId', 'createdAt'],
  IssueConnection: ['nodes', 'pageInfo'],
  PullRequestChangedFile: ['path'],
  PullRequestChangedFileConnection: ['nodes'],
  PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files', 'title', 'body'],
  PullRequestConnection: ['nodes'],
  Repository: [
    'id',
    'databaseId',
    'name',
    'nameWithOwner',
    'owner',
    'defaultBranchRef',
    'viewerPermission',
    'hasIssuesEnabled',
    'issue',
    'issues',
    'pullRequests',
    'pullRequest',
    'description',
    'isPrivate',
  ],
  __Type: ['name', 'fields', 'enumValues'],
  __Field: ['name'],
  __EnumValue: ['name'],
  CreateIssuePayload: ['issue'],
  CloseIssuePayload: ['issue'],
  CreatePullRequestPayload: ['pullRequest'],
  MergePullRequestPayload: ['pullRequest'],
  Comment: ['id', 'body'],
  CommentEdge: ['node'],
  AddCommentPayload: ['commentEdge'],
};

const ENUM_VALUES_MAP: Readonly<Record<string, readonly string[]>> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

function resolveArgValue(val: GqlValue, vars: Readonly<Record<string, unknown>>): unknown {
  switch (val.kind) {
    case 'var':
      return vars[val.name] ?? null;
    case 'int':
    case 'float':
    case 'string':
    case 'bool':
    case 'enum':
      return val.value;
    case 'null':
      return null;
    case 'list':
      return val.items.map((item) => resolveArgValue(item, vars));
    case 'object': {
      const obj: Record<string, unknown> = {};
      for (const [k, v] of val.fields.entries()) {
        obj[k] = resolveArgValue(v, vars);
      }
      return obj;
    }
  }
}

function getArgs(args: readonly GqlArg[], vars: Readonly<Record<string, unknown>>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of args) {
    out[arg.name] = resolveArgValue(arg.value, vars);
  }
  return out;
}

function encodeCursor(index: number): string {
  return Buffer.from(`idx:${index}`, 'utf8').toString('base64url');
}

function decodeCursor(cursor: string): number {
  try {
    const raw = Buffer.from(cursor, 'base64url').toString('utf8');
    if (raw.startsWith('idx:')) {
      const n = Number(raw.slice(4));
      return Number.isInteger(n) && n >= 0 ? n : -1;
    }
    return -1;
  } catch {
    return -1;
  }
}

interface ExecCtx {
  readonly store: ForgeStore;
  readonly vars: Readonly<Record<string, unknown>>;
  readonly fragments: ReadonlyMap<string, GqlFragment>;
  readonly errors: Array<{ message: string }>;
}

function collectFields(
  selections: readonly GqlSelection[],
  typeName: string,
  fragments: ReadonlyMap<string, GqlFragment>,
): Array<Extract<GqlSelection, { kind: 'field' }>> {
  const out: Array<Extract<GqlSelection, { kind: 'field' }>> = [];
  for (const sel of selections) {
    if (sel.kind === 'field') {
      out.push(sel);
    } else if (sel.kind === 'inlineFragment') {
      if (!sel.typeCondition || sel.typeCondition === typeName) {
        out.push(...collectFields(sel.selectionSet, typeName, fragments));
      }
    } else if (sel.kind === 'fragmentSpread') {
      const frag = fragments.get(sel.name);
      if (frag && frag.typeCondition === typeName) {
        out.push(...collectFields(frag.selectionSet, typeName, fragments));
      }
    }
  }
  return out;
}

function selectObject(
  typeName: string,
  selectionSet: readonly GqlSelection[] | undefined,
  ctx: ExecCtx,
  resolveField: (name: string, args: Record<string, unknown>, fieldSel: Extract<GqlSelection, { kind: 'field' }>) => unknown,
): Record<string, unknown> {
  if (!selectionSet) return {};
  const result: Record<string, unknown> = {};
  const allowed = TYPE_FIELDS_MAP[typeName];
  const fields = collectFields(selectionSet, typeName, ctx.fragments);

  for (const field of fields) {
    const key = field.alias ?? field.name;
    if (field.name === '__typename') {
      result[key] = typeName;
      continue;
    }
    if (allowed && !allowed.includes(field.name)) {
      ctx.errors.push({ message: `Field '${field.name}' doesn't exist on type '${typeName}'` });
      continue;
    }
    const args = getArgs(field.args, ctx.vars);
    result[key] = resolveField(field.name, args, field);
  }
  return result;
}

function projectIssue(issue: IssueRecord, selectionSet: readonly GqlSelection[] | undefined, ctx: ExecCtx): Record<string, unknown> {
  return selectObject('Issue', selectionSet, ctx, (name) => {
    switch (name) {
      case 'id':
        return issue.globalId;
      case 'databaseId':
        return issue.id;
      case 'number':
        return issue.number;
      case 'title':
        return issue.title;
      case 'body':
        return issue.body;
      case 'state':
        return issue.state.toUpperCase();
      case 'createdAt':
        return issue.createdAt;
      default:
        return null;
    }
  });
}

function projectPull(
  repo: RepoRecord,
  pull: PullRecord,
  selectionSet: readonly GqlSelection[] | undefined,
  ctx: ExecCtx,
): Record<string, unknown> {
  return selectObject('PullRequest', selectionSet, ctx, (name, args, fieldSel) => {
    switch (name) {
      case 'id':
        return pull.globalId;
      case 'number':
        return pull.number;
      case 'title':
        return pull.title;
      case 'body':
        return pull.body;
      case 'headRefName':
        return pull.headRef;
      case 'baseRefName':
        return pull.baseRef;
      case 'merged':
        return pull.merged;
      case 'state':
        return pull.merged ? 'MERGED' : pull.state.toUpperCase();
      case 'mergeable':
        return ctx.store.computePullMerge(repo, pull).ok ? 'MERGEABLE' : 'CONFLICTING';
      case 'files': {
        const first = typeof args.first === 'number' && args.first >= 0 ? args.first : 100;
        const files = ctx.store.getPullFiles(repo.owner, repo.name, pull.number).slice(0, first);
        return selectObject('PullRequestChangedFileConnection', fieldSel.selectionSet, ctx, (connField, _a, connSel) => {
          if (connField === 'nodes') {
            return files.map((file) =>
              selectObject('PullRequestChangedFile', connSel.selectionSet, ctx, (fn) => (fn === 'path' ? file.filename : null)),
            );
          }
          return null;
        });
      }
      default:
        return null;
    }
  });
}

function projectRepo(repo: RepoRecord, selectionSet: readonly GqlSelection[] | undefined, ctx: ExecCtx): Record<string, unknown> {
  return selectObject('Repository', selectionSet, ctx, (name, args, fieldSel) => {
    switch (name) {
      case 'id':
        return repo.globalId;
      case 'databaseId':
        return repo.id;
      case 'name':
        return repo.name;
      case 'nameWithOwner':
        return repo.fullName;
      case 'description':
        return repo.description;
      case 'isPrivate':
        return repo.private;
      case 'viewerPermission':
        return 'ADMIN';
      case 'hasIssuesEnabled':
        return true;
      case 'owner':
        return selectObject('User', fieldSel.selectionSet, ctx, (uf) => {
          if (uf === 'login') return repo.owner;
          if (uf === 'name') return ctx.store.viewer.name;
          if (uf === 'id') return ctx.store.viewer.id;
          return null;
        });
      case 'defaultBranchRef':
        return selectObject('Ref', fieldSel.selectionSet, ctx, (rf) => (rf === 'name' ? repo.defaultBranch : null));
      case 'issue': {
        const num = Number(args.number);
        const found = repo.issuesByNumber.get(num);
        return found ? projectIssue(found, fieldSel.selectionSet, ctx) : null;
      }
      case 'issues': {
        let items = [...repo.issues];
        if (Array.isArray(args.states) && args.states.length > 0) {
          const wanted = new Set(args.states.map((s) => String(s).toUpperCase()));
          items = items.filter((it) => wanted.has(it.state.toUpperCase()));
        }
        const orderBy = args.orderBy as { direction?: string } | undefined;
        if (orderBy && typeof orderBy.direction === 'string') {
          const desc = orderBy.direction.toUpperCase() === 'DESC';
          items.sort((a, b) => (desc ? b.seq - a.seq || b.number - a.number : a.seq - b.seq || a.number - b.number));
        } else {
          items.sort((a, b) => a.number - b.number);
        }
        let startIdx = 0;
        if (typeof args.after === 'string' && args.after !== '') {
          const curIdx = decodeCursor(args.after);
          if (curIdx >= 0) startIdx = curIdx + 1;
        }
        const first = typeof args.first === 'number' && args.first >= 0 ? args.first : items.length;
        const pageSlice = items.slice(startIdx, startIdx + first);
        const hasNextPage = startIdx + pageSlice.length < items.length;
        const endCursor = pageSlice.length > 0 ? encodeCursor(startIdx + pageSlice.length - 1) : null;

        return selectObject('IssueConnection', fieldSel.selectionSet, ctx, (cf, _ca, cSel) => {
          if (cf === 'nodes') {
            return pageSlice.map((it) => projectIssue(it, cSel.selectionSet, ctx));
          }
          if (cf === 'pageInfo') {
            return selectObject('PageInfo', cSel.selectionSet, ctx, (pf) => {
              if (pf === 'hasNextPage') return hasNextPage;
              if (pf === 'endCursor') return endCursor;
              return null;
            });
          }
          return null;
        });
      }
      case 'pullRequest': {
        const num = Number(args.number);
        const found = repo.pullsByNumber.get(num);
        return found ? projectPull(repo, found, fieldSel.selectionSet, ctx) : null;
      }
      case 'pullRequests': {
        const first = typeof args.first === 'number' && args.first >= 0 ? args.first : repo.pulls.length;
        const slice = repo.pulls.slice(0, first);
        return selectObject('PullRequestConnection', fieldSel.selectionSet, ctx, (cf, _ca, cSel) => {
          if (cf === 'nodes') {
            return slice.map((pr) => projectPull(repo, pr, cSel.selectionSet, ctx));
          }
          return null;
        });
      }
      default:
        return null;
    }
  });
}

function executeSingleDocument(
  store: ForgeStore,
  doc: { query?: unknown; variables?: unknown; operationName?: unknown },
): { data: unknown; errors?: Array<{ message: string }> } {
  if (typeof doc.query !== 'string' || doc.query.trim() === '') {
    return { data: null, errors: [{ message: 'Missing query string' }] };
  }
  const vars =
    doc.variables && typeof doc.variables === 'object' && !Array.isArray(doc.variables)
      ? (doc.variables as Record<string, unknown>)
      : {};

  let parsed: ReturnType<Parser['parseDocument']>;
  try {
    parsed = new Parser(tokenize(doc.query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Syntax error' }] };
  }

  let op = parsed.operations[0];
  if (typeof doc.operationName === 'string' && doc.operationName !== '') {
    const matching = parsed.operations.find((o) => o.name === doc.operationName);
    if (matching) op = matching;
  }
  if (!op) {
    return { data: null, errors: [{ message: 'Document contains no operations' }] };
  }

  const ctx: ExecCtx = {
    store,
    vars,
    fragments: parsed.fragments,
    errors: [],
  };

  let data: unknown;
  try {
    if (op.opType === 'mutation') {
      data = selectObject('Mutation', op.selectionSet, ctx, (name, args, fieldSel) => {
        const input = (args.input ?? {}) as Record<string, unknown>;
        try {
          if (name === 'createIssue') {
            const repo = store.getRepoByGlobalId(String(input.repositoryId ?? ''));
            if (!repo) {
              ctx.errors.push({ message: 'Could not resolve to a node with the global id' });
              return null;
            }
            const issue = store.createIssue(repo.owner, repo.name, {
              title: input.title,
              body: input.body,
            });
            return selectObject('CreateIssuePayload', fieldSel.selectionSet, ctx, (pf, _a, pSel) =>
              pf === 'issue' ? projectIssue(issue, pSel.selectionSet, ctx) : null,
            );
          }

          if (name === 'closeIssue') {
            const found = store.getIssueByGlobalId(String(input.issueId ?? ''));
            if (!found) {
              ctx.errors.push({ message: 'Could not resolve issue id' });
              return null;
            }
            const updated = store.updateIssue(found.repo.owner, found.repo.name, found.issue.number, { state: 'closed' });
            return selectObject('CloseIssuePayload', fieldSel.selectionSet, ctx, (pf, _a, pSel) =>
              pf === 'issue' ? projectIssue(updated, pSel.selectionSet, ctx) : null,
            );
          }

          if (name === 'createPullRequest') {
            const repo = store.getRepoByGlobalId(String(input.repositoryId ?? ''));
            if (!repo) {
              ctx.errors.push({ message: 'Could not resolve repository id' });
              return null;
            }
            const pr = store.createPull(repo.owner, repo.name, {
              title: input.title,
              body: input.body,
              head: input.headRefName,
              base: input.baseRefName,
            });
            return selectObject('CreatePullRequestPayload', fieldSel.selectionSet, ctx, (pf, _a, pSel) =>
              pf === 'pullRequest' ? projectPull(repo, pr, pSel.selectionSet, ctx) : null,
            );
          }

          if (name === 'mergePullRequest') {
            const found = store.getPullByGlobalId(String(input.pullRequestId ?? ''));
            if (!found) {
              ctx.errors.push({ message: 'Could not resolve pull request id' });
              return null;
            }
            const method = typeof input.mergeMethod === 'string' ? input.mergeMethod : 'SQUASH';
            store.mergePull(found.repo.owner, found.repo.name, found.pull.number, method);
            return selectObject('MergePullRequestPayload', fieldSel.selectionSet, ctx, (pf, _a, pSel) =>
              pf === 'pullRequest' ? projectPull(found.repo, found.pull, pSel.selectionSet, ctx) : null,
            );
          }

          if (name === 'addComment') {
            const subjectId = String(input.subjectId ?? '');
            const issueTarget = store.getIssueByGlobalId(subjectId);
            const pullTarget = issueTarget ? null : store.getPullByGlobalId(subjectId);
            const target = issueTarget ?? pullTarget;
            if (!target) {
              ctx.errors.push({ message: 'Could not resolve subject id' });
              return null;
            }
            const num = 'issue' in target ? target.issue.number : target.pull.number;
            const comment = store.addComment(target.repo.owner, target.repo.name, num, input.body);
            return selectObject('AddCommentPayload', fieldSel.selectionSet, ctx, (pf, _a, pSel) => {
              if (pf !== 'commentEdge') return null;
              return selectObject('CommentEdge', pSel.selectionSet, ctx, (ef, _a2, eSel) => {
                if (ef !== 'node') return null;
                return selectObject('Comment', eSel.selectionSet, ctx, (cf) => {
                  if (cf === 'id') return comment.globalId;
                  if (cf === 'body') return comment.body;
                  return null;
                });
              });
            });
          }
        } catch (err) {
          ctx.errors.push({ message: err instanceof Error ? err.message : 'Mutation failed' });
          return null;
        }
        return null;
      });
    } else {
      data = selectObject('Query', op.selectionSet, ctx, (name, args, fieldSel) => {
        if (name === 'viewer') {
          return selectObject('User', fieldSel.selectionSet, ctx, (uf) => {
            if (uf === 'login') return store.viewer.login;
            if (uf === 'name') return store.viewer.name;
            if (uf === 'id') return store.viewer.id;
            return null;
          });
        }
        if (name === 'repository') {
          const owner = typeof args.owner === 'string' ? args.owner : '';
          const repoName = typeof args.name === 'string' ? args.name : '';
          const repo = store.getRepoIfExists(owner, repoName);
          return repo ? projectRepo(repo, fieldSel.selectionSet, ctx) : null;
        }
        if (name === 'node') {
          const globalId = typeof args.id === 'string' ? args.id : '';
          const repo = store.getRepoByGlobalId(globalId);
          if (repo) return projectRepo(repo, fieldSel.selectionSet, ctx);
          const issueEntry = store.getIssueByGlobalId(globalId);
          if (issueEntry) return projectIssue(issueEntry.issue, fieldSel.selectionSet, ctx);
          const pullEntry = store.getPullByGlobalId(globalId);
          if (pullEntry) return projectPull(pullEntry.repo, pullEntry.pull, fieldSel.selectionSet, ctx);
          return null;
        }
        if (name === '__type') {
          const typeName = typeof args.name === 'string' ? args.name : '';
          const typeFields = TYPE_FIELDS_MAP[typeName] ?? null;
          const enumVals = ENUM_VALUES_MAP[typeName] ?? null;
          if (!typeFields && !enumVals) return null;
          return selectObject('__Type', fieldSel.selectionSet, ctx, (tf, _a, tSel) => {
            if (tf === 'name') return typeName;
            if (tf === 'fields') {
              return typeFields
                ? typeFields.map((fn) => selectObject('__Field', tSel.selectionSet, ctx, (f) => (f === 'name' ? fn : null)))
                : null;
            }
            if (tf === 'enumValues') {
              return enumVals
                ? enumVals.map((ev) => selectObject('__EnumValue', tSel.selectionSet, ctx, (f) => (f === 'name' ? ev : null)))
                : null;
            }
            return null;
          });
        }
        return null;
      });
    }
  } catch (err) {
    ctx.errors.push({ message: err instanceof Error ? err.message : 'Execution error' });
    data = null;
  }

  return ctx.errors.length > 0 ? { data, errors: ctx.errors } : { data };
}

export function handleGraphQL(
  req: IncomingMessage,
  res: ServerResponse,
  store: ForgeStore,
  urlPath: string,
  body: Buffer,
  reqId: string,
  expectedToken: string | undefined,
): boolean {
  if (urlPath !== '/api/graphql') return false;

  if (!isAuthorizedRequest(req, expectedToken)) {
    const payload = Buffer.from(JSON.stringify({ message: 'Requires authentication' }), 'utf8');
    res.writeHead(401, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': payload.length,
      'x-github-request-id': reqId,
    });
    res.end(payload);
    return true;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    const payload = Buffer.from(JSON.stringify({ message: 'Problems parsing JSON' }), 'utf8');
    res.writeHead(400, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': payload.length,
      'x-github-request-id': reqId,
    });
    res.end(payload);
    return true;
  }

  const out = Array.isArray(parsed)
    ? parsed.map((entry) =>
        executeSingleDocument(store, (entry && typeof entry === 'object' ? entry : {}) as Record<string, unknown>),
      )
    : executeSingleDocument(store, (parsed && typeof parsed === 'object' ? parsed : {}) as Record<string, unknown>);

  const buf = Buffer.from(JSON.stringify(out), 'utf8');
  res.writeHead(200, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': buf.length,
    'x-github-request-id': reqId,
  });
  res.end(buf);
  return true;
}
