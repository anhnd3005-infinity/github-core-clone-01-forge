import http, { type IncomingMessage, type ServerResponse } from 'node:http';
import { handleGitHttp } from './git-http.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRestApi } from './rest.ts';
import { ForgeStore } from './store.ts';

const HEALTHZ_BODY = Buffer.from('{"ok":true}', 'utf8');
const NOT_FOUND_BODY = Buffer.from('{"message":"Not Found"}', 'utf8');
const INTERNAL_ERROR_BODY = Buffer.from('{"message":"Internal Server Error"}', 'utf8');
const EMPTY_BUFFER = Buffer.alloc(0);

function readRequestBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer | string) => {
      const buf = typeof chunk === 'string' ? Buffer.from(chunk, 'utf8') : chunk;
      chunks.push(buf);
      total += buf.length;
    });
    req.on('end', () => {
      if (chunks.length === 0) resolve(EMPTY_BUFFER);
      else if (chunks.length === 1) resolve(chunks[0]!);
      else resolve(Buffer.concat(chunks, total));
    });
    req.on('error', reject);
  });
}

function sendNotFound(res: ServerResponse, reqId: string): void {
  res.writeHead(404, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': NOT_FOUND_BODY.length,
    'x-github-request-id': reqId,
  });
  res.end(NOT_FOUND_BODY);
}

function sendInternalError(res: ServerResponse, reqId: string): void {
  if (!res.headersSent) {
    res.writeHead(500, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': INTERNAL_ERROR_BODY.length,
      'x-github-request-id': reqId,
    });
  }
  if (!res.writableEnded) {
    res.end(INTERNAL_ERROR_BODY);
  }
}

export function createForgeServer(options: {
  token?: string;
  viewerLogin?: string;
  trialNow?: string;
} = {}): http.Server {
  const store = new ForgeStore({
    viewerLogin: options.viewerLogin,
    trialNow: options.trialNow,
  });
  const expectedToken = options.token;
  let reqSeq = 0;

  const dispatch = (
    req: IncomingMessage,
    res: ServerResponse,
    urlPath: string,
    queryStr: string,
    body: Buffer,
    reqId: string,
  ): void => {
    if (urlPath === '/api/graphql') {
      if (handleGraphQL(req, res, store, urlPath, body, reqId, expectedToken)) return;
    } else if (urlPath === '/mcp') {
      if (handleMcp(req, res, store, urlPath, body, reqId, expectedToken)) return;
    } else if (urlPath === '/api/v3' || urlPath.startsWith('/api/v3/')) {
      if (handleRestApi(req, res, store, urlPath, queryStr, body, reqId, expectedToken)) return;
    } else if (urlPath.includes('.git/')) {
      if (handleGitHttp(req, res, store, urlPath, queryStr, body, reqId)) return;
    }
    sendNotFound(res, reqId);
  };

  const server = http.createServer((req, res) => {
    const reqId = `mf-${++reqSeq}`;
    try {
      const rawUrl = req.url ?? '/';
      const qIdx = rawUrl.indexOf('?');
      const urlPath = qIdx >= 0 ? rawUrl.slice(0, qIdx) : rawUrl;
      const queryStr = qIdx >= 0 ? rawUrl.slice(qIdx + 1) : '';
      const method = req.method ?? 'GET';

      if (method === 'GET' && urlPath === '/healthz') {
        res.writeHead(200, {
          'content-type': 'application/json; charset=utf-8',
          'content-length': HEALTHZ_BODY.length,
          'x-github-request-id': reqId,
        });
        res.end(HEALTHZ_BODY);
        return;
      }

      if (method === 'GET' || method === 'HEAD') {
        dispatch(req, res, urlPath, queryStr, EMPTY_BUFFER, reqId);
        return;
      }

      const contentLen = req.headers['content-length'];
      if (contentLen === '0') {
        dispatch(req, res, urlPath, queryStr, EMPTY_BUFFER, reqId);
        return;
      }

      readRequestBody(req).then(
        (body) => {
          try {
            dispatch(req, res, urlPath, queryStr, body, reqId);
          } catch {
            sendInternalError(res, reqId);
          }
        },
        () => {
          sendInternalError(res, reqId);
        },
      );
    } catch {
      sendInternalError(res, reqId);
    }
  });

  server.keepAliveTimeout = 60_000;
  server.headersTimeout = 65_000;
  server.requestTimeout = 0;
  server.on('connection', (socket) => {
    socket.setNoDelay(true);
  });

  return server;
}
