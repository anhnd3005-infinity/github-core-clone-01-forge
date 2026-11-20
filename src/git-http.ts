import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  EMPTY_PACKFILE,
  FLUSH_PKT,
  MalformedPktLineError,
  OID_REGEX,
  PackFormatError,
  ZERO_OID,
  decodePktLines,
  encodePktLines,
  encodeSideband,
  buildPackfile,
  isAncestor,
  parseCommit,
  parseTag,
  parseTree,
  peelOid,
  unpackPackfile,
  type GitObject,
} from './git-core.ts';
import type { ForgeStore, RepoRecord } from './store.ts';

const V2_UPLOAD_ADVERTISEMENT = encodePktLines([
  'version 2\n',
  'agent=makefun-forge/1.0\n',
  'ls-refs=unborn\n',
  'fetch=shallow\n',
  'object-format=sha1\n',
  'flush',
]);

function sendGitError(res: ServerResponse, status: number, message: string, reqId: string): void {
  const payload = Buffer.from(JSON.stringify({ message }), 'utf8');
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': payload.length,
    'x-github-request-id': reqId,
  });
  res.end(payload);
}

function sendGitBytes(res: ServerResponse, contentType: string, body: Buffer, reqId: string): void {
  res.writeHead(200, {
    'content-type': contentType,
    'cache-control': 'no-cache',
    'content-length': body.length,
    'x-github-request-id': reqId,
  });
  res.end(body);
}

function collectObjectClosure(
  repo: RepoRecord,
  commitOids: Iterable<string>,
  extraObjects?: Iterable<string>,
): { objects: Set<string> } {
  const seen = new Set<string>();
  const stack: string[] = [];

  if (extraObjects) {
    for (const oid of extraObjects) stack.push(oid);
  }
  for (const commitOid of commitOids) {
    const cObj = repo.objects.get(commitOid);
    if (!cObj) continue;
    seen.add(commitOid);
    if (cObj.type === 'commit') {
      stack.push(parseCommit(cObj.data).tree);
    }
  }

  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    seen.add(oid);
    if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) {
        if (entry.mode === '160000') continue;
        if (!seen.has(entry.oid)) stack.push(entry.oid);
      }
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return { objects: seen };
}

function resolveShallowAndExcludedCommits(
  repo: RepoRecord,
  wants: readonly string[],
  haves: readonly string[],
  shallowBoundaries: readonly string[],
  depth: number | undefined,
): {
  wantCommits: Set<string>;
  newShallows: Set<string>;
  unshallows: Set<string>;
  excludeCommits: Set<string>;
  extraRoots: Set<string>;
} {
  const clientShallows = new Set<string>(shallowBoundaries);
  const excludeCommits = new Set<string>();
  const hQueue: string[] = [];

  for (const have of haves) {
    const target = peelOid(repo.objects, have);
    if (repo.objects.has(target)) hQueue.push(target);
  }
  while (hQueue.length > 0) {
    const oid = hQueue.pop()!;
    if (excludeCommits.has(oid)) continue;
    excludeCommits.add(oid);
    if (clientShallows.has(oid)) continue;
    const parents = repo.commitParents.get(oid);
    if (parents) {
      for (const p of parents) {
        if (!excludeCommits.has(p)) hQueue.push(p);
      }
    }
  }

  const wantCommits = new Set<string>();
  const newShallows = new Set<string>();
  const extraRoots = new Set<string>();

  if (depth !== undefined && depth > 0) {
    const bestDist = new Map<string, number>();
    const bfs: Array<{ oid: string; d: number }> = [];
    for (const want of wants) {
      const wantObj = repo.objects.get(want);
      if (!wantObj) continue;
      if (wantObj.type === 'tag') extraRoots.add(want);
      const target = peelOid(repo.objects, want);
      const tObj = repo.objects.get(target);
      if (!tObj) continue;
      if (tObj.type === 'commit') {
        const prev = bestDist.get(target);
        if (prev === undefined || 1 < prev) {
          bestDist.set(target, 1);
          bfs.push({ oid: target, d: 1 });
        }
      } else {
        extraRoots.add(target);
      }
    }

    let headIdx = 0;
    while (headIdx < bfs.length) {
      const item = bfs[headIdx++]!;
      if (item.d > bestDist.get(item.oid)!) continue;
      wantCommits.add(item.oid);
      const parents = repo.commitParents.get(item.oid) ?? [];
      if (item.d >= depth) {
        if (parents.length > 0) newShallows.add(item.oid);
        continue;
      }
      if (excludeCommits.has(item.oid) && !clientShallows.has(item.oid)) continue;
      for (const parent of parents) {
        const nextDist = item.d + 1;
        const prev = bestDist.get(parent);
        if (prev === undefined || nextDist < prev) {
          bestDist.set(parent, nextDist);
          bfs.push({ oid: parent, d: nextDist });
        }
      }
    }
  } else {
    const stack: string[] = [];
    for (const want of wants) {
      const wantObj = repo.objects.get(want);
      if (!wantObj) continue;
      if (wantObj.type === 'tag') extraRoots.add(want);
      const target = peelOid(repo.objects, want);
      const tObj = repo.objects.get(target);
      if (!tObj) continue;
      if (tObj.type === 'commit') stack.push(target);
      else extraRoots.add(target);
    }
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (wantCommits.has(oid) || excludeCommits.has(oid)) continue;
      if (!repo.objects.has(oid)) continue;
      wantCommits.add(oid);
      const parents = repo.commitParents.get(oid);
      if (parents) {
        for (const parent of parents) {
          if (!wantCommits.has(parent) && !excludeCommits.has(parent)) {
            stack.push(parent);
          }
        }
      }
    }
  }

  const unshallows = new Set<string>();
  if (depth !== undefined && depth > 0) {
    for (const oldShallow of clientShallows) {
      if (wantCommits.has(oldShallow) && !newShallows.has(oldShallow)) {
        unshallows.add(oldShallow);
      }
    }
  }

  return { wantCommits, newShallows, unshallows, excludeCommits, extraRoots };
}

function buildDeltaFetchPack(
  repo: RepoRecord,
  wants: readonly string[],
  haves: readonly string[],
  shallowBoundaries: readonly string[],
  depth: number | undefined,
): {
  pack: Buffer;
  newShallows: Set<string>;
  unshallows: Set<string>;
  ackOids: string[];
} {
  const { wantCommits, newShallows, unshallows, excludeCommits, extraRoots } =
    resolveShallowAndExcludedCommits(repo, wants, haves, shallowBoundaries, depth);

  const ackOids: string[] = [];
  for (const h of haves) {
    if (repo.objects.has(h)) ackOids.push(h);
  }

  const toSendCommits: string[] = [];
  for (const oid of wantCommits) {
    if (!excludeCommits.has(oid)) toSendCommits.push(oid);
  }

  if (toSendCommits.length === 0 && extraRoots.size === 0) {
    return { pack: EMPTY_PACKFILE, newShallows, unshallows, ackOids };
  }

  const excluded = collectObjectClosure(repo, excludeCommits);
  const wanted = collectObjectClosure(repo, toSendCommits, extraRoots);
  const packObjects: GitObject[] = [];
  for (const oid of wanted.objects) {
    if (excluded.objects.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj) packObjects.push(obj);
  }

  const pack = packObjects.length === 0 ? EMPTY_PACKFILE : buildPackfile(packObjects);
  return { pack, newShallows, unshallows, ackOids };
}

function formatV0Advertisement(repo: RepoRecord, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const sortedRefs = [...repo.refs.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  const caps =
    service === 'git-upload-pack'
      ? `multi_ack_detailed side-band-64k ofs-delta shallow thin-pack no-progress include-tag symref=HEAD:refs/heads/${repo.defaultBranch} agent=makefun-forge/1.0`
      : 'report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=makefun-forge/1.0';

  const lines: (string | 'flush')[] = [`# service=${service}\n`, 'flush'];
  const entries: string[] = [];

  if (service === 'git-upload-pack') {
    const headOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? sortedRefs[0]?.[1];
    if (headOid) {
      entries.push(`${headOid} HEAD`);
    }
  }

  for (const [refName, oid] of sortedRefs) {
    entries.push(`${oid} ${refName}`);
    const obj = repo.objects.get(oid);
    if (obj && obj.type === 'tag') {
      const peeled = peelOid(repo.objects, oid);
      if (peeled !== oid) {
        entries.push(`${peeled} ${refName}^{}`);
      }
    }
  }

  if (entries.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\u0000${caps}\n`);
  } else {
    entries.forEach((line, idx) => {
      lines.push(idx === 0 ? `${line}\u0000${caps}\n` : `${line}\n`);
    });
  }
  lines.push('flush');
  return encodePktLines(lines);
}

function handleUploadPackV2(repo: RepoRecord, packets: ReturnType<typeof decodePktLines>['packets'], rawBodyKey: string): Buffer {
  let command = '';
  let inArgs = false;
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | undefined;

  for (const pkt of packets) {
    if (pkt.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (pkt.kind === 'flush') break;
    if (pkt.kind !== 'data') continue;
    const line = pkt.payload.toString('utf8').replace(/\n$/, '');
    if (!inArgs) {
      if (line.startsWith('command=')) command = line.slice('command='.length);
      continue;
    }
    if (command === 'ls-refs') {
      if (line === 'peel') peel = true;
      else if (line === 'symrefs') symrefs = true;
      else if (line === 'unborn') unborn = true;
      else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
    } else if (command === 'fetch') {
      if (line.startsWith('want ')) wants.push(line.slice(5).trim());
      else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) {
        const parsed = Number(line.slice(7).trim());
        if (Number.isInteger(parsed) && parsed > 0) depth = parsed;
      }
    }
  }

  if (command === 'ls-refs') {
    const cached = repo.lsRefsCache.get(rawBodyKey);
    if (cached && cached.version === repo.refVersion) {
      return cached.buf;
    }

    const matchPrefix = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
    const sortedRefs = [...repo.refs.entries()].sort((a, b) => a[0].localeCompare(b[0]));
    const out: (string | 'flush')[] = [];

    if (matchPrefix('HEAD')) {
      const headRef = `refs/heads/${repo.defaultBranch}`;
      const headOid = repo.refs.get(headRef);
      if (headOid) {
        out.push(symrefs ? `${headOid} HEAD symref-target:${headRef}\n` : `${headOid} HEAD\n`);
      } else if (unborn) {
        out.push(symrefs ? `unborn HEAD symref-target:${headRef}\n` : `unborn HEAD\n`);
      }
    }

    for (const [refName, oid] of sortedRefs) {
      if (!matchPrefix(refName)) continue;
      let line = `${oid} ${refName}`;
      if (peel) {
        const obj = repo.objects.get(oid);
        if (obj && obj.type === 'tag') {
          const peeled = peelOid(repo.objects, oid);
          if (peeled !== oid) line += ` peeled:${peeled}`;
        }
      }
      out.push(`${line}\n`);
    }

    out.push('flush');
    const encoded = encodePktLines(out);
    repo.lsRefsCache.set(rawBodyKey, { version: repo.refVersion, buf: encoded });
    return encoded;
  }

  if (command === 'fetch') {
    const isDepth1Cacheable = depth === 1 && haves.length === 0 && shallow.length === 0 && wants.length === 1;
    if (isDepth1Cacheable) {
      const cached = repo.shallowFetchCache.get(wants[0]!);
      if (cached && cached.version === repo.refVersion) {
        return cached.buf;
      }
    }

    const fetched = buildDeltaFetchPack(repo, wants, haves, shallow, depth);
    const chunks: Buffer[] = [];
    if (fetched.newShallows.size > 0 || fetched.unshallows.size > 0) {
      const shallowLines: (string | 'delim')[] = ['shallow-info\n'];
      for (const s of fetched.newShallows) shallowLines.push(`shallow ${s}\n`);
      for (const u of fetched.unshallows) shallowLines.push(`unshallow ${u}\n`);
      shallowLines.push('delim');
      chunks.push(encodePktLines(shallowLines));
    }

    chunks.push(encodePktLines(['packfile\n']));
    chunks.push(encodeSideband(1, fetched.pack));
    chunks.push(FLUSH_PKT);
    const body = Buffer.concat(chunks);

    if (isDepth1Cacheable) {
      repo.shallowFetchCache.set(wants[0]!, { version: repo.refVersion, buf: body });
    }
    return body;
  }

  throw new MalformedPktLineError(`unsupported git v2 command ${command}`);
}

function handleUploadPackV0(repo: RepoRecord, packets: ReturnType<typeof decodePktLines>['packets']): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | undefined;
  let section = 0;

  for (const pkt of packets) {
    if (pkt.kind === 'flush') {
      section += 1;
      continue;
    }
    if (pkt.kind !== 'data') continue;
    const raw = pkt.payload.toString('utf8').replace(/\n$/, '');
    const nul = raw.indexOf('\u0000');
    const clean = (nul >= 0 ? raw.slice(0, nul) : raw).trim();
    if (!clean) continue;

    if (section === 0) {
      if (clean.startsWith('want ')) {
        const oid = clean.slice(5).split(' ')[0]!;
        wants.push(oid);
      } else if (clean.startsWith('shallow ')) {
        shallow.push(clean.slice(8).trim());
      } else if (clean.startsWith('deepen ')) {
        const parsed = Number(clean.slice(7).trim());
        if (Number.isInteger(parsed) && parsed > 0) depth = parsed;
      }
    } else if (clean.startsWith('have ')) {
      haves.push(clean.slice(5).trim());
    }
  }

  const fetched = buildDeltaFetchPack(repo, wants, haves, shallow, depth);
  const chunks: Buffer[] = [];
  if (fetched.newShallows.size > 0 || fetched.unshallows.size > 0) {
    const lines: (string | 'flush')[] = [];
    for (const s of fetched.newShallows) lines.push(`shallow ${s}\n`);
    for (const u of fetched.unshallows) lines.push(`unshallow ${u}\n`);
    lines.push('flush');
    chunks.push(encodePktLines(lines));
  }
  chunks.push(encodePktLines(['NAK\n']));
  chunks.push(encodeSideband(1, fetched.pack));
  chunks.push(FLUSH_PKT);
  return Buffer.concat(chunks);
}

function handleReceivePack(store: ForgeStore, repo: RepoRecord, body: Buffer): Buffer {
  const decoded = decodePktLines(body, { stopAtFlush: true });
  const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
  const capSet = new Set<string>();

  let first = true;
  for (const pkt of decoded.packets) {
    if (pkt.kind !== 'data') continue;
    let text = pkt.payload.toString('utf8').replace(/\n$/, '');
    if (first) {
      first = false;
      const nul = text.indexOf('\u0000');
      if (nul >= 0) {
        const capStr = text.slice(nul + 1);
        for (const cap of capStr.split(' ')) {
          if (cap) capSet.add(cap);
        }
        text = text.slice(0, nul);
      }
    }
    const parts = text.split(' ');
    if (parts.length !== 3 || !OID_REGEX.test(parts[0]!) || !OID_REGEX.test(parts[1]!)) {
      throw new MalformedPktLineError(`invalid receive-pack command: ${text}`);
    }
    commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
  }

  const packSlice = body.subarray(decoded.consumed);
  let unpackError: string | null = null;
  if (packSlice.length > 0) {
    try {
      const unpacked = unpackPackfile(packSlice, (oid) => repo.objects.get(oid));
      for (const obj of unpacked) {
        store.saveGitObject(repo, obj);
      }
    } catch (err) {
      unpackError = err instanceof Error ? err.message : 'unpack failed';
    }
  }

  const reportLines: (string | 'flush')[] = [];
  if (unpackError !== null) {
    reportLines.push(`unpack ${unpackError}\n`);
    for (const cmd of commands) {
      reportLines.push(`ng ${cmd.ref} unpack failed\n`);
    }
  } else {
    reportLines.push('unpack ok\n');
    let mutated = false;
    for (const cmd of commands) {
      const currentOid = repo.refs.get(cmd.ref) ?? ZERO_OID;
      if (cmd.newOid === ZERO_OID) {
        if (currentOid !== ZERO_OID && currentOid !== cmd.oldOid) {
          reportLines.push(`ng ${cmd.ref} failed to lock\n`);
          continue;
        }
        repo.refs.delete(cmd.ref);
        mutated = true;
        reportLines.push(`ok ${cmd.ref}\n`);
        continue;
      }

      if (!repo.objects.has(cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} missing object\n`);
        continue;
      }

      if (cmd.oldOid === ZERO_OID) {
        if (currentOid !== ZERO_OID && currentOid !== cmd.newOid) {
          reportLines.push(`ng ${cmd.ref} already exists\n`);
          continue;
        }
        repo.refs.set(cmd.ref, cmd.newOid);
        mutated = true;
        reportLines.push(`ok ${cmd.ref}\n`);
        continue;
      }

      if (currentOid !== cmd.oldOid) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward\n`);
        continue;
      }

      const ff = isAncestor((oid) => repo.commitParents.get(oid), cmd.oldOid, cmd.newOid);
      if (!ff) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward\n`);
        continue;
      }

      repo.refs.set(cmd.ref, cmd.newOid);
      mutated = true;
      reportLines.push(`ok ${cmd.ref}\n`);
    }

    if (mutated) {
      store.bumpRefVersion(repo);
    }
  }

  reportLines.push('flush');
  const reportBytes = encodePktLines(reportLines);
  if (capSet.has('side-band-64k')) {
    return Buffer.concat([encodeSideband(1, reportBytes), FLUSH_PKT]);
  }
  return reportBytes;
}

export function handleGitHttp(
  req: IncomingMessage,
  res: ServerResponse,
  store: ForgeStore,
  urlPath: string,
  queryStr: string,
  body: Buffer,
  reqId: string,
): boolean {
  const gitSuffix = urlPath.indexOf('.git/');
  if (gitSuffix < 0) return false;
  const repoSlug = urlPath.slice(1, gitSuffix);
  const slash = repoSlug.indexOf('/');
  if (slash <= 0 || repoSlug.indexOf('/', slash + 1) >= 0) return false;

  const owner = decodeURIComponent(repoSlug.slice(0, slash));
  const repoName = decodeURIComponent(repoSlug.slice(slash + 1));
  const subPath = urlPath.slice(gitSuffix + 4);
  const repo = store.getRepoIfExists(owner, repoName);

  if (req.method === 'GET' && subPath === '/info/refs') {
    if (!repo) {
      sendGitError(res, 404, 'Not Found', reqId);
      return true;
    }
    const params = new URLSearchParams(queryStr);
    const service = params.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      sendGitError(res, 403, 'Forbidden', reqId);
      return true;
    }

    if (service === 'git-upload-pack') {
      const protoHeader = String(req.headers['git-protocol'] ?? '');
      if (protoHeader.includes('version=2')) {
        sendGitBytes(res, 'application/x-git-upload-pack-advertisement', V2_UPLOAD_ADVERTISEMENT, reqId);
        return true;
      }
      const v0 = formatV0Advertisement(repo, 'git-upload-pack');
      sendGitBytes(res, 'application/x-git-upload-pack-advertisement', v0, reqId);
      return true;
    }

    if (repo.receiveRefsCache && repo.receiveRefsCache.version === repo.refVersion) {
      sendGitBytes(res, 'application/x-git-receive-pack-advertisement', repo.receiveRefsCache.buf, reqId);
      return true;
    }
    const buf = formatV0Advertisement(repo, 'git-receive-pack');
    repo.receiveRefsCache = { version: repo.refVersion, buf };
    sendGitBytes(res, 'application/x-git-receive-pack-advertisement', buf, reqId);
    return true;
  }

  if (req.method === 'POST' && subPath === '/git-upload-pack') {
    if (!repo) {
      sendGitError(res, 404, 'Not Found', reqId);
      return true;
    }
    try {
      const { packets } = decodePktLines(body);
      const firstData = packets.find((p) => p.kind === 'data');
      const firstText = firstData && firstData.kind === 'data' ? firstData.payload.toString('utf8') : '';
      const isV2 = String(req.headers['git-protocol'] ?? '').includes('version=2') || firstText.startsWith('command=');
      const reply = isV2
        ? handleUploadPackV2(repo, packets, body.toString('utf8'))
        : handleUploadPackV0(repo, packets);
      sendGitBytes(res, 'application/x-git-upload-pack-result', reply, reqId);
    } catch (err) {
      const msg = err instanceof MalformedPktLineError || err instanceof PackFormatError ? err.message : 'Bad Request';
      sendGitError(res, 400, msg, reqId);
    }
    return true;
  }

  if (req.method === 'POST' && subPath === '/git-receive-pack') {
    if (!repo) {
      sendGitError(res, 404, 'Not Found', reqId);
      return true;
    }
    try {
      const reply = handleReceivePack(store, repo, body);
      sendGitBytes(res, 'application/x-git-receive-pack-result', reply, reqId);
    } catch (err) {
      const msg = err instanceof MalformedPktLineError || err instanceof PackFormatError ? err.message : 'Bad Request';
      sendGitError(res, 400, msg, reqId);
    }
    return true;
  }

  sendGitError(res, 404, 'Not Found', reqId);
  return true;
}
