import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, verify } from 'node:crypto';
import { promises as fs } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { machineAuthorization, validateCapability, writeCapability, exportCapability } from './export_space_capability.mjs';

function fixture() {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  const identity = { keys: { sign: publicKey.export({ format: 'jwk' }).x, signPriv: privateKey.export({ format: 'jwk' }).d } };
  return { publicKey, identity };
}
function token(change = {}) {
  const claims = { typ: 'gateway-turn', sub: 'ig-0123456789ab', aud: 'pc-test',
    app: { node: 'pc-test', app: 'cpa', integration: 'ig_0123456789ab' },
    pay_space: 'pc-test', pay_as: 'app:cpa', pay_only: true, exp: Math.floor(Date.now() / 1000) + 600, ...change };
  return `header.${Buffer.from(JSON.stringify(claims)).toString('base64url')}.signature`;
}

test('machine signature binds node, endpoint, timestamp, nonce and exact body', () => {
  const { publicKey, identity } = fixture();
  const path = '/v2/machines/pc-test/apps/turn-capability';
  const body = '{"app":"cpa","session":"test"}';
  const header = machineAuthorization({ nodeId: 'pc-test', path, body, identity, now: 1234, nonce: 'nonce' });
  const signature = Buffer.from(header.split('.').at(-1), 'base64url');
  const message = ['mirasim-machine-call-v1', 'POST', path, 'pc-test', '1234', 'nonce', createHash('sha256').update(body).digest('hex')].join('\n');
  assert.ok(verify(null, Buffer.from(message), publicKey, signature));
  assert.ok(!verify(null, Buffer.from(message + 'modified'), publicKey, signature));
  assert.throws(() => machineAuthorization({ nodeId: 'pc-test', path, body, identity: { keys: { ...identity.keys, sign: 'wrong' } } }));
});

test('export rejects capabilities with personal fallback, wrong app or expired scope', () => {
  assert.ok(validateCapability(token(), 'pc-test', 'cpa') > Date.now());
  for (const change of [{ pay_only: false }, { aud: 'other' }, { pay_space: 'other' },
    { pay_as: 'personal' }, { exp: 1 }, { sub: 'usr-test' }, { app: { node: 'pc-test', app: 'other' } }]) {
    assert.throws(() => validateCapability(token(change), 'pc-test', 'cpa'));
  }
});

test('atomic export replaces old token with mode 600 and no temporary files', async () => {
  const dir = await fs.mkdtemp(join(tmpdir(), 'mirasim-space-'));
  try {
    const file = join(dir, 'space.json');
    await writeCapability(file, { token: 'old' });
    await writeCapability(file, { nodeId: 'pc-test', appId: 'cpa', token: 'new' });
    assert.equal(JSON.parse(await fs.readFile(file)).token, 'new');
    assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
    assert.deepEqual(await fs.readdir(dir), ['space.json']);
  } finally { await fs.rm(dir, { recursive: true, force: true }); }
});

test('failed platform issuance preserves the last good file and redacts response secrets', async () => {
  const dir = await fs.mkdtemp(join(tmpdir(), 'mirasim-space-'));
  try {
    const identityFile = join(dir, 'identity.json');
    const output = join(dir, 'space.json');
    await fs.writeFile(identityFile, JSON.stringify(fixture().identity), { mode: 0o600 });
    await writeCapability(output, { token: 'old' });
    const fetchImpl = async () => new Response(JSON.stringify({ error: 'private-token-that-must-stay-secret!!!' }), { status: 403 });
    await assert.rejects(exportCapability({ nodeId: 'pc-test', appId: 'cpa', identityFile, output, fetchImpl }),
      error => error.message === 'Space application authorization failed: HTTP 403 (refused)');
    assert.equal(JSON.parse(await fs.readFile(output)).token, 'old');
  } finally { await fs.rm(dir, { recursive: true, force: true }); }
});
