#!/usr/bin/env node
import { open } from 'node:fs/promises';
import { constants } from 'node:fs';
import { inspectManifest, parseManifest, scaffold } from './index.mjs';
import { MAX_PAYLOAD_BYTES } from './json.mjs';

// No network, execution, credential loading or filesystem writes. Scaffolds
// are emitted as a file map for human review, never loaded as executable code.
async function readBounded(path) {
  const file = await open(path, constants.O_RDONLY | constants.O_NONBLOCK);
  try {
    if (!(await file.stat()).isFile()) throw new Error('manifest must be a regular file');
    const bytes = Buffer.alloc(MAX_PAYLOAD_BYTES + 1);
    let used = 0;
    while (used < bytes.length) {
      const { bytesRead } = await file.read(bytes, used, bytes.length - used, null);
      if (!bytesRead) break;
      used += bytesRead;
    }
    if (used > MAX_PAYLOAD_BYTES) throw new Error('manifest exceeds 64 KiB');
    return bytes.subarray(0, used);
  } finally { await file.close(); }
}

try {
  const [command, value, ...extra] = process.argv.slice(2);
  if (!value || extra.length || !['inspect', 'scaffold'].includes(command)) {
    throw new Error('usage: node src/cli.mjs inspect MANIFEST.json | scaffold ADAPTER-ID');
  }
  if (command === 'scaffold') {
    console.log(JSON.stringify(scaffold(value), null, 2));
  } else {
    const report = inspectManifest(parseManifest(await readBounded(value)));
    console.log(JSON.stringify(report, null, 2));
    if (report.declaration === 'BLOCKED') process.exitCode = 1;
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 2;
}
