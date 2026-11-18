// Shared HTTP plumbing for the four surfaces: a handler returns an
// HttpResult; app.ts writes it to the wire. Every JSON reply is
// application/json, and every REST reply carries X-GitHub-Request-Id.

export interface HttpResult {
  status: number;
  headers?: Record<string, string>;
  body?: Buffer | string;
}

export function json(status: number, value: unknown, extraHeaders?: Record<string, string>): HttpResult {
  return {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...extraHeaders },
    body: JSON.stringify(value),
  };
}

export function raw(status: number, body: Buffer, contentType: string, extraHeaders?: Record<string, string>): HttpResult {
  return { status, headers: { 'Content-Type': contentType, ...extraHeaders }, body };
}

export function errorJson(status: number, message: string, requestId: string): HttpResult {
  return json(status, { message }, { 'X-GitHub-Request-Id': requestId });
}

/** Read a whole request body (bounded; pushes are at most a few MiB). */
export const EMPTY_BODY: Buffer = Buffer.alloc(0);

/** True when the request carries a body (Content-Length > 0 or chunked). GETs in the suite carry none; skipping their body read saves a full event-loop turn per request. */
export function requestHasBody(req: import('node:http').IncomingMessage): boolean {
  const length = req.headers['content-length'];
  if (length !== undefined && Number.parseInt(length, 10) > 0) return true;
  const encoding = req.headers['transfer-encoding'];
  return encoding !== undefined && encoding !== '';
}
export function readBody(req: import('node:http').IncomingMessage, limit = 64 * 1024 * 1024): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > limit) {
        reject(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** Parse a JSON body; returns null on malformed input (the caller sends 400). */
export function parseJsonBody(body: Buffer): unknown {
  if (body.length === 0) return undefined;
  try {
    return JSON.parse(body.toString('utf8'));
  } catch {
    return null;
  }
}

/** A JSON object body, or null when the body is not a JSON object. */
export function jsonObject(body: Buffer): Record<string, unknown> | null {
  const parsed = parseJsonBody(body);
  if (parsed === undefined) return {};
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
  return parsed as Record<string, unknown>;
}

/** Build an absolute URL on this server's own origin from a path + query. */
export function absoluteUrl(host: string, path: string): string {
  return `http://${host}${path}`;
}

/** The `Link` header for paginated lists, in the absolute-URL form the clients follow. */
export function linkHeader(host: string, basePath: string, query: Record<string, string>, page: number, perPage: number, hasNext: boolean, hasPrev: boolean): string | undefined {
  const links: string[] = [];
  const build = (pageNumber: number): string => {
    const params = new URLSearchParams({ ...query, page: String(pageNumber), per_page: String(perPage) });
    return `<${absoluteUrl(host, `${basePath}?${params.toString()}`)}>`;
  };
  if (hasNext) links.push(`${build(page + 1)}; rel="next"`);
  if (hasPrev) links.push(`${build(page - 1)}; rel="prev"`);
  return links.length > 0 ? links.join(', ') : undefined;
}

export interface PageParams {
  page: number;
  perPage: number;
}

export function pageParams(query: URLSearchParams): PageParams {
  const page = Math.max(1, Number.parseInt(query.get('page') ?? '1', 10) || 1);
  const perPage = Math.min(100, Math.max(1, Number.parseInt(query.get('per_page') ?? '30', 10) || 30));
  return { page, perPage };
}

export function paginate<T>(items: readonly T[], page: number, perPage: number): { slice: T[]; hasNext: boolean; hasPrev: boolean } {
  const start = (page - 1) * perPage;
  return { slice: items.slice(start, start + perPage), hasNext: start + perPage < items.length, hasPrev: page > 1 };
}
