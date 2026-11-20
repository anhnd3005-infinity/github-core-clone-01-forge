import type { AddressInfo } from 'node:net';
import { createForgeServer } from './src/server.ts';

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

async function registerPort(envUrl: string, port: number): Promise<void> {
  let backoffMs = 50;
  for (;;) {
    try {
      const response = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
      });
      if (response.ok) return;
      if (response.status === 409) return;
    } catch {
      // Env listener may still be binding; retry briefly.
    }
    await delay(backoffMs);
    backoffMs = Math.min(500, backoffMs * 2);
  }
}

async function waitUntilDone(envUrl: string, shouldStop: () => boolean): Promise<void> {
  let backoffMs = 25;
  while (!shouldStop()) {
    try {
      const response = await fetch(`${envUrl}/done`);
      if (response.ok) {
        const payload = (await response.json()) as { done?: boolean };
        if (payload.done === true) return;
      }
    } catch {
      // Transient poll failure; backoff briefly and continue.
    }
    await delay(backoffMs);
    backoffMs = Math.min(250, Math.round(backoffMs * 1.5));
  }
}

async function run(): Promise<void> {
  const rawEnvUrl = process.env.MAKEFUN_ENV_URL;
  if (!rawEnvUrl || rawEnvUrl.trim() === '') {
    process.stderr.write('MAKEFUN_ENV_URL is required\n');
    process.exit(1);
  }
  const envUrl = rawEnvUrl.replace(/\/+$/, '');

  const server = createForgeServer({
    token: process.env.MAKEFUN_SHOP_TOKEN,
    viewerLogin: process.env.MAKEFUN_FORGE_LOGIN ?? 'adaiversen',
    trialNow: process.env.MAKEFUN_TRIAL_NOW,
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '0.0.0.0', () => resolve());
  });

  const addr = server.address() as AddressInfo | null;
  if (!addr || typeof addr.port !== 'number') {
    server.close();
    process.exit(1);
  }

  let stopping = false;
  const onSignal = (): void => {
    stopping = true;
  };
  process.on('SIGTERM', onSignal);
  process.on('SIGINT', onSignal);

  await registerPort(envUrl, addr.port);
  await waitUntilDone(envUrl, () => stopping);

  await new Promise<void>((resolve) => {
    server.close(() => resolve());
    server.closeAllConnections();
  });
  process.exit(0);
}

run().catch((err) => {
  process.stderr.write(`${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
