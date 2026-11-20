import { execFileSync, spawn } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';

const rawEnvUrl = process.env.MAKEFUN_ENV_URL;
if (!rawEnvUrl || rawEnvUrl.trim() === '') {
  process.stderr.write('MAKEFUN_ENV_URL is required\n');
  process.exit(1);
}

const baseDir = import.meta.dirname;
const isLinuxX64 = process.platform === 'linux' && process.arch === 'x64';
let binPath = join(baseDir, isLinuxX64 ? 'forge-linux-amd64' : 'forge-darwin');

if (!existsSync(binPath)) {
  if (!isLinuxX64) {
    const tmpBin = `/tmp/forge-darwin-${process.pid}`;
    execFileSync('go', ['build', '-o', tmpBin, './cmd/forge'], {
      cwd: baseDir,
      stdio: ['ignore', 'ignore', 'pipe'],
      env: {
        ...process.env,
        GOCACHE: process.env.GOCACHE || '/tmp/go-build-cache',
      },
    });
    binPath = tmpBin;
  } else {
    process.stderr.write(`Missing forge binary: ${binPath}\n`);
    process.exit(1);
  }
}

const launch = (execPath: string) =>
  spawn(execPath, [], {
    stdio: 'inherit',
    env: {
      ...process.env,
      MAKEFUN_ENV_URL: rawEnvUrl.trim(),
    },
  });

let child = launch(binPath);

const bindChild = (proc: ReturnType<typeof spawn>, allowFallback: boolean): void => {
  proc.once('error', (err: NodeJS.ErrnoException) => {
    if (allowFallback && err.code === 'EACCES') {
      const tmpExec = `/tmp/forge-bin-${process.pid}`;
      copyFileSync(binPath, tmpExec);
      chmodSync(tmpExec, 0o755);
      child = launch(tmpExec);
      bindChild(child, false);
      return;
    }
    process.stderr.write(`${err.message}\n`);
    process.exit(1);
  });

  proc.once('exit', (code, signal) => {
    if (signal) {
      process.exit(1);
    }
    process.exit(code ?? 1);
  });
};

bindChild(child, isLinuxX64);

const forwardSignal = (sig: NodeJS.Signals): void => {
  if (!child.killed) {
    child.kill(sig);
  }
};

process.once('SIGTERM', () => forwardSignal('SIGTERM'));
process.once('SIGINT', () => forwardSignal('SIGINT'));
