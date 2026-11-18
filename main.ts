// Entry point for the Git Forge (Go core): one process that serves the forge
// from the statically linked ./forge binary beside this file and, when the
// make.fun suite is running beside it, hands the suite the port it picked
// and waits until the suite says it is done.
//
// The binary does the whole lifecycle itself (bind an ephemeral port, POST
// /register, poll GET /done, exit 0); this file only starts it with the
// same arguments and environment, forwards signals, and exits with its code.
// There are two ways it runs:
//
//   - In a trial pod, MAKEFUN_ENV_URL names the suite. The binary binds an
//     ephemeral port (port 0, so the kernel picks one), tells the suite
//     that port with a POST to /register, then polls GET /done until the
//     suite answers {"done": true} and exits 0. It never binds 8080,
//     where the suite itself listens.
//   - Anywhere else, it just listens on a fixed port — a number on the
//     command line, the PORT variable, or 3000 — and keeps serving
//     until the process is stopped, so the local runner can drive it.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const binary = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(binary, process.argv.slice(2), {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (err) => {
  process.stderr.write(`github-forge failed to start: ${err.message}\n`);
  process.exit(1);
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
