import type { IncomingMessage, ServerResponse } from 'node:http';
import { isAuthorizedRequest } from './rest.ts';
import {
  ForgeStore,
  StoreError,
  serializeIssue,
  serializePull,
  serializeRepo,
} from './store.ts';

const TOOLS_DEFINITION = Object.freeze([
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Fetch metadata for an existing repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create a new issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body text' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues for a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state filter: open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a new pull request between head and base branches.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge an open pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
        merge_method: { type: 'string', description: 'Merge method (squash, merge, rebase)' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
]);

function sendMcpJson(res: ServerResponse, status: number, payload: unknown, reqId: string): void {
  const buf = Buffer.from(JSON.stringify(payload), 'utf8');
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': buf.length,
    'mcp-protocol-version': '2026-07-28',
    'x-github-request-id': reqId,
  });
  res.end(buf);
}

function toolOk(data: unknown): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(data) }],
    structuredContent: data,
    isError: false,
  };
}

function executeTool(store: ForgeStore, name: string, args: Record<string, unknown>): {
  rpcError?: { code: number; message: string };
  result?: Record<string, unknown>;
} {
  const shortName = name.includes('__') ? name.split('__').pop()! : name;

  const missing = (fields: readonly string[]): string[] =>
    fields.filter((f) => args[f] === undefined || args[f] === null || args[f] === '');

  try {
    switch (shortName) {
      case 'create_repo': {
        const miss = missing(['name']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const repo = store.createRepo({
          name: args.name,
          description: args.description,
          private: args.private,
        });
        return { result: toolOk(serializeRepo(repo)) };
      }

      case 'get_repo': {
        const miss = missing(['owner', 'repo']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const repo = store.requireRepo(String(args.owner), String(args.repo));
        return { result: toolOk(serializeRepo(repo)) };
      }

      case 'create_issue': {
        const miss = missing(['owner', 'repo', 'title']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const issue = store.createIssue(String(args.owner), String(args.repo), {
          title: args.title,
          body: args.body,
        });
        return { result: toolOk(serializeIssue(issue)) };
      }

      case 'list_issues': {
        const miss = missing(['owner', 'repo']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const list = store.listIssues(String(args.owner), String(args.repo), {
          state: typeof args.state === 'string' ? args.state : 'open',
        });
        return { result: toolOk(list.map(serializeIssue)) };
      }

      case 'create_pull_request': {
        const miss = missing(['owner', 'repo', 'title', 'head', 'base']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const pull = store.createPull(String(args.owner), String(args.repo), {
          title: args.title,
          body: args.body,
          head: args.head,
          base: args.base,
        });
        return { result: toolOk(serializePull(pull)) };
      }

      case 'merge_pull_request': {
        const miss = missing(['owner', 'repo', 'pull_number']);
        if (miss.length > 0) return { rpcError: { code: -32602, message: `Missing required argument(s): ${miss.join(', ')}` } };
        const num = Number(args.pull_number);
        if (!Number.isInteger(num) || num <= 0) {
          return { rpcError: { code: -32602, message: 'Invalid pull_number' } };
        }
        const merged = store.mergePull(
          String(args.owner),
          String(args.repo),
          num,
          typeof args.merge_method === 'string' ? args.merge_method : 'squash',
        );
        return { result: toolOk(merged) };
      }

      default:
        return { rpcError: { code: -32601, message: `Unknown tool: ${name}` } };
    }
  } catch (err) {
    const msg = err instanceof StoreError || err instanceof Error ? err.message : 'Tool error';
    return {
      result: {
        content: [{ type: 'text', text: msg }],
        isError: true,
      },
    };
  }
}

export function handleMcp(
  req: IncomingMessage,
  res: ServerResponse,
  store: ForgeStore,
  urlPath: string,
  body: Buffer,
  reqId: string,
  expectedToken: string | undefined,
): boolean {
  if (urlPath !== '/mcp') return false;

  if (!isAuthorizedRequest(req, expectedToken)) {
    const payload = Buffer.from(JSON.stringify({ message: 'Requires authentication' }), 'utf8');
    res.writeHead(401, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': payload.length,
      'x-github-request-id': reqId,
    });
    res.end(payload);
    return true;
  }

  if (req.method === 'DELETE') {
    res.writeHead(204, { 'x-github-request-id': reqId });
    res.end();
    return true;
  }

  if (req.method !== 'POST') {
    sendMcpJson(res, 405, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Method Not Allowed' } }, reqId);
    return true;
  }

  let msg: unknown;
  try {
    msg = JSON.parse(body.toString('utf8'));
  } catch {
    sendMcpJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }, reqId);
    return true;
  }

  if (typeof msg !== 'object' || msg === null || Array.isArray(msg)) {
    sendMcpJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } }, reqId);
    return true;
  }

  const rpc = msg as Record<string, unknown>;
  const id = rpc.id === undefined ? null : rpc.id;
  const method = typeof rpc.method === 'string' ? rpc.method : '';

  if (method === 'notifications/initialized' || (rpc.id === undefined && method.startsWith('notifications/'))) {
    res.writeHead(202, { 'x-github-request-id': reqId });
    res.end();
    return true;
  }

  if (method === 'server/discover') {
    sendMcpJson(
      res,
      200,
      {
        jsonrpc: '2.0',
        id,
        result: {
          supportedVersions: ['2026-07-28', '2025-11-25'],
          capabilities: { tools: {} },
          serverInfo: { name: 'makefun-git-forge', version: '1.0.0' },
          _meta: {
            'io.modelcontextprotocol/serverInfo': { name: 'makefun-git-forge', version: '1.0.0' },
          },
        },
      },
      reqId,
    );
    return true;
  }

  if (method === 'initialize') {
    sendMcpJson(
      res,
      200,
      {
        jsonrpc: '2.0',
        id,
        result: {
          protocolVersion: '2025-11-25',
          capabilities: { tools: {} },
          serverInfo: { name: 'makefun-git-forge', version: '1.0.0' },
        },
      },
      reqId,
    );
    return true;
  }

  if (method === 'tools/list') {
    sendMcpJson(res, 200, { jsonrpc: '2.0', id, result: { tools: TOOLS_DEFINITION } }, reqId);
    return true;
  }

  if (method === 'tools/call') {
    const params = rpc.params && typeof rpc.params === 'object' && !Array.isArray(rpc.params)
      ? (rpc.params as Record<string, unknown>)
      : {};
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = params.arguments && typeof params.arguments === 'object' && !Array.isArray(params.arguments)
      ? (params.arguments as Record<string, unknown>)
      : {};
    const outcome = executeTool(store, toolName, args);
    if (outcome.rpcError) {
      sendMcpJson(res, 200, { jsonrpc: '2.0', id, error: outcome.rpcError }, reqId);
    } else {
      sendMcpJson(res, 200, { jsonrpc: '2.0', id, result: outcome.result }, reqId);
    }
    return true;
  }

  sendMcpJson(
    res,
    200,
    { jsonrpc: '2.0', id, error: { code: -32601, message: `Method not found: ${method}` } },
    reqId,
  );
  return true;
}
