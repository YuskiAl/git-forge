// GraphQL v4. A small parser for the documents the clients actually send
// (gh CLI and github-mcp-server shapes, plus introspection), and an executor
// that answers them from the store. Unknown fields are reported as errors
// (HTTP 200 with an `errors` array, as GraphQL requires); unknown
// repositories are `data.repository: null` with no errors.
//
// Global ids are opaque base64 strings (`Repository:<dbId>`,
// `Issue:<repoDbId>:<number>`, `PullRequest:<repoDbId>:<number>`,
// `IssueComment:<repoDbId>:<issueNumber>:<commentId>`), stable across reads.

import { errorJson, json } from './http.ts';
import type { HttpResult } from './http.ts';
import { buildTreeFromFlat, diffFlat, headTarget, isOid, makeServerCommit, mergeBase, type FlatFile } from './git.ts';
import { filesAt, mergedFiles, prConflicts } from './rest.ts';
import { VIEWER_LOGIN, clearIssueCache, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type Value =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: Value[] }
  | { kind: 'object'; fields: Map<string, Value> };

interface Selection {
  alias: string | null;
  name: string;
  args: Map<string, Value>;
  selections: Selection[] | null;
}

interface Operation {
  type: 'query' | 'mutation';
  name: string | null;
  variableDefinitions: Map<string, Value>;
  selections: Selection[];
}

class Parser {
  private index = 0;
  private readonly source: string;
  constructor(source: string) {
    this.source = source;
  }

  private peek(): string {
    return this.source[this.index] ?? '';
  }

  private skipWhitespace(): void {
    for (;;) {
      const ch = this.source[this.index];
      if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
        this.index += 1;
        continue;
      }
      if (ch === '#') {
        while (this.index < this.source.length && this.source[this.index] !== '\n') this.index += 1;
        continue;
      }
      return;
    }
  }

  private expect(ch: string): void {
    this.skipWhitespace();
    if (this.source[this.index] !== ch) throw new Error(`expected '${ch}' at ${this.index}`);
    this.index += 1;
  }

  private readName(): string {
    this.skipWhitespace();
    const start = this.index;
    while (this.index < this.source.length && /[A-Za-z0-9_]/.test(this.source[this.index] as string)) this.index += 1;
    if (this.index === start) throw new Error(`expected name at ${this.index}`);
    return this.source.slice(start, this.index);
  }

  private readString(): string {
    this.skipWhitespace();
    if (this.source[this.index] === '"') {
      this.index += 1;
      if (this.source[this.index] === '"' && this.source[this.index + 1] === '"' && this.source[this.index + 2] === '"') {
        // Block string: take it verbatim up to the closing triple quotes.
        this.index += 2;
        const end = this.source.indexOf('"""', this.index);
        if (end < 0) throw new Error('unterminated block string');
        const value = this.source.slice(this.index, end);
        this.index = end + 3;
        return value;
      }
      let out = '';
      while (this.index < this.source.length) {
        const ch = this.source[this.index] as string;
        this.index += 1;
        if (ch === '"') return out;
        if (ch === '\\') {
          const next = this.source[this.index] as string;
          this.index += 1;
          out += next === 'n' ? '\n' : next === 't' ? '\t' : next === 'r' ? '\r' : next;
        } else {
          out += ch;
        }
      }
      throw new Error('unterminated string');
    }
    return this.readName();
  }

  private readNumber(): Value {
    this.skipWhitespace();
    const start = this.index;
    if (this.peek() === '-') this.index += 1;
    while (this.index < this.source.length && /[0-9]/.test(this.source[this.index] as string)) this.index += 1;
    let isFloat = false;
    if (this.source[this.index] === '.') {
      isFloat = true;
      this.index += 1;
      while (this.index < this.source.length && /[0-9]/.test(this.source[this.index] as string)) this.index += 1;
    }
    const text = this.source.slice(start, this.index);
    return isFloat ? { kind: 'float', value: Number.parseFloat(text) } : { kind: 'int', value: Number.parseInt(text, 10) };
  }

  readValue(): Value {
    this.skipWhitespace();
    const ch = this.peek();
    if (ch === '$') {
      this.index += 1;
      return { kind: 'variable', name: this.readName() };
    }
    if (ch === '"') return { kind: 'string', value: this.readString() };
    if (ch === '[') {
      this.index += 1;
      const values: Value[] = [];
      for (;;) {
        this.skipWhitespace();
        if (this.peek() === ']') {
          this.index += 1;
          break;
        }
        values.push(this.readValue());
      }
      return { kind: 'list', values };
    }
    if (ch === '{') {
      this.index += 1;
      const fields = new Map<string, Value>();
      for (;;) {
        this.skipWhitespace();
        if (this.peek() === '}') {
          this.index += 1;
          break;
        }
        const name = this.readName();
        this.expect(':');
        fields.set(name, this.readValue());
      }
      return { kind: 'object', fields };
    }
    if (ch === '-' || /[0-9]/.test(ch)) return this.readNumber();
    const name = this.readName();
    if (name === 'true') return { kind: 'boolean', value: true };
    if (name === 'false') return { kind: 'boolean', value: false };
    if (name === 'null') return { kind: 'null' };
    return { kind: 'enum', value: name };
  }

  private readArguments(): Map<string, Value> {
    const args = new Map<string, Value>();
    this.skipWhitespace();
    if (this.peek() !== '(') return args;
    this.index += 1;
    for (;;) {
      this.skipWhitespace();
      if (this.peek() === ')') {
        this.index += 1;
        break;
      }
      const name = this.readName();
      this.expect(':');
      args.set(name, this.readValue());
    }
    return args;
  }

  private readType(): string {
    this.skipWhitespace();
    if (this.peek() === '[') {
      this.index += 1;
      const inner = this.readType();
      this.expect(']');
      let type = `[${inner}]`;
      this.skipWhitespace();
      if (this.peek() === '!') {
        this.index += 1;
        type += '!';
      }
      return type;
    }
    const name = this.readName();
    this.skipWhitespace();
    if (this.peek() === '!') {
      this.index += 1;
      return `${name}!`;
    }
    return name;
  }

  private readVariableDefinitions(): Map<string, Value> {
    const definitions = new Map<string, Value>();
    this.skipWhitespace();
    if (this.peek() !== '(') return definitions;
    this.index += 1;
    for (;;) {
      this.skipWhitespace();
      if (this.peek() === ')') {
        this.index += 1;
        break;
      }
      this.expect('$');
      const name = this.readName();
      this.expect(':');
      this.readType();
      this.skipWhitespace();
      if (this.peek() === '=') {
        this.index += 1;
        definitions.set(name, this.readValue());
      } else {
        definitions.set(name, { kind: 'null' });
      }
    }
    return definitions;
  }

  readSelectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    for (;;) {
      this.skipWhitespace();
      if (this.peek() === '}') {
        this.index += 1;
        break;
      }
      if (this.peek() === '.' && this.source[this.index + 1] === '.' && this.source[this.index + 2] === '.') {
        this.index += 3;
        this.skipWhitespace();
        let typeCondition: string | null = null;
        if (this.source.slice(this.index, this.index + 2) === 'on') {
          this.index += 2;
          typeCondition = this.readName();
        }
        const nested = this.readSelectionSet();
        selections.push({ alias: null, name: `...on ${typeCondition ?? ''}`, args: new Map(), selections: nested });
        continue;
      }
      let name = this.readName();
      let alias: string | null = null;
      this.skipWhitespace();
      if (this.peek() === ':') {
        this.index += 1;
        alias = name;
        name = this.readName();
      }
      const args = this.readArguments();
      this.skipWhitespace();
      let nested: Selection[] | null = null;
      if (this.peek() === '{') nested = this.readSelectionSet();
      selections.push({ alias, name, args, selections: nested });
    }
    return selections;
  }

  readDocument(): Operation[] {
    const operations: Operation[] = [];
    for (;;) {
      this.skipWhitespace();
      if (this.index >= this.source.length) break;
      let type: 'query' | 'mutation' = 'query';
      let name: string | null = null;
      const word = this.peek();
      if (word === '{') {
        operations.push({ type: 'query', name: null, variableDefinitions: new Map(), selections: this.readSelectionSet() });
        continue;
      }
      const keyword = this.readName();
      if (keyword === 'query' || keyword === 'mutation') {
        type = keyword;
        this.skipWhitespace();
        if (/[A-Za-z_]/.test(this.peek())) name = this.readName();
      } else {
        throw new Error(`unsupported document keyword ${keyword}`);
      }
      const variableDefinitions = this.readVariableDefinitions();
      const selections = this.readSelectionSet();
      operations.push({ type, name, variableDefinitions, selections });
    }
    return operations;
  }
}

// ---------------------------------------------------------------------------
// values
// ---------------------------------------------------------------------------

function resolveValue(value: Value, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'variable': {
      const resolved = variables[value.name];
      return resolved === undefined ? null : resolved;
    }
    case 'string': return value.value;
    case 'int': return value.value;
    case 'float': return value.value;
    case 'boolean': return value.value;
    case 'null': return null;
    case 'enum': return value.value;
    case 'list': return value.values.map((item) => resolveValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, item] of value.fields) out[key] = resolveValue(item, variables);
      return out;
    }
  }
}

function arg(selection: Selection, name: string, variables: Record<string, unknown>): unknown {
  const value = selection.args.get(name);
  if (value === undefined) return undefined;
  return resolveValue(value, variables);
}

// ---------------------------------------------------------------------------
// ids and field sets
// ---------------------------------------------------------------------------

export function encodeGlobalId(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from([kind, ...parts.map(String)].join(':'), 'utf8').toString('base64');
}

export function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const segments = text.split(':');
    if (segments.length < 2) return null;
    return { kind: segments[0] as string, parts: segments.slice(1) };
  } catch {
    return null;
  }
}

const FIELD_SETS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['login', 'id'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Ref: ['name', 'target'],
  Issue: ['id', 'number', 'title', 'state', 'body'],
  IssueConnection: ['nodes', 'edges', 'totalCount', 'pageInfo'],
  IssueEdge: ['node', 'cursor'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
  PullRequestConnection: ['nodes', 'edges', 'totalCount', 'pageInfo'],
  PullRequestEdge: ['node', 'cursor'],
  PullRequestChangedFile: ['path'],
  PullRequestChangedFileConnection: ['nodes', 'pageInfo'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'clientMutationId'],
  IssueCommentEdge: ['node', 'cursor'],
  IssueComment: ['id', 'body'],
  __Type: ['kind', 'name', 'description', 'fields', 'enumValues', 'inputFields', 'interfaces', 'possibleTypes'],
  __Field: ['name', 'description', 'args', 'type'],
  __EnumValue: ['name', 'description'],
  __InputValue: ['name', 'type'],
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  MergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberFromCursor(cursor: string | undefined): number | null {
  if (cursor === undefined || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    if (!text.startsWith('cursor:')) return null;
    const value = Number.parseInt(text.slice('cursor:'.length), 10);
    return Number.isNaN(value) ? null : value;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

interface ExecutorContext {
  store: Store;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function fieldError(ctx: ExecutorContext, typeName: string, field: string): null {
  ctx.errors.push({ message: `Cannot query field "${field}" on type "${typeName}".` });
  return null;
}

function resolveSelections(
  ctx: ExecutorContext,
  typeName: string,
  source: unknown,
  selections: Selection[] | null,
  resolver: (ctx: ExecutorContext, typeName: string, source: unknown, selection: Selection) => unknown,
): Record<string, unknown> | null {
  if (source === null || source === undefined) return null;
  if (selections === null) return {};
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    const key = selection.alias ?? selection.name;
    if (selection.name.startsWith('...on ')) {
      const wanted = selection.name.slice('...on '.length).trim();
      if (wanted === '' || wanted === typeName) {
        const nested = resolveSelections(ctx, typeName, source, selection.selections, resolver);
        if (nested !== null) Object.assign(out, nested);
      }
      continue;
    }
    const allowed = FIELD_SETS[typeName] ?? [];
    if (!allowed.includes(selection.name)) {
      out[key] = fieldError(ctx, typeName, selection.name);
      continue;
    }
    out[key] = resolver(ctx, typeName, source, selection);
  }
  return out;
}

function issueState(issue: Issue): 'OPEN' | 'CLOSED' {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function prState(pull: PullRequest): 'OPEN' | 'CLOSED' | 'MERGED' {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function resolveIssue(ctx: ExecutorContext, repoId: number, issue: Issue, selection: Selection): unknown {
  switch (selection.name) {
    case 'id': return encodeGlobalId('Issue', repoId, issue.number);
    case 'number': return issue.number;
    case 'title': return issue.title;
    case 'state': return issueState(issue);
    case 'body': return issue.body;
    default: return fieldError(ctx, 'Issue', selection.name);
  }
}

function resolvePull(ctx: ExecutorContext, repo: Repo, pull: PullRequest, selection: Selection): unknown {
  switch (selection.name) {
    case 'id': return encodeGlobalId('PullRequest', repo.id, pull.number);
    case 'number': return pull.number;
    case 'title': return pull.title;
    case 'state': return prState(pull);
    case 'merged': return pull.merged;
    case 'headRefName': return pull.head;
    case 'baseRefName': return pull.base;
    case 'mergeable': return prConflicts(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
    case 'files': {
      const base = mergeBase(repo, pull.headSha, pull.baseSha);
      const baseFiles = base === null ? new Map<string, FlatFile>() : filesAt(repo, base);
      const headFiles = filesAt(repo, pull.headSha);
      const paths = diffFlat(baseFiles, headFiles).map((change) => ({ path: change.path }));
      const first = arg(selection, 'first', ctx.variables);
      const nodes = typeof first === 'number' ? paths.slice(0, first) : paths;
      return resolveSelections(ctx, 'PullRequestChangedFileConnection', { nodes, hasNext: false }, selection.selections, (inner, typeName, source, sel) => {
        const connection = source as { nodes: Array<{ path: string }>; hasNext: boolean };
        if (sel.name === 'nodes') {
          return connection.nodes.map((node) => resolveSelections(inner, 'PullRequestChangedFile', node, sel.selections, (c2, _t, nodeSource, sel2) => {
            if (sel2.name === 'path') return (nodeSource as { path: string }).path;
            return fieldError(c2, 'PullRequestChangedFile', sel2.name);
          }));
        }
        if (sel.name === 'pageInfo') {
          return resolveSelections(inner, 'PageInfo', {}, sel.selections, (c2, _t, _s, sel2) => {
            if (sel2.name === 'hasNextPage') return connection.hasNext;
            if (sel2.name === 'endCursor') return null;
            if (sel2.name === 'hasPreviousPage') return false;
            if (sel2.name === 'startCursor') return null;
            return fieldError(c2, 'PageInfo', sel2.name);
          });
        }
        return fieldError(inner, 'PullRequestChangedFileConnection', sel.name);
      });
    }
    default: return fieldError(ctx, 'PullRequest', selection.name);
  }
}

function resolveRepository(ctx: ExecutorContext, repo: Repo, selection: Selection): unknown {
  switch (selection.name) {
    case 'id': return encodeGlobalId('Repository', repo.id);
    case 'databaseId': return repo.id;
    case 'name': return repo.name;
    case 'nameWithOwner': return `${repo.owner}/${repo.name}`;
    case 'owner': {
      return resolveSelections(ctx, 'User', { login: repo.owner }, selection.selections, (inner, _t, _s, sel) => {
        if (sel.name === 'login') return repo.owner;
        if (sel.name === 'id') return encodeGlobalId('User', repo.owner);
        return fieldError(inner, 'User', sel.name);
      });
    }
    case 'defaultBranchRef': {
      const tip = headTarget(repo);
      if (tip === undefined) return null;
      return resolveSelections(ctx, 'Ref', { name: repo.defaultBranch }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'name') return (source as { name: string }).name;
        if (sel.name === 'target') return null;
        return fieldError(inner, 'Ref', sel.name);
      });
    }
    case 'viewerPermission': return 'ADMIN';
    case 'hasIssuesEnabled': return true;
    case 'issue': {
      const number = arg(selection, 'number', ctx.variables);
      if (typeof number !== 'number') return null;
      const issue = repo.issues.get(number);
      if (issue === undefined) return null;
      return resolveSelections(ctx, 'Issue', issue, selection.selections, (inner, _t, source, sel) => resolveIssue(inner, repo.id, source as Issue, sel));
    }
    case 'issues': {
      const first = arg(selection, 'first', ctx.variables);
      const states = arg(selection, 'states', ctx.variables);
      const orderBy = arg(selection, 'orderBy', ctx.variables);
      const after = arg(selection, 'after', ctx.variables);
      let list = [...repo.issues.values()];
      if (Array.isArray(states) && states.length > 0) {
        const wanted = new Set(states.map((state) => String(state)));
        list = list.filter((issue) => wanted.has(issueState(issue)));
      }
      const order = orderBy as { field?: string; direction?: string } | undefined;
      const direction = order?.direction === 'ASC' ? 1 : -1;
      list.sort((a, b) => {
        const byNumber = (a.number - b.number) * direction;
        return byNumber;
      });
      const afterNumber = typeof after === 'string' ? numberFromCursor(after) : null;
      if (afterNumber !== null) list = list.filter((issue) => direction === 1 ? issue.number > afterNumber : issue.number < afterNumber);
      const limit = typeof first === 'number' ? first : list.length;
      const nodes = list.slice(0, limit);
      const hasNext = list.length > limit;
      const endCursor = nodes.length > 0 ? cursorFor(nodes[nodes.length - 1]?.number ?? 0) : null;
      return resolveSelections(ctx, 'IssueConnection', { nodes, hasNext, endCursor }, selection.selections, (inner, _t, source, sel) => {
        const connection = source as { nodes: Issue[]; hasNext: boolean; endCursor: string | null };
        if (sel.name === 'nodes') {
          return connection.nodes.map((issue) => resolveSelections(inner, 'Issue', issue, sel.selections, (c2, _t2, src2, sel2) => resolveIssue(c2, repo.id, src2 as Issue, sel2)));
        }
        if (sel.name === 'edges') {
          return connection.nodes.map((issue) => resolveSelections(inner, 'IssueEdge', { node: issue, cursor: cursorFor(issue.number) }, sel.selections, (c2, _t2, src2, sel2) => {
            const edge = src2 as { node: Issue; cursor: string };
            if (sel2.name === 'node') return resolveSelections(c2, 'Issue', edge.node, sel2.selections, (c3, _t3, src3, sel3) => resolveIssue(c3, repo.id, src3 as Issue, sel3));
            if (sel2.name === 'cursor') return edge.cursor;
            return fieldError(c2, 'IssueEdge', sel2.name);
          }));
        }
        if (sel.name === 'totalCount') return repo.issues.size;
        if (sel.name === 'pageInfo') {
          return resolveSelections(inner, 'PageInfo', {}, sel.selections, (c2, _t2, _s2, sel2) => {
            if (sel2.name === 'hasNextPage') return connection.hasNext;
            if (sel2.name === 'endCursor') return connection.endCursor;
            if (sel2.name === 'hasPreviousPage') return false;
            if (sel2.name === 'startCursor') return null;
            return fieldError(c2, 'PageInfo', sel2.name);
          });
        }
        return fieldError(inner, 'IssueConnection', sel.name);
      });
    }
    case 'pullRequests': {
      const first = arg(selection, 'first', ctx.variables);
      const list = [...repo.pulls.values()].sort((a, b) => b.number - a.number);
      const limit = typeof first === 'number' ? first : list.length;
      const nodes = list.slice(0, limit);
      return resolveSelections(ctx, 'PullRequestConnection', { nodes }, selection.selections, (inner, _t, source, sel) => {
        const connection = source as { nodes: PullRequest[] };
        if (sel.name === 'nodes') {
          return connection.nodes.map((pull) => resolveSelections(inner, 'PullRequest', pull, sel.selections, (c2, _t2, src2, sel2) => resolvePull(c2, repo, src2 as PullRequest, sel2)));
        }
        if (sel.name === 'edges') return [];
        if (sel.name === 'totalCount') return repo.pulls.size;
        if (sel.name === 'pageInfo') {
          return resolveSelections(inner, 'PageInfo', {}, sel.selections, (c2, _t2, _s2, sel2) => {
            if (sel2.name === 'hasNextPage') return false;
            if (sel2.name === 'endCursor') return null;
            if (sel2.name === 'hasPreviousPage') return false;
            if (sel2.name === 'startCursor') return null;
            return fieldError(c2, 'PageInfo', sel2.name);
          });
        }
        return fieldError(inner, 'PullRequestConnection', sel.name);
      });
    }
    case 'pullRequest': {
      const number = arg(selection, 'number', ctx.variables);
      if (typeof number !== 'number') return null;
      const pull = repo.pulls.get(number);
      if (pull === undefined) return null;
      return resolveSelections(ctx, 'PullRequest', pull, selection.selections, (inner, _t, source, sel) => resolvePull(inner, repo, source as PullRequest, sel));
    }
    default: return fieldError(ctx, 'Repository', selection.name);
  }
}

function resolveRootField(ctx: ExecutorContext, operation: Operation, selection: Selection): unknown {
  if (operation.type === 'query') {
    switch (selection.name) {
      case 'viewer': {
        return resolveSelections(ctx, 'User', { login: VIEWER_LOGIN }, selection.selections, (inner, _t, _s, sel) => {
          if (sel.name === 'login') return VIEWER_LOGIN;
          if (sel.name === 'id') return encodeGlobalId('User', VIEWER_LOGIN);
          return fieldError(inner, 'User', sel.name);
        });
      }
      case 'repository': {
        const owner = arg(selection, 'owner', ctx.variables);
        const name = arg(selection, 'name', ctx.variables);
        if (typeof owner !== 'string' || typeof name !== 'string') return null;
        const repo = ctx.store.getRepo(owner, name);
        if (repo === undefined) return null;
        return resolveSelections(ctx, 'Repository', repo, selection.selections, (inner, _t, source, sel) => resolveRepository(inner, source as Repo, sel));
      }
      case 'node': {
        const id = arg(selection, 'id', ctx.variables);
        if (typeof id !== 'string') return null;
        const decoded = decodeGlobalId(id);
        if (decoded === null) return null;
        if (decoded.kind === 'Repository') {
          const repoId = Number.parseInt(decoded.parts[0] as string, 10);
          const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
          if (repo === undefined) return null;
          return resolveSelections(ctx, 'Repository', repo, selection.selections, (inner, _t, source, sel) => resolveRepository(inner, source as Repo, sel));
        }
        if (decoded.kind === 'Issue') {
          const repoId = Number.parseInt(decoded.parts[0] as string, 10);
          const number = Number.parseInt(decoded.parts[1] as string, 10);
          const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
          const issue = repo?.issues.get(number);
          if (issue === undefined) return null;
          return resolveSelections(ctx, 'Issue', issue, selection.selections, (inner, _t, source, sel) => resolveIssue(inner, repo.id, source as Issue, sel));
        }
        if (decoded.kind === 'PullRequest') {
          const repoId = Number.parseInt(decoded.parts[0] as string, 10);
          const number = Number.parseInt(decoded.parts[1] as string, 10);
          const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
          const pull = repo?.pulls.get(number);
          if (pull === undefined || repo === undefined) return null;
          return resolveSelections(ctx, 'PullRequest', pull, selection.selections, (inner, _t, source, sel) => resolvePull(inner, repo, source as PullRequest, sel));
        }
        return null;
      }
      case '__type': {
        const name = arg(selection, 'name', ctx.variables);
        if (typeof name !== 'string') return null;
        if (FIELD_SETS[name] === undefined && ENUM_VALUES[name] === undefined) return null;
        const typeSource = { name, isEnum: ENUM_VALUES[name] !== undefined };
        return resolveSelections(ctx, '__Type', typeSource, selection.selections, (inner, _t, source, sel) => {
          const typed = source as { name: string; isEnum: boolean };
          if (sel.name === 'name') return typed.name;
          if (sel.name === 'kind') return typed.isEnum ? 'ENUM' : 'OBJECT';
          if (sel.name === 'description') return null;
          if (sel.name === 'fields') {
            const fields = FIELD_SETS[typed.name] ?? [];
            return fields.map((fieldName) => resolveSelections(inner, '__Field', { name: fieldName }, sel.selections, (c2, _t2, src2, sel2) => {
              if (sel2.name === 'name') return (src2 as { name: string }).name;
              if (sel2.name === 'description') return null;
              if (sel2.name === 'args') return [];
              if (sel2.name === 'type') return null;
              return fieldError(c2, '__Field', sel2.name);
            }));
          }
          if (sel.name === 'enumValues') {
            const values = ENUM_VALUES[typed.name] ?? [];
            return values.map((valueName) => resolveSelections(inner, '__EnumValue', { name: valueName }, sel.selections, (c2, _t2, src2, sel2) => {
              if (sel2.name === 'name') return (src2 as { name: string }).name;
              if (sel2.name === 'description') return null;
              return fieldError(c2, '__EnumValue', sel2.name);
            }));
          }
          if (sel.name === 'inputFields') return [];
          if (sel.name === 'interfaces') return [];
          if (sel.name === 'possibleTypes') return [];
          return fieldError(inner, '__Type', sel.name);
        });
      }
      case '__schema': {
        return resolveSelections(ctx, '__Type', { name: 'Query', isEnum: false }, selection.selections, () => null);
      }
      default: return fieldError(ctx, 'Query', selection.name);
    }
  }

  // Mutations.
  switch (selection.name) {
    case 'createIssue': {
      const input = arg(selection, 'input', ctx.variables) as Record<string, unknown> | undefined;
      if (input === undefined) return null;
      const repositoryId = String(input.repositoryId ?? '');
      const decoded = decodeGlobalId(repositoryId);
      if (decoded === null || decoded.kind !== 'Repository') return null;
      const repoId = Number.parseInt(decoded.parts[0] as string, 10);
      const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
      if (repo === undefined) return null;
      const number = repo.nextNumber;
      repo.nextNumber += 1;
      const seq = ctx.store.tick();
      const issue: Issue = {
        id: number,
        number,
        title: String(input.title ?? ''),
        body: typeof input.body === 'string' ? input.body : '',
        state: 'open',
        labels: [],
        user: VIEWER_LOGIN,
        createdSeq: seq,
        updatedSeq: seq,
        comments: [],
      };
      repo.issues.set(number, issue);
      return resolveSelections(ctx, 'CreateIssuePayload', { issue }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'issue') {
          return resolveSelections(inner, 'Issue', (source as { issue: Issue }).issue, sel.selections, (c2, _t2, src2, sel2) => resolveIssue(c2, repo.id, src2 as Issue, sel2));
        }
        if (sel.name === 'clientMutationId') return null;
        return fieldError(inner, 'CreateIssuePayload', sel.name);
      });
    }
    case 'closeIssue': {
      const input = arg(selection, 'input', ctx.variables) as Record<string, unknown> | undefined;
      if (input === undefined) return null;
      const decoded = decodeGlobalId(String(input.issueId ?? ''));
      if (decoded === null || decoded.kind !== 'Issue') return null;
      const repoId = Number.parseInt(decoded.parts[0] as string, 10);
      const number = Number.parseInt(decoded.parts[1] as string, 10);
      const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
      const issue = repo?.issues.get(number);
      if (issue === undefined) return null;
      issue.state = 'closed';
      issue.updatedSeq = ctx.store.tick();
      clearIssueCache(issue);
      return resolveSelections(ctx, 'CloseIssuePayload', { issue }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'issue') {
          return resolveSelections(inner, 'Issue', (source as { issue: Issue }).issue, sel.selections, (c2, _t2, src2, sel2) => resolveIssue(c2, repo.id, src2 as Issue, sel2));
        }
        if (sel.name === 'clientMutationId') return null;
        return fieldError(inner, 'CloseIssuePayload', sel.name);
      });
    }
    case 'createPullRequest': {
      const input = arg(selection, 'input', ctx.variables) as Record<string, unknown> | undefined;
      if (input === undefined) return null;
      const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
      if (decoded === null || decoded.kind !== 'Repository') return null;
      const repoId = Number.parseInt(decoded.parts[0] as string, 10);
      const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
      if (repo === undefined) return null;
      const base = String(input.baseRefName ?? '');
      const head = String(input.headRefName ?? '');
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) return null;
      const number = repo.nextNumber;
      repo.nextNumber += 1;
      const seq = ctx.store.tick();
      const pull: PullRequest = {
        id: number,
        number,
        title: String(input.title ?? ''),
        body: typeof input.body === 'string' ? input.body : '',
        state: 'open',
        head,
        base,
        headSha,
        baseSha,
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        user: VIEWER_LOGIN,
        createdSeq: seq,
        updatedSeq: seq,
      };
      repo.pulls.set(number, pull);
      return resolveSelections(ctx, 'CreatePullRequestPayload', { pull }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'pullRequest') {
          return resolveSelections(inner, 'PullRequest', (source as { pull: PullRequest }).pull, sel.selections, (c2, _t2, src2, sel2) => resolvePull(c2, repo, src2 as PullRequest, sel2));
        }
        if (sel.name === 'clientMutationId') return null;
        return fieldError(inner, 'CreatePullRequestPayload', sel.name);
      });
    }
    case 'mergePullRequest': {
      const input = arg(selection, 'input', ctx.variables) as Record<string, unknown> | undefined;
      if (input === undefined) return null;
      const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
      if (decoded === null || decoded.kind !== 'PullRequest') return null;
      const repoId = Number.parseInt(decoded.parts[0] as string, 10);
      const number = Number.parseInt(decoded.parts[1] as string, 10);
      const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
      const pull = repo?.pulls.get(number);
      if (pull === undefined || repo === undefined) return null;
      if (pull.state !== 'open' || prConflicts(repo, pull)) {
        // GraphQL merge of a conflicting PR: report the PR unmerged.
        return resolveSelections(ctx, 'MergePullRequestPayload', { pull }, selection.selections, (inner, _t, source, sel) => {
          if (sel.name === 'pullRequest') {
            return resolveSelections(inner, 'PullRequest', (source as { pull: PullRequest }).pull, sel.selections, (c2, _t2, src2, sel2) => resolvePull(c2, repo, src2 as PullRequest, sel2));
          }
          if (sel.name === 'clientMutationId') return null;
          return fieldError(inner, 'MergePullRequestPayload', sel.name);
        });
      }
      const mergeMethod = String(input.mergeMethod ?? 'MERGE');
      const baseTipSha = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
      const base = mergeBase(repo, pull.headSha, baseTipSha);
      const baseFiles = base === null ? new Map<string, FlatFile>() : filesAt(repo, base);
      const headFiles = filesAt(repo, pull.headSha);
      const baseTipFiles = filesAt(repo, baseTipSha);
      const merged = mergedFiles(baseTipFiles, baseFiles, headFiles);
      const tree = buildTreeFromFlat(repo, merged);
      const parents = mergeMethod === 'SQUASH' ? [baseTipSha] : [baseTipSha, pull.headSha];
      const message = mergeMethod === 'SQUASH'
        ? `${pull.title} (#${pull.number})`.trim()
        : `Merge pull request #${pull.number} from ${repo.owner}/${pull.head}\n\n${pull.title}`;
      const commit = makeServerCommit(ctx.store, repo, tree, parents, message);
      repo.refs.set(`refs/heads/${pull.base}`, commit);
      pull.state = 'closed';
      pull.merged = true;
      pull.mergedAt = ctx.store.isoOf(1_735_689_600 + ctx.store.seq);
      pull.mergeCommitSha = commit;
      pull.updatedSeq = ctx.store.tick();
      return resolveSelections(ctx, 'MergePullRequestPayload', { pull }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'pullRequest') {
          return resolveSelections(inner, 'PullRequest', (source as { pull: PullRequest }).pull, sel.selections, (c2, _t2, src2, sel2) => resolvePull(c2, repo, src2 as PullRequest, sel2));
        }
        if (sel.name === 'clientMutationId') return null;
        return fieldError(inner, 'MergePullRequestPayload', sel.name);
      });
    }
    case 'addComment': {
      const input = arg(selection, 'input', ctx.variables) as Record<string, unknown> | undefined;
      if (input === undefined) return null;
      const decoded = decodeGlobalId(String(input.subjectId ?? ''));
      if (decoded === null || decoded.kind !== 'Issue') return null;
      const repoId = Number.parseInt(decoded.parts[0] as string, 10);
      const number = Number.parseInt(decoded.parts[1] as string, 10);
      const repo = [...ctx.store.repos.values()].find((candidate) => candidate.id === repoId);
      const issue = repo?.issues.get(number);
      if (issue === undefined || repo === undefined) return null;
      const commentId = repo.nextCommentId;
      repo.nextCommentId += 1;
      const comment = { id: commentId, body: String(input.body ?? ''), user: VIEWER_LOGIN, createdSeq: ctx.store.tick() };
      issue.comments.push(comment);
      clearIssueCache(issue);
      const commentNode = { id: encodeGlobalId('IssueComment', repo.id, number, commentId), body: comment.body };
      return resolveSelections(ctx, 'AddCommentPayload', { comment: commentNode }, selection.selections, (inner, _t, source, sel) => {
        if (sel.name === 'commentEdge') {
          return resolveSelections(inner, 'IssueCommentEdge', { node: (source as { comment: { id: string; body: string } }).comment }, sel.selections, (c2, _t2, src2, sel2) => {
            const edge = src2 as { node: { id: string; body: string } };
            if (sel2.name === 'node') {
              return resolveSelections(c2, 'IssueComment', edge.node, sel2.selections, (c3, _t3, src3, sel3) => {
                if (sel3.name === 'id') return (src3 as { id: string }).id;
                if (sel3.name === 'body') return (src3 as { body: string }).body;
                return fieldError(c3, 'IssueComment', sel3.name);
              });
            }
            if (sel2.name === 'cursor') return null;
            return fieldError(c2, 'IssueCommentEdge', sel2.name);
          });
        }
        if (sel.name === 'clientMutationId') return null;
        return fieldError(inner, 'AddCommentPayload', sel.name);
      });
    }
    default: return fieldError(ctx, 'Mutation', selection.name);
  }
}

function executeOperation(store: Store, operation: Operation, variables: Record<string, unknown>): { data: Record<string, unknown>; errors?: Array<{ message: string }> } {
  const ctx: ExecutorContext = { store, variables, errors: [] };
  const data: Record<string, unknown> = {};
  const rootType = operation.type === 'query' ? 'Query' : 'Mutation';
  const allowed = FIELD_SETS[rootType] ?? [];
  for (const selection of operation.selections) {
    const key = selection.alias ?? selection.name;
    if (selection.name.startsWith('...')) continue;
    if (!allowed.includes(selection.name)) {
      data[key] = fieldError(ctx, rootType, selection.name);
      continue;
    }
    data[key] = resolveRootField(ctx, operation, selection);
  }
  const result: { data: Record<string, unknown>; errors?: Array<{ message: string }> } = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}

export function handleGraphQL(store: Store, requestId: string, body: unknown): HttpResult {
  if (body === null || typeof body !== 'object') {
    return errorJson(400, 'request body must be a JSON object or array', requestId);
  }
  const documents = Array.isArray(body) ? body : [body];
  const results: Array<{ data: Record<string, unknown>; errors?: Array<{ message: string }> }> = [];
  for (const document of documents) {
    if (document === null || typeof document !== 'object') {
      results.push({ data: {}, errors: [{ message: 'request body must be a JSON object' }] });
      continue;
    }
    const query = (document as Record<string, unknown>).query;
    const variables = ((document as Record<string, unknown>).variables ?? {}) as Record<string, unknown>;
    if (typeof query !== 'string') {
      results.push({ data: {}, errors: [{ message: 'query must be a string' }] });
      continue;
    }
    try {
      const operations = new Parser(query).readDocument();
      if (operations.length === 0) {
        results.push({ data: {}, errors: [{ message: 'no operation found' }] });
        continue;
      }
      const operation = operations[0] as Operation;
      results.push(executeOperation(store, operation, variables));
    } catch (error) {
      results.push({ data: {}, errors: [{ message: error instanceof Error ? error.message : 'parse error' }] });
    }
  }
  return json(200, Array.isArray(body) ? results : results[0], { 'X-GitHub-Request-Id': requestId });
}
