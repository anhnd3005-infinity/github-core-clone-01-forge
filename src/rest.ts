import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  ForgeStore,
  StoreError,
  serializeIssue,
  serializePull,
  serializeRepo,
} from './store.ts';

function sendJsonBuf(
  res: ServerResponse,
  status: number,
  buf: Buffer,
  reqId: string,
  extraHeaders?: Record<string, string>,
): void {
  if (extraHeaders === undefined) {
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': buf.length,
      'x-github-request-id': reqId,
    });
  } else {
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': buf.length,
      'x-github-request-id': reqId,
      ...extraHeaders,
    });
  }
  res.end(buf);
}

function sendJson(
  res: ServerResponse,
  status: number,
  value: unknown,
  reqId: string,
  extraHeaders?: Record<string, string>,
): void {
  sendJsonBuf(res, status, Buffer.from(JSON.stringify(value), 'utf8'), reqId, extraHeaders);
}

function sendNoContent(res: ServerResponse, reqId: string): void {
  res.writeHead(204, {
    'content-type': 'application/json; charset=utf-8',
    'x-github-request-id': reqId,
  });
  res.end();
}

export function isAuthorizedRequest(req: IncomingMessage, expectedToken: string | undefined): boolean {
  const header = req.headers.authorization;
  if (typeof header !== 'string') return false;
  if (!header.startsWith('Bearer ')) return false;
  const provided = header.slice('Bearer '.length).trim();
  if (provided === '') return false;
  if (expectedToken && expectedToken !== '') {
    return provided === expectedToken;
  }
  return true;
}

function parseJsonBody(body: Buffer): Record<string, unknown> {
  if (body.length === 0) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    throw new StoreError(400, 'Problems parsing JSON');
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new StoreError(400, 'JSON body must be an object');
  }
  return parsed as Record<string, unknown>;
}

const META_JSON = Buffer.from(JSON.stringify({ installed_version: '1.0.0' }), 'utf8');

export function handleRestApi(
  req: IncomingMessage,
  res: ServerResponse,
  store: ForgeStore,
  urlPath: string,
  queryStr: string,
  body: Buffer,
  reqId: string,
  expectedToken: string | undefined,
): boolean {
  if (urlPath !== '/api/v3' && !urlPath.startsWith('/api/v3/')) {
    return false;
  }

  if (!isAuthorizedRequest(req, expectedToken)) {
    sendJson(res, 401, { message: 'Requires authentication' }, reqId);
    return true;
  }

  const method = req.method ?? 'GET';
  const relPath = urlPath === '/api/v3' ? '/' : urlPath.slice('/api/v3'.length);

  try {
    if (method === 'GET' && relPath === '/') {
      const host = req.headers.host ?? '127.0.0.1';
      sendJson(
        res,
        200,
        {
          current_user_url: `http://${host}/api/v3/user`,
          rate_limit_url: `http://${host}/api/v3/rate_limit`,
        },
        reqId,
        { 'x-oauth-scopes': 'repo, read:org' },
      );
      return true;
    }

    if (method === 'GET' && relPath === '/meta') {
      sendJsonBuf(res, 200, META_JSON, reqId);
      return true;
    }

    if (method === 'GET' && relPath === '/user') {
      sendJson(res, 200, store.viewer, reqId);
      return true;
    }

    if (relPath === '/user/repos') {
      if (method === 'POST') {
        const json = parseJsonBody(body);
        const repo = store.createRepo(json);
        sendJson(res, 201, serializeRepo(repo), reqId);
        return true;
      }
      if (method === 'GET') {
        const params = new URLSearchParams(queryStr);
        const rawPerPage = Number(params.get('per_page') ?? '30');
        const perPage = Number.isInteger(rawPerPage) && rawPerPage > 0 ? Math.min(100, rawPerPage) : 30;
        const rawPage = Number(params.get('page') ?? '1');
        const page = Number.isInteger(rawPage) && rawPage > 0 ? rawPage : 1;

        const allRepos = store.listViewerRepos();
        const start = (page - 1) * perPage;
        const slice = allRepos.slice(start, start + perPage).map(serializeRepo);
        const extra: Record<string, string> = {};
        if (start + slice.length < allRepos.length) {
          const host = req.headers.host ?? '127.0.0.1';
          extra.link = `<http://${host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}>; rel="next"`;
        }
        sendJson(res, 200, slice, reqId, extra);
        return true;
      }
    }

    if (relPath.startsWith('/repos/')) {
      const rest = relPath.slice('/repos/'.length);
      const parts = rest.split('/');
      if (parts.length < 2 || !parts[0] || !parts[1]) {
        sendJson(res, 404, { message: 'Not Found' }, reqId);
        return true;
      }
      const owner = decodeURIComponent(parts[0]);
      const repoName = decodeURIComponent(parts[1]);

      if (parts.length === 2) {
        if (method === 'GET') {
          sendJson(res, 200, serializeRepo(store.requireRepo(owner, repoName)), reqId);
          return true;
        }
        if (method === 'PATCH') {
          const json = parseJsonBody(body);
          sendJson(res, 200, serializeRepo(store.patchRepo(owner, repoName, json)), reqId);
          return true;
        }
        if (method === 'DELETE') {
          store.deleteRepo(owner, repoName);
          sendNoContent(res, reqId);
          return true;
        }
      }

      if (parts.length === 4 && parts[2] === 'issues' && method === 'GET') {
        const issueNum = Number(parts[3]);
        if (!Number.isInteger(issueNum) || issueNum <= 0) {
          sendJson(res, 404, { message: 'Not Found' }, reqId);
          return true;
        }
        const issue = store.getIssue(owner, repoName, issueNum);
        if (issue.cachedJson) {
          sendJsonBuf(res, 200, issue.cachedJson, reqId);
        } else {
          sendJson(res, 200, serializeIssue(issue), reqId);
        }
        return true;
      }

      if (parts.length === 3 && parts[2] === 'issues') {
        if (method === 'POST') {
          const json = parseJsonBody(body);
          const issue = store.createIssue(owner, repoName, json);
          sendJsonBuf(res, 201, issue.cachedJson ?? Buffer.from(JSON.stringify(serializeIssue(issue)), 'utf8'), reqId);
          return true;
        }
        if (method === 'GET') {
          const params = new URLSearchParams(queryStr);
          const state = params.get('state') ?? 'open';
          const direction = params.get('direction') ?? 'desc';
          const rawPerPage = Number(params.get('per_page') ?? '30');
          const perPage = Number.isInteger(rawPerPage) && rawPerPage > 0 ? Math.min(100, rawPerPage) : 30;
          const rawPage = Number(params.get('page') ?? '1');
          const page = Number.isInteger(rawPage) && rawPage > 0 ? rawPage : 1;

          const items = store.listIssues(owner, repoName, { state, direction });
          const start = (page - 1) * perPage;
          const pageItems = items.slice(start, start + perPage).map(serializeIssue);
          sendJson(res, 200, pageItems, reqId);
          return true;
        }
      }

      if (parts.length === 4 && parts[2] === 'issues' && method === 'PATCH') {
        const issueNum = Number(parts[3]);
        if (!Number.isInteger(issueNum) || issueNum <= 0) {
          sendJson(res, 404, { message: 'Not Found' }, reqId);
          return true;
        }
        const json = parseJsonBody(body);
        const issue = store.updateIssue(owner, repoName, issueNum, json);
        sendJsonBuf(res, 200, issue.cachedJson ?? Buffer.from(JSON.stringify(serializeIssue(issue)), 'utf8'), reqId);
        return true;
      }

      if (parts.length === 5 && parts[2] === 'issues' && parts[4] === 'comments') {
        const issueNum = Number(parts[3]);
        if (!Number.isInteger(issueNum) || issueNum <= 0) {
          sendJson(res, 404, { message: 'Not Found' }, reqId);
          return true;
        }
        if (method === 'POST') {
          const json = parseJsonBody(body);
          const c = store.addComment(owner, repoName, issueNum, json.body);
          sendJson(res, 201, { id: c.id, node_id: c.globalId, body: c.body, created_at: c.createdAt }, reqId);
          return true;
        }
        if (method === 'GET') {
          const list = store.listComments(owner, repoName, issueNum).map((c) => ({
            id: c.id,
            node_id: c.globalId,
            body: c.body,
            created_at: c.createdAt,
          }));
          sendJson(res, 200, list, reqId);
          return true;
        }
      }

      if (parts.length === 3 && parts[2] === 'labels') {
        if (method === 'POST') {
          const json = parseJsonBody(body);
          sendJson(res, 201, store.createLabel(owner, repoName, json), reqId);
          return true;
        }
        if (method === 'GET') {
          sendJson(res, 200, store.listLabels(owner, repoName), reqId);
          return true;
        }
      }

      if (parts[2] === 'contents') {
        const filePath = parts.slice(3).map(decodeURIComponent).join('/');
        if (method === 'PUT') {
          const json = parseJsonBody(body);
          const out = store.putContent(owner, repoName, filePath, json);
          sendJson(res, out.status, { content: out.content, commit: out.commit }, reqId);
          return true;
        }
        if (method === 'GET') {
          const params = new URLSearchParams(queryStr);
          const ref = params.get('ref') ?? undefined;
          sendJson(res, 200, store.getContent(owner, repoName, filePath, ref), reqId);
          return true;
        }
        if (method === 'DELETE') {
          const json = parseJsonBody(body);
          sendJson(res, 200, store.deleteContent(owner, repoName, filePath, json), reqId);
          return true;
        }
      }

      if (parts.length === 4 && parts[2] === 'git' && parts[3] === 'refs' && method === 'POST') {
        const json = parseJsonBody(body);
        sendJson(res, 201, store.createRef(owner, repoName, json), reqId);
        return true;
      }

      if (parts.length >= 5 && parts[2] === 'git' && parts[3] === 'refs' && method === 'GET') {
        const refPath = parts.slice(4).map(decodeURIComponent).join('/');
        sendJson(res, 200, store.getRef(owner, repoName, refPath), reqId);
        return true;
      }

      if (parts.length === 3 && parts[2] === 'branches' && method === 'GET') {
        sendJson(res, 200, store.listBranches(owner, repoName), reqId);
        return true;
      }

      if (parts.length >= 4 && parts[2] === 'branches' && method === 'GET') {
        const branchName = parts.slice(3).map(decodeURIComponent).join('/');
        sendJson(res, 200, store.getBranch(owner, repoName, branchName), reqId);
        return true;
      }

      if (parts.length === 3 && parts[2] === 'commits' && method === 'GET') {
        const params = new URLSearchParams(queryStr);
        const sha = params.get('sha') ?? undefined;
        sendJson(res, 200, store.listCommits(owner, repoName, sha), reqId);
        return true;
      }

      if (parts.length === 3 && parts[2] === 'pulls') {
        if (method === 'POST') {
          const json = parseJsonBody(body);
          sendJson(res, 201, serializePull(store.createPull(owner, repoName, json)), reqId);
          return true;
        }
        if (method === 'GET') {
          const params = new URLSearchParams(queryStr);
          const state = params.get('state') ?? 'open';
          const rawPerPage = Number(params.get('per_page') ?? '30');
          const perPage = Number.isInteger(rawPerPage) && rawPerPage > 0 ? Math.min(100, rawPerPage) : 30;
          const rawPage = Number(params.get('page') ?? '1');
          const page = Number.isInteger(rawPage) && rawPage > 0 ? rawPage : 1;

          const items = store.listPulls(owner, repoName, state);
          const start = (page - 1) * perPage;
          sendJson(res, 200, items.slice(start, start + perPage).map(serializePull), reqId);
          return true;
        }
      }

      if (parts.length >= 4 && parts[2] === 'pulls') {
        const pullNum = Number(parts[3]);
        if (!Number.isInteger(pullNum) || pullNum <= 0) {
          sendJson(res, 404, { message: 'Not Found' }, reqId);
          return true;
        }
        if (parts.length === 4 && method === 'GET') {
          sendJson(res, 200, serializePull(store.getPull(owner, repoName, pullNum)), reqId);
          return true;
        }
        if (parts.length === 5 && parts[4] === 'files' && method === 'GET') {
          sendJson(res, 200, store.getPullFiles(owner, repoName, pullNum), reqId);
          return true;
        }
        if (parts.length === 5 && parts[4] === 'merge' && method === 'PUT') {
          const json = parseJsonBody(body);
          const mergeMethod = typeof json.merge_method === 'string' ? json.merge_method : 'squash';
          sendJson(res, 200, store.mergePull(owner, repoName, pullNum, mergeMethod), reqId);
          return true;
        }
      }
    }

    sendJson(res, 404, { message: 'Not Found' }, reqId);
  } catch (err) {
    if (err instanceof StoreError) {
      sendJson(res, err.status, { message: err.message }, reqId);
    } else {
      sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' }, reqId);
    }
  }
  return true;
}
