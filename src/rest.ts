// REST v3 (api.github.com-compatible surface). Every reply is JSON with a
// non-empty X-GitHub-Request-Id; unknown routes are JSON 404s; malformed
// JSON bodies are 400s; error bodies carry a string `message`.
//
// Ownership is decided by the bearer token's login (adaiversen in the
// trial): the same repository name under another owner is 404, never 403.

import {
  buildTreeFromFlat,
  collectReachable,
  defaultBranchRef,
  diffFlat,
  flattenTree,
  hashObject,
  headTarget,
  isOid,
  makeServerCommit,
  mergeBase,
  parseCommit,
  putObject,
  ZERO_OID,
  type FlatFile,
} from './git.ts';
import { errorJson, json, linkHeader, paginate, pageParams } from './http.ts';
import type { HttpResult } from './http.ts';
import { VIEWER_EMAIL, VIEWER_LOGIN, VIEWER_NAME, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

export interface RestContext {
  store: Store;
  requestId: string;
  host: string;
}

function userJson() {
  return { login: VIEWER_LOGIN, id: 1000, name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User' };
}

function repoJson(ctx: RestContext, repo: Repo): Record<string, unknown> {
  const created = ctx.store.isoOf(1_735_689_600 + repo.createdSeq);
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userJson(),
    private: repo.private,
    description: repo.description,
    fork: false,
    created_at: created,
    updated_at: created,
    pushed_at: created,
    homepage: '',
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: true,
    has_projects: true,
    has_downloads: true,
    has_wiki: true,
    has_pages: false,
    has_discussions: false,
    forks_count: 0,
    open_issues_count: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
    license: null,
    allow_forking: true,
    default_branch: repo.defaultBranch,
    url: `http://${ctx.host}/api/v3/repos/${repo.owner}/${repo.name}`,
    html_url: `http://${ctx.host}/${repo.owner}/${repo.name}`,
  };
}

function labelJson(label: { name: string; color: string; description: string }): Record<string, unknown> {
  return { name: label.name, color: label.color, description: label.description ?? '', default: false };
}

function issueJson(ctx: RestContext, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${repo.id}_${issue.number}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(repo.labels.get(name) ?? { name, color: 'ededed', description: '' })),
    user: userJson(),
    assignee: null,
    assignees: [],
    milestone: null,
    locked: false,
    comments: issue.comments.length,
    created_at: ctx.store.isoOf(issue.createdSeq),
    updated_at: ctx.store.isoOf(issue.updatedSeq),
    closed_at: issue.state === 'closed' ? ctx.store.isoOf(issue.updatedSeq) : null,
    author_association: 'OWNER',
    url: `http://${ctx.host}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    html_url: `http://${ctx.host}/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function pullJson(ctx: RestContext, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const created = ctx.store.isoOf(pull.createdSeq);
  return {
    id: pull.id,
    node_id: `PR_${repo.id}_${pull.number}`,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.head, sha: pull.headSha, label: `${repo.owner}:${pull.head}` },
    base: { ref: pull.base, sha: pull.baseSha, label: `${repo.owner}:${pull.base}` },
    user: userJson(),
    labels: [],
    assignee: null,
    assignees: [],
    milestone: null,
    locked: false,
    draft: false,
    comments: 0,
    review_comments: 0,
    commits: 1,
    additions: 0,
    deletions: 0,
    changed_files: 0,
    created_at: created,
    updated_at: ctx.store.isoOf(pull.updatedSeq),
    closed_at: pull.state === 'closed' ? ctx.store.isoOf(pull.updatedSeq) : null,
    url: `http://${ctx.host}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
    html_url: `http://${ctx.host}/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function commitJson(ctx: RestContext, repo: Repo, oid: string): Record<string, unknown> {
  const object = repo.objects.get(oid);
  const commit = object !== undefined && object.type === 'commit' ? parseCommit(object.data) : null;
  let timestamp = ctx.store.isoOf(1_735_689_600 + 1);
  if (commit !== null) {
    const match = commit.author.match(/ (\d+) \+0000$/);
    if (match !== null) timestamp = ctx.store.isoOf(Number.parseInt(match[1] as string, 10));
  }
  const author = { name: VIEWER_NAME, email: VIEWER_EMAIL, date: timestamp };
  let message = '';
  let parents: Array<{ sha: string }> = [];
  if (commit !== null) {
    message = commit.message;
    parents = commit.parents.map((sha) => ({ sha }));
  }
  return {
    sha: oid,
    node_id: `C_${oid}`,
    commit: { message, author, committer: author, tree: { sha: commit?.tree ?? ZERO_OID }, comment_count: 0 },
    author: userJson(),
    committer: userJson(),
    parents,
    url: `http://${ctx.host}/api/v3/repos/${repo.owner}/${repo.name}/commits/${oid}`,
    html_url: `http://${ctx.host}/${repo.owner}/${repo.name}/commit/${oid}`,
  };
}

/** Resolve a ref name (branch, full ref, or oid) to a commit oid, or null. */
export function resolveRef(repo: Repo, ref: string): string | null {
  if (isOid(ref)) return repo.objects.has(ref) ? ref : null;
  if (ref.startsWith('refs/')) {
    const oid = repo.refs.get(ref);
    return oid ?? null;
  }
  const branch = repo.refs.get(`refs/heads/${ref}`);
  if (branch !== undefined) return branch;
  const tag = repo.refs.get(`refs/tags/${ref}`);
  if (tag !== undefined) return tag;
  return null;
}

function commitListFrom(repo: Repo, start: string, limit: number): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue = [start];
  while (queue.length > 0 && out.length < limit) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    out.push(oid);
    const commit = parseCommit(object.data);
    for (const parent of commit.parents) queue.push(parent);
  }
  return out;
}

/** The files of a commit as a flat path map. */
export function filesAt(repo: Repo, commitOid: string): Map<string, FlatFile> {
  const object = repo.objects.get(commitOid);
  if (object === undefined || object.type !== 'commit') return new Map();
  return flattenTree(repo, parseCommit(object.data).tree);
}

/** Three-way tree merge: base tip's files plus the head side's changes from the merge base. */
export function mergedFiles(baseTip: Map<string, FlatFile>, baseFiles: Map<string, FlatFile>, headFiles: Map<string, FlatFile>): Map<string, FlatFile> {
  const out = new Map(baseTip);
  for (const change of diffFlat(baseFiles, headFiles)) {
    if (change.status === 'removed') out.delete(change.path);
    else if (change.newOid !== null) out.set(change.path, { mode: headFiles.get(change.path)?.mode ?? '100644', oid: change.newOid });
  }
  return out;
}

/** Does merging this PR conflict? Both sides changed the same file from the merge base with different content. The base side is the base branch's CURRENT tip, not the tip recorded when the pull request was opened: a merge that landed in between must count. */
export function prConflicts(repo: Repo, pull: PullRequest): boolean {
  const headTip = pull.headSha;
  const baseTip = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  if (headTip === ZERO_OID || baseTip === ZERO_OID) return false;
  const base = mergeBase(repo, headTip, baseTip);
  if (base === null) return false;
  const baseFiles = filesAt(repo, base);
  const headFiles = filesAt(repo, headTip);
  const baseTipFiles = filesAt(repo, baseTip);
  const headChanged = new Map<string, string | null>();
  for (const change of diffFlat(baseFiles, headFiles)) headChanged.set(change.path, change.newOid);
  for (const change of diffFlat(baseFiles, baseTipFiles)) {
    const headValue = headChanged.get(change.path);
    if (headValue !== undefined && headValue !== change.newOid) return true;
  }
  return false;
}

/** Count added/removed lines between two text blobs (multiset line difference). */
function lineCounts(oldText: string | null, newText: string | null): { additions: number; deletions: number } {
  const countLines = (text: string | null): Map<string, number> => {
    const map = new Map<string, number>();
    if (text === null) return map;
    for (const line of text.split('\n')) map.set(line, (map.get(line) ?? 0) + 1);
    return map;
  };
  const oldLines = countLines(oldText);
  const newLines = countLines(newText);
  let additions = 0;
  let deletions = 0;
  for (const [line, count] of newLines) {
    const oldCount = oldLines.get(line) ?? 0;
    if (count > oldCount) additions += count - oldCount;
  }
  for (const [line, count] of oldLines) {
    const newCount = newLines.get(line) ?? 0;
    if (count > newCount) deletions += count - newCount;
  }
  return { additions, deletions };
}

function blobText(repo: Repo, oid: string | null): string | null {
  if (oid === null) return null;
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'blob') return null;
  return object.data.toString('utf8');
}

export async function handleRest(
  ctx: RestContext,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: Record<string, unknown> | null,
): Promise<HttpResult> {
  const { store } = ctx;
  const rid = ctx.requestId;
  const notFound = (): HttpResult => errorJson(404, 'Not Found', rid);

  // GET /api/v3/ — the root; the rate-limit check reads X-OAuth-Scopes.
  if (method === 'GET' && segments.length === 0) {
    return json(200, {
      current_user_url: `http://${ctx.host}/api/v3/user`,
      repository_url: `http://${ctx.host}/api/v3/repos/{owner}/{repo}`,
      user_url: `http://${ctx.host}/api/v3/users/{user}`,
    }, { 'X-OAuth-Scopes': 'repo, user', 'X-GitHub-Request-Id': rid });
  }

  // GET /api/v3/meta
  if (method === 'GET' && segments.length === 1 && segments[0] === 'meta') {
    return json(200, { installed_version: '1.0.0', verifiable_password_authentication: true }, { 'X-GitHub-Request-Id': rid });
  }

  // GET /api/v3/user
  if (method === 'GET' && segments.length === 1 && segments[0] === 'user') {
    return json(200, userJson(), { 'X-GitHub-Request-Id': rid });
  }

  // POST /user/repos  +  GET /user/repos
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'POST') {
      const name = typeof body?.name === 'string' ? body.name : '';
      if (name === '') return errorJson(422, 'name is required', rid);
      if (store.getRepo(VIEWER_LOGIN, name) !== undefined) {
        return errorJson(422, `Repository ${VIEWER_LOGIN}/${name} already exists`, rid);
      }
      const repo = store.createRepo(VIEWER_LOGIN, name, typeof body?.description === 'string' ? body.description : '', body?.private === true);
      return json(201, repoJson(ctx, repo), { 'X-GitHub-Request-Id': rid });
    }
    if (method === 'GET') {
      const { page, perPage } = pageParams(query);
      const repos = store.listViewerRepos();
      const { slice, hasNext, hasPrev } = paginate(repos, page, perPage);
      const link = linkHeader(ctx.host, '/api/v3/user/repos', {}, page, perPage, hasNext, hasPrev);
      return json(200, slice.map((repo) => repoJson(ctx, repo)), {
        'X-GitHub-Request-Id': rid,
        ...(link !== undefined ? { Link: link } : {}),
      });
    }
    return notFound();
  }

  // Everything below is under /repos/{owner}/{repo}.
  if (segments.length < 3 || segments[0] !== 'repos') return notFound();
  const owner = segments[1] as string;
  const repoName = segments[2] as string;
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined) return notFound();
  const rest = segments.slice(3);

  // GET /repos/{o}/{r}
  if (method === 'GET' && rest.length === 0) {
    return json(200, repoJson(ctx, repo), { 'X-GitHub-Request-Id': rid });
  }
  // PATCH /repos/{o}/{r}
  if (method === 'PATCH' && rest.length === 0) {
    if (typeof body?.description === 'string') repo.description = body.description;
    if (typeof body?.name === 'string' && body.name !== repo.name) {
      store.repos.delete(store.key(owner, repoName));
      repo.name = body.name;
      store.repos.set(store.key(owner, repo.name), repo);
    }
    return json(200, repoJson(ctx, repo), { 'X-GitHub-Request-Id': rid });
  }
  // DELETE /repos/{o}/{r}
  if (method === 'DELETE' && rest.length === 0) {
    store.deleteRepo(owner, repoName);
    return { status: 204, headers: { 'X-GitHub-Request-Id': rid }, body: '' };
  }

  // Branches.
  if (rest[0] === 'branches') {
    if (method === 'GET' && rest.length === 1) {
      const branches = [...repo.refs.entries()]
        .filter(([name]) => name.startsWith('refs/heads/'))
        .map(([name, oid]) => ({ name: name.slice('refs/heads/'.length), commit: { sha: oid }, protected: false }));
      branches.sort((a, b) => (a.name < b.name ? -1 : 1));
      return json(200, branches, { 'X-GitHub-Request-Id': rid });
    }
    if (method === 'GET' && rest.length === 2) {
      const oid = repo.refs.get(`refs/heads/${rest[1]}`);
      if (oid === undefined) return notFound();
      return json(200, { name: rest[1], commit: { sha: oid }, protected: false }, { 'X-GitHub-Request-Id': rid });
    }
    return notFound();
  }

  // Commits.
  if (rest[0] === 'commits' && method === 'GET') {
    const { page, perPage } = pageParams(query);
    const ref = query.get('sha');
    const start = ref === null ? headTarget(repo) : resolveRef(repo, ref);
    if (start === undefined || start === null) return notFound();
    const list = commitListFrom(repo, start, 1000);
    const { slice, hasNext, hasPrev } = paginate(list, page, perPage);
    const link = linkHeader(ctx.host, `/api/v3/repos/${owner}/${repoName}/commits`, { sha: ref ?? '' }, page, perPage, hasNext, hasPrev);
    return json(200, slice.map((oid) => commitJson(ctx, repo, oid)), {
      'X-GitHub-Request-Id': rid,
      ...(link !== undefined ? { Link: link } : {}),
    });
  }

  // git refs.
  if (rest[0] === 'git' && rest[1] === 'refs') {
    if (method === 'POST' && rest.length === 2) {
      const ref = typeof body?.ref === 'string' ? body.ref : '';
      const sha = typeof body?.sha === 'string' ? body.sha : '';
      if (!ref.startsWith('refs/') || !isOid(sha) || repo.objects.get(sha) === undefined) {
        return errorJson(422, 'ref or sha is invalid', rid);
      }
      repo.refs.set(ref, sha);
      return json(201, { ref, object: { sha }, node_id: `REF_${repo.id}_${ref}` }, { 'X-GitHub-Request-Id': rid });
    }
    if (method === 'GET' && rest.length >= 3) {
      const ref = rest.slice(2).join('/');
      const oid = repo.refs.get(ref);
      if (oid === undefined) return notFound();
      return json(200, { ref, object: { sha: oid } }, { 'X-GitHub-Request-Id': rid });
    }
    return notFound();
  }

  // Contents.
  if (rest[0] === 'contents') {
    const path = rest.slice(1).join('/');
    if (path === '') return notFound();
    const refParam = query.get('ref');
    const baseCommit = refParam === null ? headTarget(repo) : resolveRef(repo, refParam);
    const files = baseCommit === undefined || baseCommit === null ? new Map<string, FlatFile>() : filesAt(repo, baseCommit);

    if (method === 'GET') {
      if (files.has(path)) {
        const entry = files.get(path) as FlatFile;
        const object = repo.objects.get(entry.oid);
        if (object === undefined) return notFound();
        return json(200, {
          name: path.split('/').pop(),
          path,
          sha: entry.oid,
          size: object.data.length,
          type: 'file',
          encoding: 'base64',
          content: object.data.toString('base64'),
          url: `http://${ctx.host}/api/v3/repos/${owner}/${repoName}/contents/${path}`,
          html_url: `http://${ctx.host}/${owner}/${repoName}/blob/${repo.defaultBranch}/${path}`,
        }, { 'X-GitHub-Request-Id': rid });
      }
      const children = new Map<string, { type: 'file' | 'dir'; name: string; path: string; sha: string }>();
      for (const [filePath, file] of files) {
        if (!filePath.startsWith(path + '/')) continue;
        const remainder = filePath.slice(path.length + 1);
        const first = remainder.split('/')[0] as string;
        if (remainder.includes('/')) {
          children.set(first, { type: 'dir', name: first, path: `${path}/${first}`, sha: '' });
        } else {
          children.set(first, { type: 'file', name: first, path: filePath, sha: file.oid });
        }
      }
      if (children.size === 0) return notFound();
      const items = [...children.values()].sort((a, b) => (a.name < b.name ? -1 : 1));
      return json(200, items.map((item) => ({ ...item, size: 0, url: `http://${ctx.host}/api/v3/repos/${owner}/${repoName}/contents/${item.path}` })), { 'X-GitHub-Request-Id': rid });
    }

    if (method === 'PUT' || method === 'DELETE') {
      const message = typeof body?.message === 'string' ? body.message : `${method === 'PUT' ? 'Update' : 'Delete'} ${path}`;
      const branch = typeof body?.branch === 'string' ? body.branch : repo.defaultBranch;
      const branchRef = `refs/heads/${branch}`;
      let branchTip = repo.refs.get(branchRef);
      if (branchTip === undefined) {
        // A contents write to a branch that does not exist yet creates it
        // from the default branch tip (or unborn, for an empty repo).
        branchTip = headTarget(repo);
      }
      const currentFiles = branchTip === undefined ? new Map<string, FlatFile>() : filesAt(repo, branchTip);
      if (method === 'PUT') {
        const contentB64 = typeof body?.content === 'string' ? body.content : '';
        const decoded = Buffer.from(contentB64, 'base64');
        const exists = currentFiles.has(path);
        const sha = typeof body?.sha === 'string' ? body.sha : undefined;
        if (exists) {
          const currentSha = currentFiles.get(path)?.oid;
          if (sha === undefined || sha !== currentSha) return errorJson(409, 'sha does not match the current blob', rid);
        }
        const blobOid = putObject(repo, 'blob', decoded);
        const nextFiles = new Map(currentFiles);
        nextFiles.set(path, { mode: '100644', oid: blobOid });
        const tree = buildTreeFromFlat(repo, nextFiles);
        const parents = branchTip === undefined ? [] : [branchTip];
        const commit = makeServerCommit(store, repo, tree, parents, message);
        repo.refs.set(branchRef, commit);
        return json(exists ? 200 : 201, {
          content: { name: path.split('/').pop(), path, sha: blobOid, size: decoded.length, type: 'file', encoding: 'base64', content: contentB64 },
          commit: { sha: commit, message },
        }, { 'X-GitHub-Request-Id': rid });
      }
      // DELETE
      if (!currentFiles.has(path)) return notFound();
      const currentSha = currentFiles.get(path)?.oid;
      const sha = typeof body?.sha === 'string' ? body.sha : undefined;
      if (sha !== undefined && sha !== currentSha) return errorJson(409, 'sha does not match the current blob', rid);
      const nextFiles = new Map(currentFiles);
      nextFiles.delete(path);
      const tree = buildTreeFromFlat(repo, nextFiles);
      const parents = branchTip === undefined ? [] : [branchTip];
      const commit = makeServerCommit(store, repo, tree, parents, message);
      repo.refs.set(branchRef, commit);
      return json(200, { commit: { sha: commit, message } }, { 'X-GitHub-Request-Id': rid });
    }
    return notFound();
  }

  // Labels.
  if (rest[0] === 'labels') {
    if (method === 'GET' && rest.length === 1) {
      return json(200, [...repo.labels.values()].map(labelJson), { 'X-GitHub-Request-Id': rid });
    }
    if (method === 'POST' && rest.length === 1) {
      const name = typeof body?.name === 'string' ? body.name : '';
      if (name === '') return errorJson(422, 'name is required', rid);
      const label = { name, color: typeof body?.color === 'string' ? body.color : 'ededed', description: typeof body?.description === 'string' ? body.description : '' };
      repo.labels.set(name, label);
      return json(201, labelJson(label), { 'X-GitHub-Request-Id': rid });
    }
    return notFound();
  }

  // Issues.
  if (rest[0] === 'issues') {
    if (rest.length === 1) {
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        const sort = query.get('sort') ?? 'created';
        const direction = query.get('direction') ?? 'desc';
        let list = [...repo.issues.values()];
        if (state !== 'all') list = list.filter((issue) => issue.state === state);
        list.sort((a, b) => {
          const keyA = sort === 'created' ? a.createdSeq : a.updatedSeq;
          const keyB = sort === 'created' ? b.createdSeq : b.updatedSeq;
          return direction === 'asc' ? keyA - keyB : keyB - keyA;
        });
        const { page, perPage } = pageParams(query);
        const { slice, hasNext, hasPrev } = paginate(list, page, perPage);
        const link = linkHeader(ctx.host, `/api/v3/repos/${owner}/${repoName}/issues`, { state, sort, direction }, page, perPage, hasNext, hasPrev);
        return json(200, slice.map((issue) => issueJson(ctx, repo, issue)), {
          'X-GitHub-Request-Id': rid,
          ...(link !== undefined ? { Link: link } : {}),
        });
      }
      if (method === 'POST') {
        const title = typeof body?.title === 'string' ? body.title : '';
        if (title === '') return errorJson(422, 'title is required', rid);
        const requestedLabels = Array.isArray(body?.labels) ? (body.labels as unknown[]) : [];
        const labels: string[] = [];
        for (const raw of requestedLabels) {
          const name = typeof raw === 'string' ? raw : typeof raw === 'object' && raw !== null ? String((raw as Record<string, unknown>).name ?? '') : '';
          if (name === '') continue;
          if (!repo.labels.has(name)) return errorJson(422, `label does not exist: ${name}`, rid);
          labels.push(name);
        }
        const number = repo.nextNumber;
        repo.nextNumber += 1;
        const seq = store.tick();
        const issue: Issue = {
          id: number,
          number,
          title,
          body: typeof body?.body === 'string' ? body.body : '',
          state: 'open',
          labels,
          user: VIEWER_LOGIN,
          createdSeq: seq,
          updatedSeq: seq,
          comments: [],
        };
        repo.issues.set(number, issue);
        return json(201, issueJson(ctx, repo, issue), { 'X-GitHub-Request-Id': rid });
      }
      return notFound();
    }
    const number = Number.parseInt(rest[1] as string, 10);
    const issue = repo.issues.get(number);
    if (issue === undefined) return notFound();
    if (rest.length === 2) {
      if (method === 'GET') return json(200, issueJson(ctx, repo, issue), { 'X-GitHub-Request-Id': rid });
      if (method === 'PATCH') {
        if (typeof body?.state === 'string' && (body.state === 'open' || body.state === 'closed')) {
          issue.state = body.state;
          issue.updatedSeq = store.tick();
        }
        if (typeof body?.title === 'string') issue.title = body.title;
        if (typeof body?.body === 'string') issue.body = body.body;
        return json(200, issueJson(ctx, repo, issue), { 'X-GitHub-Request-Id': rid });
      }
      return notFound();
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      if (method === 'GET') {
        return json(200, issue.comments.map((comment) => ({
          id: comment.id,
          body: comment.body,
          user: userJson(),
          created_at: ctx.store.isoOf(comment.createdSeq),
          updated_at: ctx.store.isoOf(comment.createdSeq),
        })), { 'X-GitHub-Request-Id': rid });
      }
      if (method === 'POST') {
        const commentBody = typeof body?.body === 'string' ? body.body : '';
        const comment = { id: repo.nextCommentId, body: commentBody, user: VIEWER_LOGIN, createdSeq: store.tick() };
        repo.nextCommentId += 1;
        issue.comments.push(comment);
        return json(201, { id: comment.id, body: comment.body, user: userJson(), created_at: ctx.store.isoOf(comment.createdSeq) }, { 'X-GitHub-Request-Id': rid });
      }
      return notFound();
    }
    return notFound();
  }

  // Pull requests.
  if (rest[0] === 'pulls') {
    if (rest.length === 1) {
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        let list = [...repo.pulls.values()];
        if (state !== 'all') list = list.filter((pull) => pull.state === state);
        list.sort((a, b) => b.createdSeq - a.createdSeq);
        const { page, perPage } = pageParams(query);
        const { slice, hasNext, hasPrev } = paginate(list, page, perPage);
        const link = linkHeader(ctx.host, `/api/v3/repos/${owner}/${repoName}/pulls`, { state }, page, perPage, hasNext, hasPrev);
        return json(200, slice.map((pull) => pullJson(ctx, repo, pull)), {
          'X-GitHub-Request-Id': rid,
          ...(link !== undefined ? { Link: link } : {}),
        });
      }
      if (method === 'POST') {
        const title = typeof body?.title === 'string' ? body.title : '';
        const head = typeof body?.head === 'string' ? body.head : '';
        const base = typeof body?.base === 'string' ? body.base : '';
        if (title === '' || head === '' || base === '') return errorJson(422, 'title, head and base are required', rid);
        const headSha = repo.refs.get(`refs/heads/${head}`);
        const baseSha = repo.refs.get(`refs/heads/${base}`);
        if (headSha === undefined || baseSha === undefined) return errorJson(422, 'head or base branch does not exist', rid);
        const duplicate = [...repo.pulls.values()].some((pull) => pull.state === 'open' && pull.head === head && pull.base === base);
        if (duplicate) return errorJson(422, 'a pull request already exists for this head', rid);
        const number = repo.nextNumber;
        repo.nextNumber += 1;
        const seq = store.tick();
        const pull: PullRequest = {
          id: number,
          number,
          title,
          body: typeof body?.body === 'string' ? body.body : '',
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
        return json(201, pullJson(ctx, repo, pull), { 'X-GitHub-Request-Id': rid });
      }
      return notFound();
    }
    const number = Number.parseInt(rest[1] as string, 10);
    const pull = repo.pulls.get(number);
    if (pull === undefined) return notFound();
    if (rest.length === 2) {
      if (method === 'GET') return json(200, pullJson(ctx, repo, pull), { 'X-GitHub-Request-Id': rid });
      return notFound();
    }
    if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
      const base = mergeBase(repo, pull.headSha, pull.baseSha);
      const baseFiles = base === null ? new Map<string, FlatFile>() : filesAt(repo, base);
      const headFiles = filesAt(repo, pull.headSha);
      const changes = diffFlat(baseFiles, headFiles).map((change) => {
        const counts = lineCounts(blobText(repo, change.oldOid), blobText(repo, change.newOid));
        return {
          sha: change.newOid ?? change.oldOid,
          filename: change.path,
          status: change.status,
          additions: counts.additions,
          deletions: counts.deletions,
          changes: counts.additions + counts.deletions,
          blob_url: '',
          raw_url: '',
          contents_url: '',
        };
      });
      return json(200, changes, { 'X-GitHub-Request-Id': rid });
    }
    if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
      if (pull.state !== 'open') return errorJson(405, 'pull request is not open', rid);
      if (prConflicts(repo, pull)) return errorJson(405, 'merge conflict', rid);
      const mergeMethod = typeof body?.merge_method === 'string' ? body.merge_method : 'merge';
      const baseTipSha = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
      const base = mergeBase(repo, pull.headSha, baseTipSha);
      const baseFiles = base === null ? new Map<string, FlatFile>() : filesAt(repo, base);
      const headFiles = filesAt(repo, pull.headSha);
      const baseTipFiles = filesAt(repo, baseTipSha);
      const merged = mergedFiles(baseTipFiles, baseFiles, headFiles);
      const tree = buildTreeFromFlat(repo, merged);
      const parents = mergeMethod === 'squash' ? [baseTipSha] : [baseTipSha, pull.headSha];
      const message = mergeMethod === 'squash'
        ? `${pull.title} (#${pull.number})\n\n${pull.body}`.trim()
        : `Merge pull request #${pull.number} from ${repo.owner}/${pull.head}\n\n${pull.title}`;
      const commit = makeServerCommit(store, repo, tree, parents, message);
      repo.refs.set(`refs/heads/${pull.base}`, commit);
      pull.state = 'closed';
      pull.merged = true;
      pull.mergedAt = ctx.store.isoOf(1_735_689_600 + ctx.store.seq);
      pull.mergeCommitSha = commit;
      pull.updatedSeq = store.tick();
      return json(200, { sha: commit, merged: true, message }, { 'X-GitHub-Request-Id': rid });
    }
    return notFound();
  }

  return notFound();
}

/** The GraphQL `repository` lookup needs the same shared counter as REST: exposed for graphql.ts. */
export function nextNumber(repo: Repo): number {
  const number = repo.nextNumber;
  repo.nextNumber += 1;
  return number;
}
