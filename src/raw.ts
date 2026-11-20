// Raw-TCP HTTP/1.1 server for the forge's hot surfaces (v3-node, P1).
//
// Why: node:http pays per request for a Request/Response object pair, a
// header map, and header framing — on the timed workloads (2,000 sequential
// GETs on one keep-alive socket, 50 concurrent clones, 200 sequential
// pushes/posts) that overhead is most of the wall time. This server parses
// HTTP/1.1 by hand from raw bytes and serves three fast paths with almost no
// per-request work:
//
//   A. GET /api/v3/repos/{o}/{r}/issues/{n} — pre-built full HTTP frame,
//      split around the per-request X-GitHub-Request-Id and written as one
//      socket.write; the frame is cached per issue and cleared by the same
//      clearIssueCache() calls as the v2 body cache.
//   B. GET ...git/info/refs?service= — the v2 advertisement frame is built
//      once; the v0 advertisement body is already cached by advertisementV0.
//   C. POST .../issues — a small hand scanner pulls `title`/`body` straight
//      out of the request Buffer (no toString + JSON.parse round-trip);
//      anything unusual (labels, non-string fields, escapes it cannot prove)
//      falls back to JSON.parse + the normal REST handler.
//
// Everything else goes through the exact same handlers app.ts uses
// (handleRest / handleGraphQL / handleMcp / the git.ts protocol functions),
// so behaviour, status codes and state export are unchanged. The only
// differences from node:http framing are the absence of a `Date` header
// and of `Connection: keep-alive` (HTTP/1.1 keeps alive by default).

import { createServer, type Server, type Socket } from 'node:net';
import { EMPTY_BODY, errorJson, parseJsonBody, type HttpResult } from './http.ts';

const MAX_HEADER_BYTES = 64 * 1024;
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
import { handleRest, issueBody, type RestContext } from './rest.ts';
import { type Issue, type Repo, type Store } from './store.ts';

const REASON: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

const EMPTY = Buffer.alloc(0);

/** One request, fully buffered: method, path, query, the headers the router needs, body. */
interface RawRequest {
  method: string;
  path: string;
  query: string;
  authorization: string;
  host: string;
  gitProtocol: string;
  body: Buffer;
  close: boolean;
}

/** Value of header `name` (lowercase, with its colon) in `head`; '' when absent.
 * Names are matched against the lowercased head (clients send `Content-Length`,
 * `content-length`, `Git-Protocol`, ... in varying cases); the value is sliced
 * from the original head at the same offsets. */
function headerValue(head: string, lower: string, name: string): string {
  const at = lower.indexOf(`\r\n${name}:`);
  if (at < 0) return '';
  let start = at + name.length + 3;
  if (head.charCodeAt(start) === 32) start += 1;
  let end = head.indexOf('\r\n', start);
  if (end < 0) end = head.length;
  let value = head.slice(start, end);
  const last = value.charCodeAt(value.length - 1);
  if (last === 32 || last === 9) value = value.trimEnd();
  return value;
}

/** Parse one complete request out of `buf`, or null when more bytes are needed. */
function tryParseRequest(buf: Buffer): { request: RawRequest; consumed: number } | null {
  const headerEnd = buf.indexOf('\r\n\r\n');
  if (headerEnd < 0) {
    if (buf.length > MAX_HEADER_BYTES) throw new Error('request header too large');
    return null;
  }
  const head = buf.subarray(0, headerEnd).toString('ascii');
  const lower = head.toLowerCase();
  const lineEnd = head.indexOf('\r\n');
  const requestLine = lineEnd < 0 ? head : head.slice(0, lineEnd);
  const sp1 = requestLine.indexOf(' ');
  const sp2 = sp1 < 0 ? -1 : requestLine.indexOf(' ', sp1 + 1);
  const method = sp1 < 0 ? '' : requestLine.slice(0, sp1);
  const target = sp1 < 0 || sp2 < 0 ? '/' : requestLine.slice(sp1 + 1, sp2);
  const version = sp2 < 0 ? 'HTTP/1.1' : requestLine.slice(sp2 + 1);
  const q = target.indexOf('?');
  const path = q < 0 ? target : target.slice(0, q);
  const query = q < 0 ? '' : target.slice(q + 1);
  const authorization = headerValue(head, lower, 'authorization');
  const host = headerValue(head, lower, 'host');
  const gitProtocol = headerValue(head, lower, 'git-protocol');
  const lengthText = headerValue(head, lower, 'content-length');
  const transferEncoding = headerValue(head, lower, 'transfer-encoding');
  const bodyStart = headerEnd + 4;
  let body: Buffer;
  let consumed: number;
  if (transferEncoding.toLowerCase().includes('chunked')) {
    const parsed = tryParseChunked(buf, bodyStart);
    if (parsed === null) return null;
    body = parsed.body;
    consumed = bodyStart + parsed.consumed;
  } else {
    const length = lengthText === '' ? 0 : Number.parseInt(lengthText, 10);
    const contentLength = Number.isFinite(length) && length > 0 ? length : 0;
    if (buf.length < bodyStart + contentLength) return null;
    body = contentLength === 0 ? EMPTY_BODY : buf.subarray(bodyStart, bodyStart + contentLength);
    consumed = bodyStart + contentLength;
  }
  const connection = headerValue(head, lower, 'connection').toLowerCase();
  const close = connection === 'close' || (version === 'HTTP/1.0' && connection !== 'keep-alive');
  return { request: { method, path, query, authorization, host, gitProtocol, body, close }, consumed };
}

/** Decode a chunked body starting at `start`; null when incomplete. */
function tryParseChunked(buf: Buffer, start: number): { body: Buffer; consumed: number } | null {
  const chunks: Buffer[] = [];
  let pos = start;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', pos);
    if (lineEnd < 0) return null;
    const sizeText = buf.subarray(pos, lineEnd).toString('ascii').split(';')[0]?.trim() ?? '';
    const size = Number.parseInt(sizeText, 16);
    if (!Number.isFinite(size) || size < 0) throw new Error('bad chunk size');
    pos = lineEnd + 2;
    if (size === 0) {
      // Trailers, if any, end at an empty line; a bare CRLF ends them at once.
      if (buf.subarray(pos, pos + 2).toString('ascii') === '\r\n') return { body: Buffer.concat(chunks), consumed: pos + 2 - start };
      const trailerEnd = buf.indexOf('\r\n\r\n', pos);
      if (trailerEnd < 0) return null;
      return { body: Buffer.concat(chunks), consumed: trailerEnd + 4 - start };
    }
    if (buf.length < pos + size + 2) return null;
    chunks.push(buf.subarray(pos, pos + size));
    pos += size + 2;
  }
}

/** Build one HTTP frame (header + body) from a handler's HttpResult. */
function resultFrame(result: HttpResult, rid: string): Buffer {
  const body = result.body === undefined
    ? EMPTY
    : Buffer.isBuffer(result.body)
      ? result.body
      : Buffer.from(result.body, 'utf8');
  const headers = result.headers ?? {};
  let head = `HTTP/1.1 ${result.status} ${REASON[result.status] ?? ''}\r\n`;
  let hasContentType = false;
  let hasRid = false;
  let hasLength = false;
  for (const [name, value] of Object.entries(headers)) {
    head += `${name}: ${value}\r\n`;
    const lower = name.toLowerCase();
    if (lower === 'content-type') hasContentType = true;
    else if (lower === 'x-github-request-id') hasRid = true;
    else if (lower === 'content-length') hasLength = true;
  }
  if (!hasContentType) head += 'Content-Type: application/json; charset=utf-8\r\n';
  if (!hasRid) head += `X-GitHub-Request-Id: ${rid}\r\n`;
  if (!hasLength) head += `Content-Length: ${body.length}\r\n`;
  head += '\r\n';
  return Buffer.concat([Buffer.from(head, 'ascii'), body]);
}

/** A frame split around the request id: head ends just before it, tail starts just after it. */
interface SplitFrame {
  head: Buffer;
  tail: Buffer;
}

function splitFrame(status: number, contentType: string, body: Buffer): SplitFrame {
  const head = Buffer.from(
    `HTTP/1.1 ${status} ${REASON[status] ?? ''}\r\nContent-Type: ${contentType}\r\nContent-Length: ${body.length}\r\nX-GitHub-Request-Id: `,
    'ascii',
  );
  const tail = Buffer.concat([Buffer.from('\r\n\r\n', 'ascii'), body]);
  return { head, tail };
}

/**
 * Write a split frame as ONE socket.write of one contiguous Buffer.
 * (Three small writes — head, id, tail — cost more in write() overhead and
 * string conversion than the single ~600-byte memcpy that joins them.)
 */
function writeSplit(socket: Socket, frame: SplitFrame, rid: string): void {
  socket.write(Buffer.concat([frame.head, Buffer.from(rid, 'ascii'), frame.tail]));
}

// ---------------------------------------------------------------------------
// Fast-path C: pull `title` / `body` strings out of a small JSON object body.
// ---------------------------------------------------------------------------

const ESCAPES: Record<string, string> = {
  '"': '"',
  '\\': '\\',
  '/': '/',
  b: '\b',
  f: '\f',
  n: '\n',
  r: '\r',
  t: '\t',
};

/** Parse a JSON string starting at the `"` at `start`; null when malformed. */
function parseJsonStringAt(buf: Buffer, start: number): { value: string; next: number } | null {
  if (buf[start] !== 0x22) return null;
  const parts: string[] = [];
  let segStart = start + 1;
  let i = start + 1;
  while (i < buf.length) {
    const byte = buf[i] as number;
    if (byte === 0x22) {
      if (i > segStart) parts.push(buf.subarray(segStart, i).toString('utf8'));
      return { value: parts.join(''), next: i + 1 };
    }
    if (byte === 0x5c) {
      if (i > segStart) parts.push(buf.subarray(segStart, i).toString('utf8'));
      const esc = String.fromCharCode(buf[i + 1] as number);
      if (esc === 'u') {
        if (i + 5 >= buf.length) return null;
        const hex = buf.subarray(i + 2, i + 6).toString('ascii');
        if (!/^[0-9a-fA-F]{4}$/.test(hex)) return null;
        parts.push(String.fromCharCode(Number.parseInt(hex, 16)));
        i += 6;
        segStart = i;
        continue;
      }
      const mapped = ESCAPES[esc];
      if (mapped === undefined) return null;
      parts.push(mapped);
      i += 2;
      segStart = i;
      continue;
    }
    if (byte < 0x20) return null;
    i += 1;
  }
  return null;
}

/** Find `"key": "<string>"` in a flat JSON object body; null when absent or not a string. */
function findStringField(buf: Buffer, key: string): string | null | undefined {
  const at = buf.indexOf(`"${key}"`, 0, 'utf8');
  if (at < 0) return undefined;
  let i = at + key.length + 2;
  while (i < buf.length && (buf[i] === 0x20 || buf[i] === 0x09 || buf[i] === 0x0a || buf[i] === 0x0d)) i += 1;
  if (buf[i] !== 0x3a) return null;
  i += 1;
  while (i < buf.length && (buf[i] === 0x20 || buf[i] === 0x09 || buf[i] === 0x0a || buf[i] === 0x0d)) i += 1;
  const parsed = parseJsonStringAt(buf, i);
  return parsed === null ? null : parsed.value;
}

/**
 * Fast issue fields, or null to fall back to JSON.parse. Only handles the
 * flat `{title, body}` shape the timed write workload sends; any `labels`
 * key, non-string field, or unparseable string means "not this shape".
 */
function fastIssueFields(body: Buffer): { title: string; body: string } | null {
  if (body.indexOf('"labels"', 0, 'utf8') >= 0) return null;
  const title = findStringField(body, 'title');
  if (title === null || title === undefined) return null;
  const bodyField = findStringField(body, 'body');
  if (bodyField === null) return null;
  return { title, body: bodyField === undefined ? '' : bodyField };
}

function decodeSegment(value: string): string {
  if (value.indexOf('%') < 0) return value;
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

/** Match GET /api/v3/repos/{owner}/{repo}/issues/{n} without a regex. */
function matchIssueGet(path: string): { owner: string; name: string; num: number } | null {
  if (!path.startsWith('/api/v3/repos/')) return null;
  const rest = path.slice(14);
  const s1 = rest.indexOf('/');
  if (s1 < 0) return null;
  const s2 = rest.indexOf('/', s1 + 1);
  if (s2 < 0 || rest.slice(s2 + 1, s2 + 8) !== 'issues/') return null;
  const numText = rest.slice(s2 + 8);
  if (numText === '') return null;
  let num = 0;
  for (let i = 0; i < numText.length; i += 1) {
    const c = numText.charCodeAt(i);
    if (c < 48 || c > 57) return null;
    num = num * 10 + (c - 48);
  }
  return { owner: rest.slice(0, s1), name: rest.slice(s1 + 1, s2), num };
}

/** Match POST /api/v3/repos/{owner}/{repo}/issues without a regex. */
function matchIssuesPost(path: string): { owner: string; name: string } | null {
  if (!path.startsWith('/api/v3/repos/')) return null;
  const rest = path.slice(14);
  const s1 = rest.indexOf('/');
  if (s1 < 0) return null;
  const s2 = rest.indexOf('/', s1 + 1);
  if (s2 < 0 || rest.slice(s2 + 1) !== 'issues') return null;
  return { owner: rest.slice(0, s1), name: rest.slice(s1 + 1, s2) };
}

/** Match /{owner}/{repo}.git/{servicePath} without a regex. Note `.git` is a
 * suffix of the repo segment, not its own segment: `/{o}/{r}.git/...`. */
function matchGit(path: string): { owner: string; name: string; servicePath: string } | null {
  const gi = path.indexOf('.git/');
  if (gi < 0) return null;
  const head = path.slice(1, gi);
  const slash = head.indexOf('/');
  if (slash < 0 || head.indexOf('/', slash + 1) >= 0) return null;
  return { owner: head.slice(0, slash), name: head.slice(slash + 1), servicePath: path.slice(gi + 5) };
}

export function createRawServer(store: Store, shopToken: string): Server {
  let requestCounter = 0;
  /** The next request number; its string form is `req-` + 7 zero-padded digits. */
  const nextRidNum = (): number => {
    requestCounter += 1;
    return requestCounter;
  };
  const ridString = (num: number): string => `req-${String(num).padStart(7, '0')}`;

  // Constant frames, built once: the v2 advertisement body never changes,
  // and the healthz / 401 / 404 JSON bodies are constants.
  const advV2Frame = splitFrame(200, 'application/x-git-upload-pack-advertisement', advertisementV2());
  const healthzFrame = splitFrame(200, 'application/json; charset=utf-8', Buffer.from('{"ok":true}', 'utf8'));
  const unauthFrame = splitFrame(401, 'application/json; charset=utf-8', Buffer.from('{"message":"Requires authentication"}', 'utf8'));
  const notFoundFrame = splitFrame(404, 'application/json; charset=utf-8', Buffer.from('{"message":"Not Found"}', 'utf8'));

  const server = createServer((socket: Socket) => {
    socket.setNoDelay(true);
    socket.on('error', () => undefined);

    // Per-connection state: pending input bytes, a one-entry repo cache
    // (the timed workloads all hit one repository), and one writable copy
    // of each cached issue frame (its request-id digits are patched in place).
    let buf: Buffer = EMPTY;
    let pumping = false;
    let lastOwner = '';
    let lastName = '';
    let lastRepo: Repo | undefined;
    const frameCopies = new Map<Issue, Buffer>();

    const getRepo = (owner: string, name: string): Repo | undefined => {
      if (owner === lastOwner && name === lastName && lastRepo !== undefined) return lastRepo;
      const repo = store.getRepo(owner, name);
      lastOwner = owner;
      lastName = name;
      lastRepo = repo;
      return repo;
    };

    const writeResult = (result: HttpResult, rid: string, close: boolean): void => {
      socket.write(resultFrame(result, rid));
      if (close) socket.end();
    };

    /** Handle one fully buffered request; writes exactly one response. */
    const dispatch = async (req: RawRequest): Promise<void> => {
      const ridNum = nextRidNum();
      const rid = ridString(ridNum);
      const host = req.host === '' ? 'localhost' : req.host;
      const ctx: RestContext = { store, requestId: rid, host };

      // /healthz takes no token.
      if (req.method === 'GET' && req.path === '/healthz') {
        writeSplit(socket, healthzFrame, rid);
        if (req.close) socket.end();
        return;
      }

      // Auth: every other endpoint needs the trial's bearer token.
      if (shopToken !== '') {
        if (req.authorization !== `Bearer ${shopToken}`) {
          writeSplit(socket, unauthFrame, rid);
          if (req.close) socket.end();
          return;
        }
      }

      // ---- fast path A: GET single issue ----
      if (req.method === 'GET') {
        const issueMatch = matchIssueGet(req.path);
        if (issueMatch !== null) {
          const repo = getRepo(decodeSegment(issueMatch.owner), decodeSegment(issueMatch.name));
          const issue: Issue | undefined = repo?.issues.get(issueMatch.num);
          if (repo === undefined || issue === undefined) {
            writeSplit(socket, notFoundFrame, rid);
          } else {
            // One writable copy of the cached frame per connection: patch
            // this request's id digits in place, then a single write.
            // A copy is rebuilt when the issue's cached frame was cleared
            // (any mutation) or the host changed.
            let frame = frameCopies.get(issue);
            if (frame === undefined || issue.cachedGetHost !== host) {
              let template = issue.cachedGetFrame;
              if (template === undefined || issue.cachedGetHost !== host) {
                const body = issueBody(ctx, repo, issue);
                const split = splitFrame(200, 'application/json; charset=utf-8', body);
                template = Buffer.concat([split.head, Buffer.from('req-0000000', 'ascii'), split.tail]);
                issue.cachedGetFrame = template;
                issue.cachedGetRidOff = split.head.length;
                issue.cachedGetHost = host;
              }
              frame = Buffer.from(template);
              frameCopies.set(issue, frame);
            }
            const ridOff = issue.cachedGetRidOff as number;
            if (ridNum > 9_999_999) {
              // Counter overflow (never inside a trial): fresh frame instead.
              writeSplit(socket, { head: frame.subarray(0, ridOff), tail: frame.subarray(ridOff + 11) }, rid);
            } else {
              let n = ridNum;
              for (let i = 0; i < 7; i += 1) {
                frame[ridOff + 10 - i] = 48 + (n % 10);
                n = Math.floor(n / 10);
              }
              socket.write(frame);
            }
          }
          if (req.close) socket.end();
          return;
        }
      }

      // ---- fast path C: POST issues (flat title/body shape) ----
      if (req.method === 'POST') {
        const postMatch = matchIssuesPost(req.path);
        if (postMatch !== null) {
          const fast = fastIssueFields(req.body);
          if (fast !== null) {
            const segments = ['repos', decodeSegment(postMatch.owner), decodeSegment(postMatch.name), 'issues'];
            const result = await handleRest(ctx, 'POST', segments, new URLSearchParams(req.query), fast);
            writeResult(result, rid, req.close);
            return;
          }
          // Fall through to the generic REST path (JSON.parse + handleRest).
        }
      }

      // ---- git smart HTTP ----
      const gitMatch = matchGit(req.path);
      if (gitMatch !== null) {
        const owner = decodeSegment(gitMatch.owner);
        const repoName = decodeSegment(gitMatch.name);
        const servicePath = gitMatch.servicePath;
        const repo = getRepo(owner, repoName);
        if (repo === undefined) {
          writeSplit(socket, notFoundFrame, rid);
          if (req.close) socket.end();
          return;
        }
        const body = req.body;
        if (req.method === 'GET' && servicePath === 'info/refs') {
          const service = new URLSearchParams(req.query).get('service') ?? '';
          if (service === 'git-upload-pack') {
            // ---- fast path B: v2 advertisement is one constant frame ----
            if (req.gitProtocol === 'version=2') {
              writeSplit(socket, advV2Frame, rid);
              if (req.close) socket.end();
              return;
            }
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-upload-pack-advertisement' }, body: advertisementV0(repo, 'git-upload-pack') }, rid, req.close);
            return;
          }
          if (service === 'git-receive-pack') {
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-receive-pack-advertisement' }, body: advertisementV0(repo, 'git-receive-pack') }, rid, req.close);
            return;
          }
          writeSplit(socket, notFoundFrame, rid);
          if (req.close) socket.end();
          return;
        }
        if (req.method === 'POST' && servicePath === 'git-upload-pack') {
          const packets = decodePktLines(body);
          const first = packets[0];
          const command = first !== undefined && first.kind === 'data' ? first.payload.toString('utf8').trim() : '';
          if (command === 'command=ls-refs') {
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-upload-pack-result' }, body: lsRefsResponse(repo, parseLsRefsArgs(packets)) }, rid, req.close);
            return;
          }
          if (command === 'command=fetch') {
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-upload-pack-result' }, body: fetchV2Response(repo, parseFetchV2Args(packets)) }, rid, req.close);
            return;
          }
          if (command.startsWith('want ') || command.startsWith('command=fetch') || command === '') {
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-upload-pack-result' }, body: fetchV0Response(repo, parseFetchV0Args(packets)) }, rid, req.close);
            return;
          }
          writeResult(errorJson(400, `unsupported upload-pack command: ${command}`, rid), rid, req.close);
          return;
        }
        if (req.method === 'POST' && servicePath === 'git-receive-pack') {
          try {
            const parsed = parseReceivePack(body);
            const objects = parsed.pack.length > 0 ? readPack(parsed.pack) : new Map();
            const applied = applyPush(repo, store, parsed.commands, objects);
            const sideband = parsed.caps.includes('side-band-64k');
            writeResult({ status: 200, headers: { 'Content-Type': 'application/x-git-receive-pack-result' }, body: pushReportResponse(applied.report, sideband) }, rid, req.close);
          } catch (error) {
            writeResult(errorJson(400, error instanceof Error ? error.message : 'invalid push', rid), rid, req.close);
          }
          return;
        }
        writeSplit(socket, notFoundFrame, rid);
        if (req.close) socket.end();
        return;
      }

      // ---- REST v3 ----
      if (req.path === '/api/v3' || req.path.startsWith('/api/v3/')) {
        const segments = req.path.split('/').filter((part) => part !== '').slice(2);
        let parsedBody: Record<string, unknown> | null = {};
        if (req.body.length > 0) {
          const parsed = parseJsonBody(req.body);
          if (parsed === null) {
            writeResult(errorJson(400, 'Malformed JSON in request body', rid), rid, req.close);
            return;
          }
          if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
            writeResult(errorJson(400, 'Request body must be a JSON object', rid), rid, req.close);
            return;
          }
          parsedBody = parsed as Record<string, unknown>;
        }
        const result = await handleRest(ctx, req.method, segments, new URLSearchParams(req.query), parsedBody);
        writeResult(result, rid, req.close);
        return;
      }

      // ---- GraphQL v4 ----
      if (req.path === '/api/graphql') {
        if (req.method !== 'POST') {
          writeSplit(socket, notFoundFrame, rid);
          if (req.close) socket.end();
          return;
        }
        const parsed = parseJsonBody(req.body);
        if (parsed === null) {
          writeResult(errorJson(400, 'Malformed JSON in request body', rid), rid, req.close);
          return;
        }
        writeResult(handleGraphQL(store, rid, parsed), rid, req.close);
        return;
      }

      // ---- MCP ----
      if (req.path === '/mcp') {
        if (req.method !== 'POST') {
          writeSplit(socket, notFoundFrame, rid);
          if (req.close) socket.end();
          return;
        }
        writeResult(await handleMcp(ctx, req.body), rid, req.close);
        return;
      }

      writeSplit(socket, notFoundFrame, rid);
      if (req.close) socket.end();
    };

    /** Drain complete requests out of `buf`, in order, one at a time. */
    const pump = async (): Promise<void> => {
      if (pumping) return;
      pumping = true;
      try {
        for (;;) {
          let parsed;
          try {
            parsed = tryParseRequest(buf);
          } catch (error) {
            if (!socket.destroyed) {
              const rid = ridString(nextRidNum());
              socket.write(resultFrame(errorJson(400, error instanceof Error ? error.message : 'bad request', rid), rid));
              socket.end();
            }
            return;
          }
          if (parsed === null) return;
          const { request, consumed } = parsed;
          buf = consumed >= buf.length ? EMPTY : Buffer.from(buf.subarray(consumed));
          if (socket.destroyed) return;
          try {
            await dispatch(request);
          } catch (error) {
            if (!socket.destroyed) {
              const rid = ridString(nextRidNum());
              socket.write(resultFrame(errorJson(500, error instanceof Error ? error.message : 'Internal Server Error', rid), rid));
            }
            return;
          }
          if (socket.destroyed) return;
        }
      } finally {
        pumping = false;
      }
    };

    socket.on('data', (chunk: Buffer) => {
      buf = buf.length === 0 ? chunk : Buffer.concat([buf, chunk]);
      void pump();
    });
    socket.on('close', () => {
      buf = EMPTY;
    });
  });

  return server;
}
