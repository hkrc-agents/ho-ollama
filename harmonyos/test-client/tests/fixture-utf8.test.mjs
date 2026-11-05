import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('controlled UTF8 fixture preserves exact bytes on both pipes', async () => {
  const temp = mkdtempSync(join(tmpdir(), 'pico-client-utf8-'));
  try {
    const exe = join(temp, 'fixture');
    execFileSync('cc', ['-Wall', '-Wextra', '-Werror', 'harmonyos/test-client/entry/src/main/cpp/fixture.c', '-o', exe]);
    const child = spawn(exe, ['utf8']);
    const stdout=[],stderr=[];
    child.stdout.on('data', b=>stdout.push(b)); child.stderr.on('data', b=>stderr.push(b));
    const done = new Promise((resolve,reject)=>{child.on('error',reject);child.on('close',resolve)});
    child.stdin.end('\n\n\n\n');
    assert.equal(await done,0);
    assert.equal(Buffer.concat(stdout).toString('utf8'),'你好😀\n');
    assert.equal(Buffer.concat(stderr).toString('utf8'),'\x1b[?25l\x1b[1G警告😀\n\x1b[?25h');
  } finally { rmSync(temp,{recursive:true,force:true}); }
});
