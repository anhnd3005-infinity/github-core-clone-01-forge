import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { join } from 'node:path';

const rawEnvUrl = process.env.MAKEFUN_ENV_URL;
if (!rawEnvUrl || rawEnvUrl.trim() === '') {
  process.stderr.write('MAKEFUN_ENV_URL is required\n');
  process.exit(1);
}

const baseDir = import.meta.dirname;
const binName =
  process.platform === 'linux' && process.arch === 'x64'
    ? 'forge-linux-amd64'
    : 'forge-darwin';
const binPath = join(baseDir, binName);

if (!existsSync(binPath)) {
  process.stderr.write(`Missing forge binary: ${binPath}\n`);
  process.exit(1);
}

const child = spawn(binPath, [], {
  stdio: 'inherit',
  env: {
    ...process.env,
    MAKEFUN_ENV_URL: rawEnvUrl.trim(),
  },
});

const forwardSignal = (sig: NodeJS.Signals): void => {
  if (!child.killed) {
    child.kill(sig);
  }
};

process.once('SIGTERM', () => forwardSignal('SIGTERM'));
process.once('SIGINT', () => forwardSignal('SIGINT'));

child.once('error', (err) => {
  process.stderr.write(`${err.message}\n`);
  process.exit(1);
});

child.once('exit', (code, signal) => {
  if (signal) {
    process.exit(1);
  }
  process.exit(code ?? 1);
});
