// MCP (Model Context Protocol), Streamable HTTP at /mcp. The modern era
// (2026-07-28) is preferred: `server/discover` reports the supported
// versions; `initialize` + `notifications/initialized` stay as the legacy
// fallback. Tools are answered from the same store as REST and GraphQL, so
// a repository or issue created over MCP reads back identically over REST.
//
// Every MCP request carries its revision in `_meta` and the
// `MCP-Protocol-Version` header; responses are plain application/json.
// A missing or wrong bearer token is 401 on every /mcp POST (enforced in
// app.ts, before this handler runs).

import { errorJson, json, jsonObject } from './http.ts';
import type { HttpResult } from './http.ts';
import { handleRest } from './rest.ts';
import { VIEWER_LOGIN, type Store } from './store.ts';

const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-03-26', '2024-11-05'];
const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
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
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
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
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
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
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed, or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge an open pull request by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function rpcError(id: unknown, code: number, message: string): HttpResult {
  return json(200, { jsonrpc: '2.0', id: id ?? null, error: { code, message } });
}

function rpcResult(id: unknown, result: unknown): HttpResult {
  return json(200, { jsonrpc: '2.0', id: id ?? null, result });
}

function toolText(payload: unknown): { content: Array<{ type: 'text'; text: string }>; structuredContent: unknown } {
  return { content: [{ type: 'text', text: JSON.stringify(payload) }], structuredContent: payload };
}

function toolError(message: string): { content: Array<{ type: 'text'; text: string }>; isError: true } {
  return { content: [{ type: 'text', text: message }], isError: true };
}

interface McpContext {
  store: Store;
  requestId: string;
  host: string;
}

/** Call the REST layer from a tool, reusing its validation and shapes. */
async function rest(ctx: McpContext, method: string, path: string, body: Record<string, unknown> | null): Promise<{ status: number; value: unknown }> {
  const url = new URL(path, 'http://localhost');
  const segments = url.pathname.split('/').filter((part) => part !== '');
  const apiSegments = segments[0] === 'api' && segments[1] === 'v3' ? segments.slice(2) : segments;
  const result = await handleRest({ store: ctx.store, requestId: ctx.requestId, host: ctx.host }, method, apiSegments, url.searchParams, body);
  const text = result.body === undefined ? '' : Buffer.isBuffer(result.body) ? result.body.toString('utf8') : result.body;
  let value: unknown = text;
  try {
    value = JSON.parse(text);
  } catch {
    // Keep the raw text.
  }
  return { status: result.status, value };
}

function pickArg(args: Record<string, unknown>, ...names: string[]): unknown {
  for (const name of names) {
    if (args[name] !== undefined) return args[name];
  }
  return undefined;
}

async function callTool(ctx: McpContext, name: string, args: Record<string, unknown>): Promise<unknown> {
  const owner = String(pickArg(args, 'owner') ?? '');
  const repoName = String(pickArg(args, 'repo', 'repository') ?? '');
  const fullName = String(pickArg(args, 'full_name') ?? '');
  const [ownerFromFull, repoFromFull] = fullName.includes('/') ? fullName.split('/') : [undefined, undefined];
  const resolvedOwner = owner !== '' ? owner : ownerFromFull ?? VIEWER_LOGIN;
  const resolvedRepo = repoName !== '' ? repoName : repoFromFull ?? '';

  switch (name) {
    case 'create_repo': {
      const repoNameArg = String(pickArg(args, 'name') ?? '');
      if (repoNameArg === '') return toolError('missing required argument: name');
      const result = await rest(ctx, 'POST', '/api/v3/user/repos', {
        name: repoNameArg,
        description: typeof args.description === 'string' ? args.description : '',
        private: args.private === true,
      });
      if (result.status !== 201) return toolError(`create_repo failed: ${JSON.stringify(result.value)}`);
      return toolText(result.value);
    }
    case 'get_repo': {
      if (resolvedRepo === '') return toolError('missing required argument: repo');
      const result = await rest(ctx, 'GET', `/api/v3/repos/${resolvedOwner}/${resolvedRepo}`, null);
      if (result.status !== 200) return toolError(`get_repo failed: ${JSON.stringify(result.value)}`);
      return toolText(result.value);
    }
    case 'create_issue': {
      const title = String(pickArg(args, 'title') ?? '');
      if (resolvedRepo === '' || title === '') return toolError('missing required argument: repo or title');
      const result = await rest(ctx, 'POST', `/api/v3/repos/${resolvedOwner}/${resolvedRepo}/issues`, {
        title,
        body: typeof args.body === 'string' ? args.body : '',
      });
      if (result.status !== 201) return toolError(`create_issue failed: ${JSON.stringify(result.value)}`);
      return toolText(result.value);
    }
    case 'list_issues': {
      if (resolvedRepo === '') return toolError('missing required argument: repo');
      const state = typeof args.state === 'string' ? args.state : 'open';
      const result = await rest(ctx, 'GET', `/api/v3/repos/${resolvedOwner}/${resolvedRepo}/issues?state=${encodeURIComponent(state)}`, null);
      if (result.status !== 200) return toolError(`list_issues failed: ${JSON.stringify(result.value)}`);
      const issues = result.value as Array<Record<string, unknown>>;
      return toolText({ issues });
    }
    case 'create_pull_request': {
      const title = String(pickArg(args, 'title') ?? '');
      const head = String(pickArg(args, 'head') ?? '');
      const base = String(pickArg(args, 'base') ?? '');
      if (resolvedRepo === '' || title === '' || head === '' || base === '') {
        return toolError('missing required argument: repo, title, head or base');
      }
      const result = await rest(ctx, 'POST', `/api/v3/repos/${resolvedOwner}/${resolvedRepo}/pulls`, {
        title,
        body: typeof args.body === 'string' ? args.body : '',
        head,
        base,
      });
      if (result.status !== 201) return toolError(`create_pull_request failed: ${JSON.stringify(result.value)}`);
      return toolText(result.value);
    }
    case 'merge_pull_request': {
      const pullNumber = pickArg(args, 'pull_number', 'number');
      if (resolvedRepo === '' || typeof pullNumber !== 'number') return toolError('missing required argument: repo or pull_number');
      const result = await rest(ctx, 'PUT', `/api/v3/repos/${resolvedOwner}/${resolvedRepo}/pulls/${pullNumber}/merge`, { merge_method: 'merge' });
      if (result.status !== 200) return toolError(`merge_pull_request failed: ${JSON.stringify(result.value)}`);
      return toolText(result.value);
    }
    default:
      return null;
  }
}

export async function handleMcp(ctx: McpContext, rawBody: Buffer): Promise<HttpResult> {
  const parsed = jsonObject(rawBody);
  if (parsed === null) {
    return json(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  const id = parsed.id ?? null;
  const method = typeof parsed.method === 'string' ? parsed.method : '';
  const params = (parsed.params ?? {}) as Record<string, unknown>;
  const meta = (params._meta ?? {}) as Record<string, unknown>;

  switch (method) {
    case 'server/discover': {
      return rpcResult(id, {
        supportedVersions: SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
      });
    }
    case 'initialize': {
      const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const version = SUPPORTED_VERSIONS.includes(requested) ? requested : SUPPORTED_VERSIONS[0];
      return rpcResult(id, {
        protocolVersion: version,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
      });
    }
    case 'notifications/initialized':
    case 'notifications/cancelled':
    case 'notifications/progress': {
      return { status: 202, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: '' };
    }
    case 'tools/list': {
      return rpcResult(id, { tools: TOOLS, _meta: meta });
    }
    case 'tools/call': {
      const name = typeof params.name === 'string' ? params.name : '';
      const args = (params.arguments ?? {}) as Record<string, unknown>;
      const tool = TOOLS.find((candidate) => candidate.name === name);
      if (tool === undefined) return rpcError(id, -32602, `unknown tool: ${name}`);
      for (const required of tool.inputSchema.required) {
        if (args[required] === undefined || args[required] === '') {
          if (required === 'pull_number' && args.number !== undefined) continue;
          return rpcResult(id, toolError(`missing required argument: ${required}`));
        }
      }
      const outcome = await callTool(ctx, name, args);
      if (outcome === null) return rpcError(id, -32602, `unknown tool: ${name}`);
      return rpcResult(id, outcome);
    }
    default:
      return rpcError(id, -32601, `method not found: ${method}`);
  }
}
