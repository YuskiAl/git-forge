// The forge's HTTP server: one process serving four surfaces from one store
// — git smart HTTP, REST v3, GraphQL v4 and MCP — plus /healthz.
//
// Auth: every request except GET /healthz must carry
// `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`; anything else is 401.
// A repository belongs to the login the token names, so the same name under
// another owner is 404, never 403.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { errorJson, json, parseJsonBody, raw, readBody } from './http.ts';
import type { HttpResult } from './http.ts';
import {
  advertisementV0,
  advertisementV2,
  applyPush,
  decodePktLines,
  fetchV0Response,
  fetchV2Response,
  lsRefsResponse,
  parseFetchV0Args,
  parseFetchV2Args,
  parseLsRefsArgs,
  parseReceivePack,
  pushReportResponse,
  readPack,
} from './git.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';
import { Store } from './store.ts';

const GIT_OWNER_REPO = /^\/([^/]+)\/([^/]+)\.git\/(.+)$/;

function send(res: ServerResponse, result: HttpResult): void {
  const body = result.body === undefined ? '' : result.body;
  res.writeHead(result.status, result.headers ?? {});
  res.end(body);
}

export function createApp(store: Store, shopToken: string): ReturnType<typeof createServer> {
  let requestCounter = 0;
  const requestId = (): string => {
    requestCounter += 1;
    return `req-${requestCounter}`;
  };

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const url = new URL(req.url ?? '/', 'http://localhost');
        const path = url.pathname;
        const method = req.method ?? 'GET';
        const rid = requestId();
        const host = req.headers.host ?? 'localhost';

        // /healthz takes no token.
        if (method === 'GET' && path === '/healthz') {
          send(res, json(200, { ok: true }, { 'X-GitHub-Request-Id': rid }));
          return;
        }

        // Auth: every other endpoint needs the trial's bearer token.
        if (shopToken !== '') {
          const authorization = req.headers.authorization ?? '';
          if (authorization !== `Bearer ${shopToken}`) {
            send(res, errorJson(401, 'Requires authentication', rid));
            return;
          }
        }

        // ---- git smart HTTP ----
        const gitMatch = GIT_OWNER_REPO.exec(path);
        if (gitMatch !== null) {
          const owner = decodeURIComponent(gitMatch[1] as string);
          const repoName = decodeURIComponent(gitMatch[2] as string);
          const servicePath = gitMatch[3] as string;
          const repo = store.getRepo(owner, repoName);
          if (repo === undefined) {
            send(res, errorJson(404, 'Not Found', rid));
            return;
          }
          const body = await readBody(req);
          if (method === 'GET' && servicePath === 'info/refs') {
            const service = url.searchParams.get('service') ?? '';
            if (service === 'git-upload-pack') {
              // A client that asks for protocol v2 gets the v2 capability
              // advertisement; everyone else gets the v0 ref advertisement.
              if ((req.headers['git-protocol'] ?? '') === 'version=2') {
                send(res, raw(200, advertisementV2(), 'application/x-git-upload-pack-advertisement', { 'X-GitHub-Request-Id': rid }));
                return;
              }
              send(res, raw(200, advertisementV0(repo, 'git-upload-pack'), 'application/x-git-upload-pack-advertisement', { 'X-GitHub-Request-Id': rid }));
              return;
            }
            if (service === 'git-receive-pack') {
              send(res, raw(200, advertisementV0(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement', { 'X-GitHub-Request-Id': rid }));
              return;
            }
            send(res, errorJson(404, 'Not Found', rid));
            return;
          }
          if (method === 'POST' && servicePath === 'git-upload-pack') {
            const packets = decodePktLines(body);
            const first = packets[0];
            const command = first !== undefined && first.kind === 'data' ? first.payload.toString('utf8').trim() : '';
            if (command === 'command=ls-refs') {
              send(res, raw(200, lsRefsResponse(repo, parseLsRefsArgs(packets)), 'application/x-git-upload-pack-result', { 'X-GitHub-Request-Id': rid }));
              return;
            }
            if (command === 'command=fetch') {
              send(res, raw(200, fetchV2Response(repo, parseFetchV2Args(packets)), 'application/x-git-upload-pack-result', { 'X-GitHub-Request-Id': rid }));
              return;
            }
            if (command.startsWith('want ') || command.startsWith('command=fetch') || command === '') {
              // v0 fetch: the body opens with `want <oid>`.
              send(res, raw(200, fetchV0Response(repo, parseFetchV0Args(packets)), 'application/x-git-upload-pack-result', { 'X-GitHub-Request-Id': rid }));
              return;
            }
            send(res, errorJson(400, `unsupported upload-pack command: ${command}`, rid));
            return;
          }
          if (method === 'POST' && servicePath === 'git-receive-pack') {
            try {
              const parsed = parseReceivePack(body);
              const objects = parsed.pack.length > 0 ? readPack(parsed.pack) : new Map();
              const applied = applyPush(repo, store, parsed.commands, objects);
              const sideband = parsed.caps.includes('side-band-64k');
              send(res, raw(200, pushReportResponse(applied.report, sideband), 'application/x-git-receive-pack-result', { 'X-GitHub-Request-Id': rid }));
            } catch (error) {
              send(res, errorJson(400, error instanceof Error ? error.message : 'invalid push', rid));
            }
            return;
          }
          send(res, errorJson(404, 'Not Found', rid));
          return;
        }

        // ---- REST v3 ----
        if (path === '/api/v3' || path.startsWith('/api/v3/')) {
          const segments = path.split('/').filter((part) => part !== '').slice(2);
          const rawBody = method === 'GET' || method === 'DELETE' ? await readBody(req) : await readBody(req);
          let parsedBody: Record<string, unknown> | null = {};
          if (rawBody.length > 0) {
            const parsed = parseJsonBody(rawBody);
            if (parsed === null) {
              send(res, errorJson(400, 'Malformed JSON in request body', rid));
              return;
            }
            if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
              send(res, errorJson(400, 'Request body must be a JSON object', rid));
              return;
            }
            parsedBody = parsed as Record<string, unknown>;
          }
          const result = await handleRest({ store, requestId: rid, host }, method, segments, url.searchParams, parsedBody);
          send(res, result);
          return;
        }

        // ---- GraphQL v4 ----
        if (path === '/api/graphql') {
          if (method !== 'POST') {
            send(res, errorJson(404, 'Not Found', rid));
            return;
          }
          const rawBody = await readBody(req);
          const parsed = parseJsonBody(rawBody);
          if (parsed === null) {
            send(res, errorJson(400, 'Malformed JSON in request body', rid));
            return;
          }
          send(res, handleGraphQL(store, rid, parsed));
          return;
        }

        // ---- MCP ----
        if (path === '/mcp') {
          if (method !== 'POST') {
            send(res, errorJson(404, 'Not Found', rid));
            return;
          }
          const rawBody = await readBody(req);
          send(res, await handleMcp({ store, requestId: rid, host }, rawBody));
          return;
        }

        send(res, errorJson(404, 'Not Found', rid));
      } catch (error) {
        if (!res.headersSent) {
          send(res, errorJson(500, error instanceof Error ? error.message : 'Internal Server Error', 'req-error'));
        }
      }
    })();
  });
  return server;
}
