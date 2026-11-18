// The entrypoint: a thin launcher. It spawns the ./forge binary next to
// this file with stdio and environment inherited, waits for it, and exits
// with the child's exit code. It opens no port and proxies no request: the
// Go binary owns the whole forge (lifecycle, git smart HTTP, REST,
// GraphQL, MCP) in one process.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const forge = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(forge, [], { stdio: 'inherit', env: process.env });

child.on('error', (err: Error) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  process.exit(signal ? 1 : (code ?? 1));
});
