// git, implemented on the standard library only: object hashing, tree /
// commit / tag parsing and building, packfile read (plain + OFS/REF deltas)
// and write (plain objects), reachability, fast-forward checks, merge-base,
// shallow boundaries, and the smart-HTTP protocol (v2 ls-refs/fetch,
// v0 upload-pack/receive-pack) as pkt-line streams.
//
// No wall clock, no randomness: object ids come from content alone, and
// server-made commits take their timestamp from the store's sequence.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { EPOCH_SEC, VIEWER_EMAIL, VIEWER_NAME, type GitObject, type GitObjectType, type Repo, type Store } from './store.ts';

export const ZERO_OID = '0'.repeat(40);
const HEX40 = /^[0-9a-f]{40}$/;
const PKT_MAX = 65520;

export function isOid(value: string): boolean {
  return HEX40.test(value);
}

export function hashObject(type: GitObjectType, data: Uint8Array): string {
  const body = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${body.length}\0`, 'utf8'));
  hash.update(body);
  return hash.digest('hex');
}

export function putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

// ---------------------------------------------------------------------------
// trees, commits, tags
// ---------------------------------------------------------------------------

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Uint8Array): TreeEntry[] {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < buffer.length) {
    const space = buffer.indexOf(0x20, offset);
    const nul = buffer.indexOf(0x00, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > buffer.length) throw new Error('malformed tree object');
    entries.push({
      mode: buffer.subarray(offset, space).toString('ascii'),
      name: buffer.subarray(space + 1, nul).toString('utf8'),
      oid: buffer.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Uint8Array): CommitView {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const blank = buffer.indexOf('\n\n');
  const headerEnd = blank < 0 ? buffer.length : blank;
  const message = blank < 0 ? '' : buffer.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of buffer.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

export function buildCommitText(fields: { tree: string; parents: readonly string[]; message: string; timestamp: number }): Buffer {
  const who = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${fields.timestamp} +0000`;
  const lines = [`tree ${fields.tree}`];
  for (const parent of fields.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return Buffer.from(`${lines.join('\n')}\n${fields.message}`, 'utf8');
}

export interface TagView {
  object: string;
  type: GitObjectType;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Uint8Array): TagView {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const blank = buffer.indexOf('\n\n');
  const headerEnd = blank < 0 ? buffer.length : blank;
  const headers = new Map<string, string>();
  for (const line of buffer.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space >= 0) headers.set(line.slice(0, space), line.slice(space + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: (headers.get('type') ?? 'commit') as GitObjectType,
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : buffer.subarray(blank + 2).toString('utf8'),
  };
}

/** Peel an annotated tag to the commit it points at (directly or through tags). */
export function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let hops = 0; hops < 16; hops += 1) {
    const object = repo.objects.get(current);
    if (object === undefined) return null;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return null;
  }
  return null;
}

// ---------------------------------------------------------------------------
// flat file maps (path -> {mode, oid}) and tree building from them
// ---------------------------------------------------------------------------

export interface FlatFile {
  mode: string;
  oid: string;
}

/** The whole tree of `rootOid` as a flat path -> entry map. */
export function flattenTree(repo: Repo, rootOid: string): Map<string, FlatFile> {
  const out = new Map<string, FlatFile>();
  const walk = (treeOid: string, prefix: string): void => {
    const object = repo.objects.get(treeOid);
    if (object === undefined || object.type !== 'tree') return;
    for (const entry of parseTree(object.data)) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, { mode: entry.mode, oid: entry.oid });
    }
  };
  walk(rootOid, '');
  return out;
}

/** Build nested trees for a flat file map; stores every tree object; returns the root tree oid. */
export function buildTreeFromFlat(repo: Repo, files: Map<string, FlatFile>): string {
  interface Node {
    files: Map<string, FlatFile>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, file] of files) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const name = segments[i] as string;
      let child = node.dirs.get(name);
      if (child === undefined) {
        child = make();
        node.dirs.set(name, child);
      }
      node = child;
    }
    node.files.set(segments[segments.length - 1] as string, file);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, file] of node.files) entries.push({ mode: file.mode, name, oid: file.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return putObject(repo, 'tree', buildTree(entries));
  };
  return build(root);
}

/** Diff two flat file maps: changed paths with old/new blob ids. */
export interface FileChange {
  path: string;
  status: 'added' | 'modified' | 'removed';
  oldOid: string | null;
  newOid: string | null;
}

export function diffFlat(oldMap: Map<string, FlatFile>, newMap: Map<string, FlatFile>): FileChange[] {
  const changes: FileChange[] = [];
  const paths = new Set<string>([...oldMap.keys(), ...newMap.keys()]);
  for (const path of paths) {
    const oldEntry = oldMap.get(path);
    const newEntry = newMap.get(path);
    if (oldEntry === undefined && newEntry !== undefined) {
      changes.push({ path, status: 'added', oldOid: null, newOid: newEntry.oid });
    } else if (oldEntry !== undefined && newEntry === undefined) {
      changes.push({ path, status: 'removed', oldOid: oldEntry.oid, newOid: null });
    } else if (oldEntry !== undefined && newEntry !== undefined && oldEntry.oid !== newEntry.oid) {
      changes.push({ path, status: 'modified', oldOid: oldEntry.oid, newOid: newEntry.oid });
    }
  }
  changes.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return changes;
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

export function collectReachable(roots: Iterable<string>, lookup: (oid: string) => GitObject | undefined): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const root of roots) if (isOid(root)) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (object === undefined) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return seen;
}

/** Objects reachable from `roots`, not descending below commits in `stopAt`: their trees and blobs are included, their parents are not. This is the set a shallow client actually holds: it lists a shallow boundary commit as a `have`, but it does not hold that commit's parents. */
export function collectHaveReachable(roots: Iterable<string>, stopAt: readonly string[], lookup: (oid: string) => GitObject | undefined): Set<string> {
  const stop = new Set(stopAt);
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const root of roots) if (isOid(root)) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (object === undefined) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      for (const extra of collectReachable([commit.tree], lookup)) seen.add(extra);
      if (!stop.has(oid)) {
        for (const parent of commit.parents) queue.push(parent);
      }
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return seen;
}
/** True when `ancestor` is reachable from `tip` (or equal to it). */
export function isAncestor(repo: Repo, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) {
      if (parent === ancestor) return true;
      queue.push(parent);
    }
  }
  return false;
}

/** The newest common ancestor of two commits (first-parent linear histories: the nearest shared one). */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const ancestorsA = new Map<string, number>();
  const queueA: Array<[string, number]> = [[a, 0]];
  while (queueA.length > 0) {
    const [oid, depth] = queueA.shift() as [string, number];
    if (ancestorsA.has(oid)) continue;
    ancestorsA.set(oid, depth);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queueA.push([parent, depth + 1]);
  }
  let best: string | null = null;
  let bestDepth = Number.POSITIVE_INFINITY;
  const queueB: Array<[string, number]> = [[b, 0]];
  const seenB = new Set<string>();
  while (queueB.length > 0) {
    const [oid, depth] = queueB.shift() as [string, number];
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    const depthA = ancestorsA.get(oid);
    if (depthA !== undefined && depthA + depth < bestDepth) {
      bestDepth = depthA + depth;
      best = oid;
    }
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queueB.push([parent, depth + 1]);
  }
  return best;
}

// ---------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function readVarintSize(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start] as number;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    byte = buffer[offset] as number;
    offset += 1;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: offset, first };
}

function readOfsBase(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start] as number;
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    byte = buffer[offset] as number;
    offset += 1;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset] as number;
      offset += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset] as number;
    offset += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          copyOffset += (delta[offset] as number) * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          copySize += (delta[offset] as number) * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== targetSize) throw new Error('delta size mismatch');
  return out;
}

/** Parse a packfile; returns every resolved object keyed by id. */
export function readPack(pack: Buffer): Map<string, GitObject> {
  if (pack.length < 32) throw new Error('pack too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack header missing');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  interface Raw {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    rawZlib?: Buffer;
    baseOffset?: number;
    baseOid?: string;
    resolved?: { type: GitObjectType; data: Buffer; oid: string; rawZlib?: Buffer };
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let offset = 12;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = offset;
    const header = readVarintSize(pack, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsBase(pack, offset);
      offset = base.next;
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (CODE_TYPE[typeCode] === undefined) {
      throw new Error(`unknown pack type ${typeCode}`);
    }
    const zlibStart = offset;
    const inflated = inflateSync(pack.subarray(offset, trailerStart), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    if (inflated.buffer.length !== header.size) throw new Error('pack entry size mismatch');
    entry.data = inflated.buffer;
    // Zero-deflate: keep the exact zlib bytes for plain entries, so writePack
    // can reuse them verbatim. Delta entries resolve to new data, so their
    // zlib stream (a delta) must never be reused as a full object.
    if (typeCode !== 6 && typeCode !== 7) entry.rawZlib = pack.subarray(zlibStart, zlibStart + inflated.engine.bytesWritten);
    offset += inflated.engine.bytesWritten;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  const objects = new Map<string, GitObject>();
  const resolvedByOid = new Map<string, Raw>();
  const resolving = new Set<number>();
  const resolve = (entry: Raw): { type: GitObjectType; data: Buffer; oid: string; rawZlib?: Buffer } => {
    if (entry.resolved !== undefined) return entry.resolved;
    if (resolving.has(entry.offset)) throw new Error('delta cycle');
    resolving.add(entry.offset);
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (baseEntry === undefined) throw new Error('ofs-delta base missing');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid as string;
      const baseEntry = resolvedByOid.get(wanted);
      if (baseEntry !== undefined) {
        const base = resolve(baseEntry);
        type = base.type;
        data = applyDelta(base.data, entry.data);
      } else {
        throw new Error('ref-delta base missing');
      }
    } else {
      type = CODE_TYPE[entry.typeCode] as GitObjectType;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    entry.resolved = { type, data, oid, rawZlib: entry.rawZlib };
    resolvedByOid.set(oid, entry);
    resolving.delete(entry.offset);
    return entry.resolved;
  };
  for (const entry of raw) {
    const resolved = resolve(entry);
    if (!objects.has(resolved.oid)) {
      objects.set(resolved.oid, resolved.rawZlib !== undefined
        ? { type: resolved.type, data: resolved.data, rawZlib: resolved.rawZlib }
        : { type: resolved.type, data: resolved.data });
    }
  }
  return objects;
}

function writePackObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas): header, zlib entries, SHA-1 trailer. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    // Level 1: the pack is checksummed and re-inflated by the client, never
    // stored; speed beats size here (fetch is a timed workload). When the
    // object arrived in a pack, its original zlib bytes are reused verbatim
    // instead: zero-deflate, no inflate/deflate round-trip at all.
    parts.push(object.rawZlib !== undefined ? object.rawZlib : deflateSync(object.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

export function pktLine(payload: string | Uint8Array): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : Buffer.from(payload);
  if (data.length + 4 > PKT_MAX) throw new Error('pkt-line too long');
  const header = Buffer.allocUnsafe(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

export const pktFlush = (): Buffer => Buffer.from('0000', 'ascii');
export const pktDelim = (): Buffer => Buffer.from('0001', 'ascii');

export function pktLines(items: readonly (string | Uint8Array | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(pktFlush());
    else if (item === 'delim') parts.push(pktDelim());
    else parts.push(pktLine(item));
  }
  return Buffer.concat(parts);
}

/** Decode a whole buffer into packets (data payloads and control markers). */
export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'response-end' };

export function decodePktLines(bytes: Buffer): Packet[] {
  const packets: Packet[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    if (offset + 4 > bytes.length) throw new Error('truncated pkt-line');
    const header = bytes.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error('invalid pkt-line header');
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      offset += 4;
    } else if (length === 1) {
      packets.push({ kind: 'delim' });
      offset += 4;
    } else if (length === 2) {
      packets.push({ kind: 'response-end' });
      offset += 4;
    } else {
      if (offset + length > bytes.length) throw new Error('truncated pkt-line');
      packets.push({ kind: 'data', payload: bytes.subarray(offset + 4, offset + length) });
      offset += length;
    }
  }
  return packets;
}

/** Split one sideband stream into band-1 chunks (pack data), band 2 is progress. */
export function sidebandWrap(data: Buffer, chunkSize = 32768): Buffer {
  const parts: Buffer[] = [];
  for (let offset = 0; offset < data.length; offset += chunkSize) {
    const chunk = data.subarray(offset, Math.min(data.length, offset + chunkSize));
    const payload = Buffer.concat([Buffer.from([1]), chunk]);
    parts.push(pktLine(payload));
  }
  if (data.length === 0) parts.push(pktLine(Buffer.from([1])));
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// ref helpers
// ---------------------------------------------------------------------------

export function defaultBranchRef(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

export function headTarget(repo: Repo): string | undefined {
  return repo.refs.get(defaultBranchRef(repo));
}

function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

const UPLOAD_V2_CAPS = ['ls-refs=unborn', 'fetch=shallow', 'object-format=sha1'];
const UPLOAD_V0_CAPS = 'symref=HEAD:refs/heads/main side-band-64k shallow thin-pack ofs-delta agent=git-forge/1.0';
const RECEIVE_V0_CAPS = 'report-status side-band-64k delete-refs ofs-delta agent=git-forge/1.0';

/** The v2 capability advertisement: `version 2` then one capability per line. Static: built once. */
export function advertisementV2(): Buffer {
  return ADV_V2;
}

const ADV_V2: Buffer = pktLines([
  'version 2\n',
  ...UPLOAD_V2_CAPS.map((cap) => `${cap}\n`),
  'flush',
]);

/**
 * The v0 ref advertisement, with the `# service=...` preamble. The first ref
 * line carries the capabilities after a NUL; an empty repository advertises
 * the `capabilities^{}` pseudo-ref instead.
 */
export function advertisementV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  // Cache by the full ref fingerprint: any ref move changes the key.
  const fingerprint = [...repo.refs.entries()]
    .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
    .map(([name, oid]) => `${name}=${oid}`)
    .join(';');
  const key = `${service}|${fingerprint}`;
  const cached = repo.advCache.get(key);
  if (cached !== undefined) return cached;
  const built = buildAdvertisementV0(repo, service);
  if (repo.advCache.size > 16) repo.advCache.clear();
  repo.advCache.set(key, built);
  return built;
}

function buildAdvertisementV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_V0_CAPS : RECEIVE_V0_CAPS;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), pktFlush()];
  const refs = sortedRefs(repo);
  const tip = headTarget(repo);
  const lines: string[] = [];
  if (refs.length === 0 || tip === undefined) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    lines.push(`${tip} HEAD\0${caps}\n`);
    for (const [name, oid] of refs) {
      lines.push(`${oid} ${name}\n`);
      const object = repo.objects.get(oid);
      if (object !== undefined && object.type === 'tag') {
        const peeled = peelToCommit(repo, oid);
        if (peeled !== null) lines.push(`${peeled} ${name}^{}\n`);
      }
    }
  }
  parts.push(pktLines(lines.map((line) => line as string | 'flush')));
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// ls-refs (v2)
// ---------------------------------------------------------------------------

export interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

/** Parse an ls-refs request body (pkt-lines after `command=ls-refs`). */
export function parseLsRefsArgs(packets: readonly Packet[]): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  let seenDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      seenDelim = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').trim();
    if (!seenDelim) continue;
    if (line === 'peel') args.peel = true;
    else if (line === 'symrefs') args.symrefs = true;
    else if (line === 'unborn') args.unborn = true;
    else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
  }
  return args;
}

/** ls-refs responses are a pure function of the refs + args, so cache them by ref fingerprint (same invalidation-by-key trick as advertisementV0). */
const LS_REFS_CACHE = new WeakMap<Repo, Map<string, Buffer>>();

export function lsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const fingerprint = [...repo.refs.entries()]
    .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
    .map(([name, oid]) => `${name}=${oid}`)
    .join(';');
  const key = `${fingerprint}|${args.peel ? 1 : 0}|${args.symrefs ? 1 : 0}|${args.unborn ? 1 : 0}|${args.prefixes.join(',')}`;
  let cache = LS_REFS_CACHE.get(repo);
  if (cache === undefined) {
    cache = new Map<string, Buffer>();
    LS_REFS_CACHE.set(repo, cache);
  }
  const cached = cache.get(key);
  if (cached !== undefined) return cached;
  const built = buildLsRefsResponse(repo, args);
  if (cache.size > 32) cache.clear();
  cache.set(key, built);
  return built;
}

function buildLsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const lines: string[] = [];
  const tip = headTarget(repo);
  const refs = sortedRefs(repo);
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  if (refs.length === 0) {
    if (args.unborn) lines.push(`unborn HEAD symref-target:${defaultBranchRef(repo)}\n`);
  } else {
    if (tip !== undefined && keep('HEAD') && args.prefixes.length === 0) {
      lines.push(args.symrefs ? `${tip} HEAD symref-target:${defaultBranchRef(repo)}\n` : `${tip} HEAD\n`);
    }
    for (const [name, oid] of refs) {
      if (!keep(name)) continue;
      let line = `${oid} ${name}`;
      const object = repo.objects.get(oid);
      if (args.peel && object !== undefined && object.type === 'tag') {
        const peeled = peelToCommit(repo, oid);
        if (peeled !== null) line += ` peeled:${peeled}`;
      }
      lines.push(`${line}\n`);
    }
  }
  return pktLines([...lines, 'flush']);
}

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

export interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  deepenSince: number | null;
  deepenNot: string[];
}

/** Parse a v2 fetch request body. */
export function parseFetchV2Args(packets: readonly Packet[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, deepenSince: null, deepenNot: [] };
  let seenDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      seenDelim = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').trim();
    if (!seenDelim) continue;
    if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) args.depth = Number.parseInt(line.slice(7).trim(), 10);
    else if (line.startsWith('deepen-since ')) args.deepenSince = Number.parseInt(line.slice(13).trim(), 10);
    else if (line.startsWith('deepen-not ')) args.deepenNot.push(line.slice(11).trim());
  }
  return args;
}

/** Parse a v0 fetch request body: wants before the first flush, haves after it. */
export function parseFetchV0Args(packets: readonly Packet[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, deepenSince: null, deepenNot: [] };
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').trim();
    if (section === 0) {
      if (line.startsWith('want ')) args.wants.push(line.slice(5).split(' ')[0] as string);
      else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) args.depth = Number.parseInt(line.slice(7).trim(), 10);
      else if (line.startsWith('deepen-since ')) args.deepenSince = Number.parseInt(line.slice(13).trim(), 10);
      else if (line.startsWith('deepen-not ')) args.deepenNot.push(line.slice(11).trim());
    } else {
      if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    }
  }
  return args;
}

export interface FetchPlan {
  /** Objects to send, in pack order. */
  objects: GitObject[];
  /** The new shallow boundary commits (depth-limited cut). */
  shallow: string[];
  /** Previously shallow commits that are now fully included. */
  unshallow: string[];
}

/**
 * Plan a fetch: the objects reachable from `wants` within `depth` generations
 * (unlimited when depth is null), minus anything reachable from `haves`.
 * A commit at exactly the depth limit is a shallow boundary; a previously
 * shallow commit now included below the limit is unshallow.
 */
export function planFetch(repo: Repo, args: FetchArgs): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  // What the client already holds: reachability from its haves, stopping at
  // its shallow boundaries (it does not hold what lies below them).
  const haveReachable = collectHaveReachable(args.haves, args.shallow, lookup);
  const depth = args.depth;
  const included = new Set<string>();
  const shallow = new Set<string>();
  const boundary = new Set<string>();
  if (depth === null) {
    for (const oid of collectReachable(args.wants, lookup)) included.add(oid);
  } else {
    // BFS over commits by generation; trees/blobs of included commits are whole.
    const queue: Array<[string, number]> = args.wants.filter(isOid).map((oid) => [oid, 1] as [string, number]);
    const seenCommits = new Set<string>();
    while (queue.length > 0) {
      const [oid, generation] = queue.shift() as [string, number];
      if (seenCommits.has(oid)) continue;
      seenCommits.add(oid);
      const object = repo.objects.get(oid);
      if (object === undefined) continue;
      if (object.type === 'tag') {
        const target = parseTag(object.data).object;
        included.add(oid);
        if (isOid(target)) queue.push([target, generation]);
        continue;
      }
      if (object.type !== 'commit') {
        included.add(oid);
        continue;
      }
      included.add(oid);
      const commit = parseCommit(object.data);
      for (const extra of collectReachable([commit.tree], lookup)) included.add(extra);
      if (generation >= depth) {
        boundary.add(oid);
      } else {
        for (const parent of commit.parents) queue.push([parent, generation + 1]);
      }
    }
    for (const oid of boundary) shallow.add(oid);
  }
  // Never re-send what the client already holds.
  for (const oid of haveReachable) included.delete(oid);
  const unshallow: string[] = [];
  for (const oid of args.shallow) {
    if (!shallow.has(oid) && included.has(oid)) unshallow.push(oid);
  }
  const objects: GitObject[] = [];
  for (const oid of included) {
    const object = repo.objects.get(oid);
    if (object !== undefined) objects.push(object);
  }
  objects.sort((a, b) => {
    const order = (type: GitObjectType): number => (type === 'commit' ? 0 : type === 'tree' ? 1 : type === 'tag' ? 2 : 3);
    return order(a.type) - order(b.type);
  });
  return { objects, shallow: [...shallow].sort(), unshallow: unshallow.sort() };
}

/**
 * Fetch cache key. The object database is append-only, so (head tip, object
 * count) plus the request args fully determine the response bytes: any push,
 * ref move or new object changes the key. No wall clock or per-request data
 * enters a fetch response, so a cached body is byte-identical to a fresh one.
 */
function fetchCacheKey(repo: Repo, protocol: 'v0' | 'v2', args: FetchArgs): string {
  const tip = repo.refs.get(defaultBranchRef(repo)) ?? '';
  return [
    protocol,
    repo.id,
    tip,
    repo.objects.size,
    args.wants.join(','),
    args.haves.join(','),
    args.shallow.join(','),
    args.depth === null ? '' : String(args.depth),
    args.deepenSince === null ? '' : String(args.deepenSince),
    args.deepenNot.join(','),
  ].join('|');
}

/** Store a fetch response unless it is huge; keep the cache bounded. */
function cacheFetchResponse(repo: Repo, key: string, body: Buffer): void {
  if (body.length > 2 * 1024 * 1024) return;
  if (repo.fetchCache.size > 64) repo.fetchCache.clear();
  repo.fetchCache.set(key, body);
}

/** The v2 fetch response: acknowledgments, shallow-info, packfile sections. */
export function fetchV2Response(repo: Repo, args: FetchArgs): Buffer {
  const key = fetchCacheKey(repo, 'v2', args);
  const cached = repo.fetchCache.get(key);
  if (cached !== undefined) return cached;
  const built = buildFetchV2Response(repo, args);
  cacheFetchResponse(repo, key, built);
  return built;
}

function buildFetchV2Response(repo: Repo, args: FetchArgs): Buffer {
  const plan = planFetch(repo, args);
  const parts: Buffer[] = [];
  parts.push(pktLines(['acknowledgments\n', 'NAK\n', 'delim']));
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const lines: Array<string | 'delim'> = ['shallow-info\n'];
    for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
    lines.push('delim');
    parts.push(pktLines(lines));
  }
  if (plan.objects.length > 0) {
    const pack = writePack(plan.objects);
    parts.push(pktLine('packfile\n'));
    parts.push(sidebandWrap(pack));
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

/** The v0 fetch response: NAK, shallow lines, then the sideband pack. */
export function fetchV0Response(repo: Repo, args: FetchArgs): Buffer {
  const key = fetchCacheKey(repo, 'v0', args);
  const cached = repo.fetchCache.get(key);
  if (cached !== undefined) return cached;
  const built = buildFetchV0Response(repo, args);
  cacheFetchResponse(repo, key, built);
  return built;
}

function buildFetchV0Response(repo: Repo, args: FetchArgs): Buffer {
  const plan = planFetch(repo, args);
  const lines: Array<string | 'flush'> = ['NAK\n'];
  for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
  for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
  const parts: Buffer[] = [pktLines(lines)];
  if (plan.objects.length > 0) {
    parts.push(sidebandWrap(writePack(plan.objects)));
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export interface PushReportLine {
  ref: string;
  ok: boolean;
  reason: string;
}

/** Parse a receive-pack body: command pkt-lines, a flush, then the raw pack. */
export function parseReceivePack(body: Buffer): { commands: PushCommand[]; pack: Buffer; caps: string[] } {
  const commands: PushCommand[] = [];
  let caps: string[] = [];
  let offset = 0;
  let sawFlush = false;
  while (offset < body.length) {
    if (offset + 4 > body.length) throw new Error('truncated pkt-line in push');
    const header = body.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) {
      // The pack follows raw; the flush already ended the command list.
      break;
    }
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      offset += 4;
      sawFlush = true;
      break;
    }
    if (length === 1 || length === 2) {
      offset += 4;
      continue;
    }
    if (offset + length > body.length) throw new Error('truncated pkt-line in push');
    const line = body.subarray(offset + 4, offset + length).toString('utf8');
    offset += length;
    const nul = line.indexOf('\0');
    let text = line;
    if (nul >= 0) {
      caps = line.slice(nul + 1).trim().split(/\s+/).filter((token) => token !== '');
      text = line.slice(0, nul);
    }
    const parts = text.trim().split(' ');
    if (parts.length >= 3) {
      commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts.slice(2).join(' ') });
    }
  }
  if (!sawFlush && commands.length === 0) throw new Error('push has no command list');
  return { commands, pack: body.subarray(offset), caps };
}

/**
 * Apply a push: store the pack's objects, then decide each ref update.
 * A ref moves only on a fast-forward (or a create/delete with the right
 * old value); anything else is refused per ref with `non-fast-forward`.
 */
export function applyPush(repo: Repo, store: Store, commands: readonly PushCommand[], packObjects: Map<string, GitObject>): { report: PushReportLine[]; unpackOk: boolean; unpackDetail: string } {
  for (const [oid, object] of packObjects) {
    if (!repo.objects.has(oid)) repo.objects.set(oid, object);
  }
  const report: PushReportLine[] = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      } else {
        repo.refs.delete(command.ref);
        report.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    const target = repo.objects.get(command.newOid);
    if (target === undefined) {
      report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (current === undefined) {
      if (command.oldOid !== ZERO_OID) {
        report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      report.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (command.oldOid !== current) {
      report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (command.newOid === current || isAncestor(repo, current, command.newOid)) {
      repo.refs.set(command.ref, command.newOid);
      report.push({ ref: command.ref, ok: true, reason: '' });
    } else {
      report.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
    }
  }
  void store;
  return { report, unpackOk: true, unpackDetail: 'ok' };
}

/** The report-status stream, framed in sideband band 1 when asked for. */
export function pushReportResponse(report: readonly PushReportLine[], sideband: boolean): Buffer {
  const lines: Array<string | 'flush'> = ['unpack ok\n'];
  for (const entry of report) {
    lines.push(entry.ok ? `ok ${entry.ref}\n` : `ng ${entry.ref} ${entry.reason}\n`);
  }
  lines.push('flush');
  const stream = pktLines(lines);
  if (!sideband) return Buffer.concat([stream, pktFlush()]);
  return Buffer.concat([sidebandWrap(stream), pktFlush()]);
}

/** Create a commit object for a server-side change (contents, merge). The message is stored verbatim, as the clients build it. */
export function makeServerCommit(store: Store, repo: Repo, tree: string, parents: readonly string[], message: string): string {
  const timestamp = store.tick();
  const data = buildCommitText({ tree, parents, message, timestamp });
  return putObject(repo, 'commit', data);
}

/** The deterministic timestamp the store would use for a new commit (for tests). */
export function nextEpochSec(store: Store): number {
  return EPOCH_SEC + store.seq + 1;
}
