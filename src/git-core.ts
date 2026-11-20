import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly oid: string;
  readonly type: GitObjectType;
  readonly data: Buffer;
  deflated?: Buffer;
}

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export interface CommitView {
  readonly tree: string;
  readonly parents: readonly string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

export interface TagView {
  readonly object: string;
  readonly type: GitObjectType;
  readonly tag: string;
  readonly tagger: string;
  readonly message: string;
}

export interface PathBlobEntry {
  readonly mode: string;
  readonly oid: string;
}

export interface FileDiffItem {
  readonly filename: string;
  readonly status: 'added' | 'modified' | 'removed';
  readonly additions: number;
  readonly deletions: number;
  readonly changes: number;
}

export type PktPacket =
  | { readonly kind: 'flush' }
  | { readonly kind: 'delim' }
  | { readonly kind: 'response-end' }
  | { readonly kind: 'data'; readonly payload: Buffer };

export class MalformedPktLineError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'MalformedPktLineError';
  }
}

export class PackFormatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PackFormatError';
  }
}

export const ZERO_OID = '0'.repeat(40);
export const OID_REGEX = /^[0-9a-f]{40}$/;
export const PKT_LINE_MAX = 65_520;
export const SIDEBAND_CHUNK_MAX = 65_515;

const TYPE_TO_CODE: Readonly<Record<GitObjectType, number>> = {
  commit: 1,
  tree: 2,
  blob: 3,
  tag: 4,
};

const CODE_TO_TYPE: Readonly<Record<number, GitObjectType>> = {
  1: 'commit',
  2: 'tree',
  3: 'blob',
  4: 'tag',
};

export function hashGitObject(type: GitObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\u0000`, 'utf8');
  return createHash('sha1').update(header).update(data).digest('hex');
}

export function createGitObject(type: GitObjectType, data: Buffer, deflated?: Buffer): GitObject {
  return {
    oid: hashGitObject(type, data),
    type,
    data,
    deflated,
  };
}

function treeSortKey(entry: TreeEntry): Buffer {
  return Buffer.from(entry.mode === '40000' ? `${entry.name}/` : entry.name, 'utf8');
}

export function compareTreeEntries(a: TreeEntry, b: TreeEntry): number {
  return Buffer.compare(treeSortKey(a), treeSortKey(b));
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const mode = data.subarray(offset, space).toString('ascii');
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeEntries);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\u0000`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return parts.length > 0 ? Buffer.concat(parts) : Buffer.alloc(0);
}

export function parseCommit(data: Buffer): CommitView {
  const separator = data.indexOf('\n\n');
  const headerBuf = separator < 0 ? data : data.subarray(0, separator);
  const message = separator < 0 ? '' : data.subarray(separator + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of headerBuf.toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space < 0) continue;
    const key = line.slice(0, space);
    const val = line.slice(space + 1);
    if (key === 'tree') tree = val;
    else if (key === 'parent') parents.push(val);
    else if (key === 'author') author = val;
    else if (key === 'committer') committer = val;
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(fields: {
  tree: string;
  parents: readonly string[];
  author: string;
  committer: string;
  message: string;
}): Buffer {
  const lines = [`tree ${fields.tree}`];
  for (const parent of fields.parents) {
    lines.push(`parent ${parent}`);
  }
  lines.push(`author ${fields.author}`, `committer ${fields.committer}`, '');
  return Buffer.from(`${lines.join('\n')}\n${fields.message}`, 'utf8');
}

export function parseTag(data: Buffer): TagView {
  const separator = data.indexOf('\n\n');
  const headerBuf = separator < 0 ? data : data.subarray(0, separator);
  const message = separator < 0 ? '' : data.subarray(separator + 2).toString('utf8');
  let object = '';
  let type: GitObjectType = 'commit';
  let tag = '';
  let tagger = '';
  for (const line of headerBuf.toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space < 0) continue;
    const key = line.slice(0, space);
    const val = line.slice(space + 1);
    if (key === 'object') object = val;
    else if (key === 'type' && (val === 'commit' || val === 'tree' || val === 'blob' || val === 'tag')) type = val;
    else if (key === 'tag') tag = val;
    else if (key === 'tagger') tagger = val;
  }
  return { object, type, tag, tagger, message };
}

export function peelOid(objects: ReadonlyMap<string, GitObject>, startOid: string): string {
  let current = startOid;
  for (let depth = 0; depth < 16; depth += 1) {
    const obj = objects.get(current);
    if (!obj || obj.type !== 'tag') return current;
    current = parseTag(obj.data).object;
  }
  return current;
}

export function resolveTreeOid(objects: ReadonlyMap<string, GitObject>, commitOrTreeOid: string): string | null {
  const peeled = peelOid(objects, commitOrTreeOid);
  const obj = objects.get(peeled);
  if (!obj) return null;
  if (obj.type === 'tree') return obj.oid;
  if (obj.type === 'commit') return parseCommit(obj.data).tree;
  return null;
}

export function flattenTree(objects: ReadonlyMap<string, GitObject>, commitOrTreeOid: string): Map<string, PathBlobEntry> {
  const result = new Map<string, PathBlobEntry>();
  const rootTree = resolveTreeOid(objects, commitOrTreeOid);
  if (!rootTree) return result;

  const walk = (treeOid: string, prefix: string): void => {
    const treeObj = objects.get(treeOid);
    if (!treeObj || treeObj.type !== 'tree') return;
    for (const entry of parseTree(treeObj.data)) {
      const fullPath = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.mode === '40000') {
        walk(entry.oid, fullPath);
      } else if (entry.mode !== '160000') {
        result.set(fullPath, { mode: entry.mode, oid: entry.oid });
      }
    }
  };
  walk(rootTree, '');
  return result;
}

interface TreeDirNode {
  readonly files: Map<string, { mode: string; oid: string }>;
  readonly dirs: Map<string, TreeDirNode>;
}

function makeDirNode(): TreeDirNode {
  return { files: new Map(), dirs: new Map() };
}

export function buildTreeFromPaths(
  saveObject: (obj: GitObject) => GitObject,
  pathMap: ReadonlyMap<string, PathBlobEntry>,
): string {
  const root = makeDirNode();
  for (const [rawPath, entry] of pathMap.entries()) {
    const parts = rawPath.split('/').filter(Boolean);
    if (parts.length === 0) continue;
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const segment = parts[i]!;
      let child = node.dirs.get(segment);
      if (!child) {
        child = makeDirNode();
        node.dirs.set(segment, child);
      }
      node = child;
    }
    node.files.set(parts[parts.length - 1]!, entry);
  }

  const writeNode = (node: TreeDirNode): string => {
    const entries: TreeEntry[] = [];
    for (const [name, file] of node.files.entries()) {
      entries.push({ mode: file.mode, name, oid: file.oid });
    }
    for (const [name, child] of node.dirs.entries()) {
      const childOid = writeNode(child);
      entries.push({ mode: '40000', name, oid: childOid });
    }
    const obj = saveObject(createGitObject('tree', buildTree(entries)));
    return obj.oid;
  };

  return writeNode(root);
}

export function isAncestor(
  getParents: (oid: string) => readonly string[] | undefined,
  ancestorOid: string,
  descendantOid: string,
): boolean {
  if (ancestorOid === descendantOid) return true;
  const direct = getParents(descendantOid);
  if (direct && direct.includes(ancestorOid)) return true;

  const visited = new Set<string>([descendantOid]);
  const queue: string[] = [descendantOid];
  while (queue.length > 0) {
    const cur = queue.pop()!;
    const parents = getParents(cur);
    if (!parents) continue;
    for (const parent of parents) {
      if (parent === ancestorOid) return true;
      if (!visited.has(parent)) {
        visited.add(parent);
        queue.push(parent);
      }
    }
  }
  return false;
}

export function findMergeBase(
  getParents: (oid: string) => readonly string[] | undefined,
  oidA: string,
  oidB: string,
): string | null {
  if (oidA === oidB) return oidA;
  const ancA = new Set<string>([oidA]);
  const qA: string[] = [oidA];
  while (qA.length > 0) {
    const cur = qA.shift()!;
    const parents = getParents(cur);
    if (!parents) continue;
    for (const parent of parents) {
      if (!ancA.has(parent)) {
        ancA.add(parent);
        qA.push(parent);
      }
    }
  }

  if (ancA.has(oidB)) return oidB;
  const visitedB = new Set<string>([oidB]);
  const qB: string[] = [oidB];
  while (qB.length > 0) {
    const cur = qB.shift()!;
    if (ancA.has(cur)) return cur;
    const parents = getParents(cur);
    if (!parents) continue;
    for (const parent of parents) {
      if (!visitedB.has(parent)) {
        visitedB.add(parent);
        qB.push(parent);
      }
    }
  }
  return null;
}

function sameEntry(a: PathBlobEntry | undefined, b: PathBlobEntry | undefined): boolean {
  if (a === undefined && b === undefined) return true;
  if (a === undefined || b === undefined) return false;
  return a.oid === b.oid && a.mode === b.mode;
}

export function threeWayMergeTrees(
  saveObject: (obj: GitObject) => GitObject,
  ancestorMap: ReadonlyMap<string, PathBlobEntry>,
  baseMap: ReadonlyMap<string, PathBlobEntry>,
  headMap: ReadonlyMap<string, PathBlobEntry>,
): { ok: true; treeOid: string } | { ok: false; conflicts: string[] } {
  const allPaths = new Set<string>([...ancestorMap.keys(), ...baseMap.keys(), ...headMap.keys()]);
  const merged = new Map<string, PathBlobEntry>();
  const conflicts: string[] = [];

  for (const path of allPaths) {
    const anc = ancestorMap.get(path);
    const base = baseMap.get(path);
    const head = headMap.get(path);
    if (sameEntry(base, head)) {
      if (base !== undefined) merged.set(path, base);
    } else if (sameEntry(base, anc)) {
      if (head !== undefined) merged.set(path, head);
    } else if (sameEntry(head, anc)) {
      if (base !== undefined) merged.set(path, base);
    } else {
      conflicts.push(path);
    }
  }

  if (conflicts.length > 0) {
    conflicts.sort();
    return { ok: false, conflicts };
  }
  return { ok: true, treeOid: buildTreeFromPaths(saveObject, merged) };
}

function splitLines(buf: Buffer | undefined): string[] {
  if (!buf || buf.length === 0) return [];
  const text = buf.toString('utf8');
  const raw = text.split('\n');
  if (raw.length > 0 && raw[raw.length - 1] === '') raw.pop();
  return raw;
}

function countLineDelta(baseBuf: Buffer | undefined, headBuf: Buffer | undefined): { additions: number; deletions: number } {
  const baseLines = splitLines(baseBuf);
  const headLines = splitLines(headBuf);
  let start = 0;
  const minLen = Math.min(baseLines.length, headLines.length);
  while (start < minLen && baseLines[start] === headLines[start]) {
    start += 1;
  }
  let baseEnd = baseLines.length;
  let headEnd = headLines.length;
  while (baseEnd > start && headEnd > start && baseLines[baseEnd - 1] === headLines[headEnd - 1]) {
    baseEnd -= 1;
    headEnd -= 1;
  }
  const deletions = baseEnd - start;
  const additions = headEnd - start;
  if (additions === 0 && deletions === 0 && (baseBuf?.length ?? 0) !== (headBuf?.length ?? 0)) {
    return { additions: 1, deletions: 1 };
  }
  return { additions, deletions };
}

export function diffTrees(
  objects: ReadonlyMap<string, GitObject>,
  baseMap: ReadonlyMap<string, PathBlobEntry>,
  headMap: ReadonlyMap<string, PathBlobEntry>,
): FileDiffItem[] {
  const allPaths = [...new Set<string>([...baseMap.keys(), ...headMap.keys()])].sort();
  const diffs: FileDiffItem[] = [];

  for (const filename of allPaths) {
    const baseEntry = baseMap.get(filename);
    const headEntry = headMap.get(filename);
    if (sameEntry(baseEntry, headEntry)) continue;

    if (!baseEntry && headEntry) {
      const headLines = splitLines(objects.get(headEntry.oid)?.data);
      const additions = Math.max(1, headLines.length);
      diffs.push({ filename, status: 'added', additions, deletions: 0, changes: additions });
    } else if (baseEntry && !headEntry) {
      const baseLines = splitLines(objects.get(baseEntry.oid)?.data);
      const deletions = Math.max(1, baseLines.length);
      diffs.push({ filename, status: 'removed', additions: 0, deletions, changes: deletions });
    } else if (baseEntry && headEntry) {
      const baseData = objects.get(baseEntry.oid)?.data;
      const headData = objects.get(headEntry.oid)?.data;
      const { additions, deletions } = countLineDelta(baseData, headData);
      const safeAdd = additions === 0 && deletions === 0 ? 1 : additions;
      diffs.push({
        filename,
        status: 'modified',
        additions: safeAdd,
        deletions,
        changes: safeAdd + deletions,
      });
    }
  }
  return diffs;
}

export function encodePktLine(payload: string | Uint8Array): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : Buffer.from(payload);
  const totalLen = data.length + 4;
  if (totalLen > PKT_LINE_MAX) {
    throw new MalformedPktLineError(`pkt-line payload exceeds limit (${totalLen})`);
  }
  const out = Buffer.allocUnsafe(totalLen);
  out.write(totalLen.toString(16).padStart(4, '0'), 0, 4, 'ascii');
  data.copy(out, 4);
  return out;
}

export const FLUSH_PKT = Buffer.from('0000', 'ascii');
export const DELIM_PKT = Buffer.from('0001', 'ascii');
export const RESPONSE_END_PKT = Buffer.from('0002', 'ascii');

export function encodePktLines(items: readonly (string | Buffer | 'flush' | 'delim' | 'response-end')[]): Buffer {
  const chunks: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') chunks.push(FLUSH_PKT);
    else if (item === 'delim') chunks.push(DELIM_PKT);
    else if (item === 'response-end') chunks.push(RESPONSE_END_PKT);
    else chunks.push(encodePktLine(item));
  }
  return Buffer.concat(chunks);
}

export function encodeSideband(band: 1 | 2 | 3, data: Buffer): Buffer {
  if (data.length === 0) return Buffer.alloc(0);
  const chunks: Buffer[] = [];
  for (let offset = 0; offset < data.length; offset += SIDEBAND_CHUNK_MAX) {
    const slice = data.subarray(offset, Math.min(offset + SIDEBAND_CHUNK_MAX, data.length));
    const totalLen = 4 + 1 + slice.length;
    const pkt = Buffer.allocUnsafe(totalLen);
    pkt.write(totalLen.toString(16).padStart(4, '0'), 0, 4, 'ascii');
    pkt[4] = band;
    slice.copy(pkt, 5);
    chunks.push(pkt);
  }
  return Buffer.concat(chunks);
}

function parseHex4(buf: Buffer, offset: number): number {
  let value = 0;
  for (let i = 0; i < 4; i += 1) {
    const code = buf[offset + i]!;
    let digit = -1;
    if (code >= 0x30 && code <= 0x39) digit = code - 0x30;
    else if (code >= 0x61 && code <= 0x66) digit = code - 0x61 + 10;
    else if (code >= 0x41 && code <= 0x46) digit = code - 0x41 + 10;
    if (digit < 0) {
      throw new MalformedPktLineError(`invalid pkt-line length hex at ${offset}`);
    }
    value = (value << 4) | digit;
  }
  return value;
}

export function decodePktLines(
  buf: Buffer,
  options: { stopAtFlush?: boolean } = {},
): { packets: PktPacket[]; consumed: number } {
  const packets: PktPacket[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) {
      throw new MalformedPktLineError('truncated pkt-line header');
    }
    const len = parseHex4(buf, offset);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      offset += 4;
      if (options.stopAtFlush) break;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      offset += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'response-end' });
      offset += 4;
      continue;
    }
    if (len < 4 || len > PKT_LINE_MAX) {
      throw new MalformedPktLineError(`invalid pkt-line length ${len}`);
    }
    if (offset + len > buf.length) {
      throw new MalformedPktLineError(`truncated pkt-line payload at ${offset}`);
    }
    packets.push({ kind: 'data', payload: buf.subarray(offset + 4, offset + len) });
    offset += len;
  }
  return { packets, consumed: offset };
}

function readPackEntryHeader(buf: Buffer, start: number): { typeCode: number; size: number; next: number } {
  if (start >= buf.length) throw new PackFormatError('truncated pack entry header');
  let byte = buf[start]!;
  const typeCode = (byte >> 4) & 0x07;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    if (offset >= buf.length) throw new PackFormatError('truncated pack size varint');
    byte = buf[offset++]!;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if (shift > 56) throw new PackFormatError('oversized pack varint');
  }
  return { typeCode, size, next: offset };
}

function readOfsDistance(buf: Buffer, start: number): { distance: number; next: number } {
  if (start >= buf.length) throw new PackFormatError('missing ofs-delta offset');
  let byte = buf[start]!;
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    if (offset >= buf.length) throw new PackFormatError('truncated ofs-delta offset');
    byte = buf[offset++]!;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { distance: value, next: offset };
}

function readDeltaVarint(buf: Buffer, state: { offset: number }): number {
  let value = 0;
  let shift = 0;
  for (;;) {
    if (state.offset >= buf.length) throw new PackFormatError('truncated delta varint');
    const byte = buf[state.offset++]!;
    value += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if ((byte & 0x80) === 0) break;
    if (shift > 56) throw new PackFormatError('oversized delta varint');
  }
  return value;
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  const state = { offset: 0 };
  const sourceSize = readDeltaVarint(delta, state);
  const targetSize = readDeltaVarint(delta, state);
  if (sourceSize !== base.length) {
    throw new PackFormatError(`delta source length mismatch (${sourceSize} != ${base.length})`);
  }
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;

  while (state.offset < delta.length) {
    const op = delta[state.offset++]!;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          if (state.offset >= delta.length) throw new PackFormatError('truncated delta copy offset');
          copyOffset |= delta[state.offset++]! << (bit * 8);
        }
      }
      for (let bit = 0; bit < 3; bit += 1) {
        if (op & (1 << (bit + 4))) {
          if (state.offset >= delta.length) throw new PackFormatError('truncated delta copy size');
          copySize |= delta[state.offset++]! << (bit * 8);
        }
      }
      if (copySize === 0) copySize = 0x10000;
      copyOffset >>>= 0;
      if (copyOffset + copySize > base.length || written + copySize > targetSize) {
        throw new PackFormatError('delta copy out of range');
      }
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const insertLen = op & 0x7f;
      if (state.offset + insertLen > delta.length || written + insertLen > targetSize) {
        throw new PackFormatError('delta insert out of range');
      }
      delta.copy(out, written, state.offset, state.offset + insertLen);
      state.offset += insertLen;
      written += insertLen;
    } else {
      throw new PackFormatError('invalid delta opcode 0');
    }
  }
  if (written !== targetSize) {
    throw new PackFormatError(`delta size mismatch (${written} != ${targetSize})`);
  }
  return out;
}

export function unpackPackfile(
  packBuf: Buffer,
  lookupExisting: (oid: string) => GitObject | undefined,
): GitObject[] {
  if (packBuf.length < 32) throw new PackFormatError('packfile shorter than header + trailer');
  if (packBuf.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new PackFormatError('packfile missing PACK signature');
  }
  const version = packBuf.readUInt32BE(4);
  if (version !== 2 && version !== 3) {
    throw new PackFormatError(`unsupported pack version ${version}`);
  }
  const objectCount = packBuf.readUInt32BE(8);
  const trailerStart = packBuf.length - 20;
  const actualChecksum = packBuf.subarray(trailerStart);
  const expectedChecksum = createHash('sha1').update(packBuf.subarray(0, trailerStart)).digest();
  if (!actualChecksum.equals(expectedChecksum)) {
    throw new PackFormatError('packfile checksum mismatch');
  }

  interface ParsedEntry {
    offset: number;
    typeCode: number;
    data: Buffer;
    deflated?: Buffer;
    baseOffset?: number;
    baseOid?: string;
    resolved?: GitObject;
  }

  const entries: ParsedEntry[] = [];
  const byOffset = new Map<number, ParsedEntry>();
  let offset = 12;

  for (let i = 0; i < objectCount; i += 1) {
    const entryOffset = offset;
    const header = readPackEntryHeader(packBuf, offset);
    offset = header.next;
    const entry: ParsedEntry = {
      offset: entryOffset,
      typeCode: header.typeCode,
      data: Buffer.alloc(0),
    };
    if (header.typeCode === 6) {
      const ofs = readOfsDistance(packBuf, offset);
      offset = ofs.next;
      const baseOff = entryOffset - ofs.distance;
      if (baseOff < 12 || !byOffset.has(baseOff)) {
        throw new PackFormatError(`invalid ofs-delta base offset ${baseOff}`);
      }
      entry.baseOffset = baseOff;
    } else if (header.typeCode === 7) {
      if (offset + 20 > trailerStart) throw new PackFormatError('truncated ref-delta oid');
      entry.baseOid = packBuf.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (!CODE_TO_TYPE[header.typeCode]) {
      throw new PackFormatError(`unknown pack type code ${header.typeCode}`);
    }

    const zStart = offset;
    let inf: { buffer: Buffer; engine: { bytesWritten: number } };
    try {
      inf = inflateSync(packBuf.subarray(offset, trailerStart), { info: true }) as unknown as {
        buffer: Buffer;
        engine: { bytesWritten: number };
      };
    } catch (err) {
      throw new PackFormatError(`zlib inflate failed at ${offset}: ${(err as Error).message}`);
    }
    const consumed = inf.engine.bytesWritten;
    if (!consumed || consumed <= 0) throw new PackFormatError('zlib stream consumed zero bytes');
    if (inf.buffer.length !== header.size) {
      throw new PackFormatError(`inflated size ${inf.buffer.length} != header size ${header.size}`);
    }
    entry.data = inf.buffer;
    if (header.typeCode !== 6 && header.typeCode !== 7) {
      entry.deflated = Buffer.from(packBuf.subarray(zStart, zStart + consumed));
    }
    offset += consumed;
    entries.push(entry);
    byOffset.set(entryOffset, entry);
  }

  if (offset !== trailerStart) {
    throw new PackFormatError(`trailing bytes in packfile (${trailerStart - offset})`);
  }

  const byOid = new Map<string, GitObject>();
  const resolving = new Set<number>();

  const resolve = (entry: ParsedEntry): GitObject => {
    if (entry.resolved) return entry.resolved;
    if (resolving.has(entry.offset)) throw new PackFormatError('delta cycle detected');
    resolving.add(entry.offset);

    let obj: GitObject;
    if (entry.typeCode === 6) {
      const base = resolve(byOffset.get(entry.baseOffset!)!);
      obj = createGitObject(base.type, applyDelta(base.data, entry.data));
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid!;
      const baseObj = byOid.get(wanted) ?? lookupExisting(wanted);
      if (!baseObj) throw new PackFormatError(`missing ref-delta base ${wanted}`);
      obj = createGitObject(baseObj.type, applyDelta(baseObj.data, entry.data));
    } else {
      const objType = CODE_TO_TYPE[entry.typeCode]!;
      obj = createGitObject(objType, entry.data, entry.deflated);
    }

    resolving.delete(entry.offset);
    entry.resolved = obj;
    byOid.set(obj.oid, obj);
    return obj;
  };

  const out: GitObject[] = [];
  for (const entry of entries) {
    out.push(resolve(entry));
  }
  return out;
}

function encodePackObjectHeader(typeCode: number, size: number): Buffer {
  const bytes: number[] = [];
  let remaining = size;
  let first = (typeCode << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  if (remaining > 0) first |= 0x80;
  bytes.push(first);
  while (remaining > 0) {
    let next = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
    if (remaining > 0) next |= 0x80;
    bytes.push(next);
  }
  return Buffer.from(bytes);
}

export function buildPackfile(objects: readonly GitObject[]): Buffer {
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length >>> 0, 8);

  const chunks: Buffer[] = [header];
  for (const obj of objects) {
    const code = TYPE_TO_CODE[obj.type];
    chunks.push(encodePackObjectHeader(code, obj.data.length));
    if (!obj.deflated) {
      obj.deflated = deflateSync(obj.data, { level: 1 });
    }
    chunks.push(obj.deflated);
  }

  const body = Buffer.concat(chunks);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

export const EMPTY_PACKFILE = buildPackfile([]);
