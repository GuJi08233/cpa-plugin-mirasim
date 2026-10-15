#!/usr/bin/env node
// Run on the registered Mirasim space host. Only the scoped application token
// is exported to CPA; the machine signing key stays outside the container.
import { createHash, createPrivateKey, createPublicKey, randomBytes, sign } from 'node:crypto';
import { promises as fs } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import { setTimeout as delay } from 'node:timers/promises';

const relayDefault = 'https://relay.mirasim.ai';
const idPattern = /^[A-Za-z0-9_-]{1,128}$/;

export function machineAuthorization({ nodeId, path, body, identity, now = Date.now(), nonce = randomBytes(16).toString('base64url') }) {
  const seed = Buffer.from(identity.keys?.signPriv ?? '', 'base64url');
  if (seed.length !== 32) throw new Error('Invalid Mirasim machine signing key');
  const key = createPrivateKey({
    key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), seed]),
    format: 'der', type: 'pkcs8',
  });
  const publicKey = createPublicKey(key).export({ format: 'jwk' }).x;
  if (publicKey !== identity.keys?.sign) throw new Error('Mirasim machine key does not match its public identity');
  const message = ['mirasim-machine-call-v1', 'POST', path, nodeId, String(now), nonce,
    createHash('sha256').update(body).digest('hex')].join('\n');
  const signature = sign(null, Buffer.from(message), key).toString('base64url');
  return `MirasimMachine v1.${nodeId}.${now}.${nonce}.${signature}`;
}

export function validateCapability(token, nodeId, appId, now = Date.now()) {
  let claims;
  try {
    if (typeof token !== 'string' || token.length > 65536 || token.split('.').length !== 3) throw new Error();
    claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8'));
  } catch { throw new Error('The platform returned an invalid application capability'); }
  if (claims.typ !== 'gateway-turn' || claims.aud !== nodeId || claims.app?.node !== nodeId ||
      claims.app?.app !== appId || !/^ig_[0-9a-f]{12}$/.test(claims.app?.integration ?? '') ||
      claims.sub !== `ig-${claims.app.integration.slice(3)}` || claims.pay_space !== nodeId ||
      claims.pay_as !== `app:${appId}` || claims.pay_only !== true ||
      !Number.isSafeInteger(claims.exp) || claims.exp * 1000 <= now + 120000) {
    throw new Error('The application capability has the wrong identity, billing scope, or expiry');
  }
  return claims.exp * 1000;
}

export async function writeCapability(output, capability) {
  await fs.mkdir(dirname(output), { recursive: true, mode: 0o700 });
  const temporary = `${output}.${process.pid}.${randomBytes(8).toString('hex')}.tmp`;
  let handle;
  try {
    handle = await fs.open(temporary, 'wx', 0o600);
    await handle.writeFile(JSON.stringify(capability) + '\n');
    await handle.sync();
    await handle.close(); handle = null;
    await fs.rename(temporary, output);
  } finally {
    await handle?.close();
    await fs.unlink(temporary).catch(() => {});
  }
}

export async function exportCapability({ nodeId, appId, identityFile, output, relayURL = relayDefault, fetchImpl = fetch }) {
  if (!idPattern.test(nodeId) || !idPattern.test(appId)) throw new Error('A valid node ID and application ID are required');
  const relay = new URL(relayURL);
  if (relay.protocol !== 'https:' || relay.username || relay.password || relay.search || relay.hash || relay.pathname !== '/') {
    throw new Error('The relay URL must be an HTTPS origin');
  }
  const stat = await fs.stat(identityFile);
  if (stat.size > 128 * 1024 || !stat.isFile() || (process.platform !== 'win32' && (stat.mode & 0o077))) {
    throw new Error('The machine identity must be a private file (mode 600)');
  }
  const identity = JSON.parse(await fs.readFile(identityFile, 'utf8'));
  const path = `/v2/machines/${encodeURIComponent(nodeId)}/apps/turn-capability`;
  const body = JSON.stringify({ app: appId, session: `cpa-app-${appId}` });
  const authorization = machineAuthorization({ nodeId, path, body, identity });
  const response = await fetchImpl(relay.origin + path, {
    method: 'POST', headers: { authorization, 'content-type': 'application/json' },
    body, redirect: 'error', signal: AbortSignal.timeout(20000),
  });
  // Never include response bodies, capabilities, or signing material in errors.
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    const code = typeof data.error === 'string' && /^[a-z_-]{1,50}$/.test(data.error) ? data.error : 'refused';
    throw new Error(`Space application authorization failed: HTTP ${response.status} (${code})`);
  }
  const raw = await response.text();
  if (raw.length > 128 * 1024) throw new Error('Oversized capability response');
  const result = JSON.parse(raw);
  const expiresAt = validateCapability(result.token, nodeId, appId);
  await writeCapability(output, { nodeId, appId, token: result.token });
  return expiresAt;
}

async function main() {
  const { values } = parseArgs({ options: {
    'node-id': { type: 'string' }, 'app-id': { type: 'string' },
    'identity-file': { type: 'string' }, output: { type: 'string' },
    'relay-url': { type: 'string', default: relayDefault },
    watch: { type: 'boolean', default: false }, interval: { type: 'string', default: '240' },
    help: { type: 'boolean', default: false },
  } });
  if (values.help) {
    console.log('node scripts/export_space_capability.mjs --node-id NODE --app-id APP --identity-file /private/instance.json --output /private/space.json [--watch] [--interval 240]');
    return;
  }
  if (!values['node-id'] || !values['app-id'] || !values['identity-file'] || !values.output) {
    throw new Error('Required: --node-id, --app-id, --identity-file, --output');
  }
  const interval = Number(values.interval);
  if (!Number.isFinite(interval) || interval < 30 || interval > 300) throw new Error('Interval must be 30–300 seconds');
  const controller = new AbortController();
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => controller.abort());
  do {
    let wait = interval * 1000;
    try {
      const expiresAt = await exportCapability({ nodeId: values['node-id'], appId: values['app-id'],
        identityFile: resolve(values['identity-file']), output: resolve(values.output), relayURL: values['relay-url'] });
      console.log(`Space application capability updated; expires ${new Date(expiresAt).toISOString()}`);
      wait = Math.min(wait, Math.max(30000, expiresAt - Date.now() - 150000));
    } catch (error) {
      // Underlying filesystem / network errors may contain local paths but
      // never credential values. JSON parse errors can quote input: redact them.
      const message = error instanceof SyntaxError ? 'Invalid authorization JSON' : error.message;
      if (!values.watch) throw new Error(message);
      console.error(message); wait = 30000;
    }
    if (!values.watch || controller.signal.aborted) break;
    await delay(wait, undefined, { signal: controller.signal }).catch(() => {});
  } while (!controller.signal.aborted);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => {
    console.error(error instanceof SyntaxError ? 'Invalid authorization JSON' : error.message);
    process.exitCode = 1;
  });
}
