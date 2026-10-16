#!/usr/bin/env node
// Run on the registered Mirasim space host. Only the scoped application token
// is exported to CPA; the machine signing key stays outside the container.
import { createHash, createPrivateKey, createPublicKey, randomBytes, sign } from 'node:crypto';
import { promises as fs } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';
import { setTimeout as delay } from 'node:timers/promises';
import { createServer } from 'node:http';
import { createConnection } from 'node:net';

const relayDefault = 'https://relay.mirasim.ai';
const idPattern = /^[A-Za-z0-9_-]{1,128}$/;

export function machineAuthorization({ nodeId, path, body, bodySha256, method = 'POST', identity, now = Date.now(), nonce = randomBytes(16).toString('base64url') }) {
  const seed = Buffer.from(identity.keys?.signPriv ?? '', 'base64url');
  if (seed.length !== 32) throw new Error('Invalid Mirasim machine signing key');
  const key = createPrivateKey({
    key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), seed]),
    format: 'der', type: 'pkcs8',
  });
  const publicKey = createPublicKey(key).export({ format: 'jwk' }).x;
  if (publicKey !== identity.keys?.sign) throw new Error('Mirasim machine key does not match its public identity');
  const digest = bodySha256 ?? createHash('sha256').update(body ?? '').digest('hex');
  if (!/^[a-f0-9]{64}$/.test(digest) || (body !== undefined && digest !== createHash('sha256').update(body).digest('hex'))) {
    throw new Error('Invalid request body digest');
  }
  const message = ['mirasim-machine-call-v1', method, path, nodeId, String(now), nonce, digest].join('\n');
  const signature = sign(null, Buffer.from(message), key).toString('base64url');
  return `MirasimMachine v1.${nodeId}.${now}.${nonce}.${signature}`;
}

async function readIdentity(identityFile) {
  const stat = await fs.stat(identityFile);
  if (stat.size > 128 * 1024 || !stat.isFile() || (process.platform !== 'win32' && (stat.mode & 0o077))) {
    throw new Error('The machine identity must be a private file (mode 600)');
  }
  return JSON.parse(await fs.readFile(identityFile, 'utf8'));
}

const signerPaths = new Set(['/v2/gateway/turn-capability/renew',
  ...['/v1/responses', '/v1/responses/compact', '/v1/messages', '/v1/messages/count_tokens',
    '/v1/images/generations', '/v1/images/edits', '/v1/alpha/search'].map(path => '/v2/gateway/model' + path)]);

// This private Unix socket is a restricted signing service, not a model proxy.
// Only node/app, an allowlisted route, and a body hash cross the socket. The
// capability is always chosen from our own atomic export, never from callers.
export async function startSigner({ nodeId, appId, identityFile, capabilityFile, socketPath }) {
  if (!idPattern.test(nodeId) || !idPattern.test(appId)) throw new Error('Invalid signer application binding');
  await fs.mkdir(dirname(socketPath), { recursive: true, mode: 0o700 });
  const previous = await fs.lstat(socketPath).catch(error => { if (error.code !== 'ENOENT') throw error; return null; });
  if (previous) {
    if (!previous.isSocket() || (process.getuid && previous.uid !== process.getuid())) throw new Error('Signer socket path is occupied');
    const live = await new Promise((resolve, reject) => {
      const conn = createConnection(socketPath);
      conn.once('connect', () => { conn.destroy(); resolve(true); });
      conn.once('error', error => { conn.destroy(); if (['ECONNREFUSED', 'ENOENT'].includes(error.code)) resolve(false); else reject(error); });
      conn.setTimeout(1000, () => { conn.destroy(); resolve(true); });
    });
    if (live) throw new Error('A signer already listens on this socket');
    await fs.unlink(socketPath);
  }
  const server = createServer(async (req, res) => {
    const reply = (status, data) => { res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' }); res.end(JSON.stringify(data)); };
    if (req.method !== 'POST' || req.url !== '/sign') { reply(404, { error: 'unknown_route' }); return; }
    try {
      let length = 0; const chunks = [];
      for await (const chunk of req) {
        length += chunk.length;
        if (length > 8192) { reply(413, { error: 'request_too_large' }); return; }
        chunks.push(chunk);
      }
      const request = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      if (request.nodeId !== nodeId || request.appId !== appId || request.method !== 'POST' ||
          !signerPaths.has(request.path) || !/^[a-f0-9]{64}$/.test(request.bodySha256 ?? '')) {
        reply(403, { error: 'scope_mismatch' }); return;
      }
      const stat = await fs.stat(capabilityFile);
      if (!stat.isFile() || stat.size > 128 * 1024 || (stat.mode & 0o077)) throw new Error('Invalid capability file');
      const capability = JSON.parse(await fs.readFile(capabilityFile, 'utf8'));
      if (capability.nodeId !== nodeId || capability.appId !== appId || !capability.token?.startsWith('gtr.')) throw new Error('Invalid capability binding');
      validateCapability(capability.token, nodeId, appId, Date.now(), { nodeId, appId });
      const subject = JSON.parse(Buffer.from(capability.token.split('.')[1], 'base64url')).sub;
      if (capability.subject !== subject) throw new Error('Invalid capability subject');
      const authorization = machineAuthorization({ nodeId, method: request.method, path: request.path + '#' + capability.token,
        bodySha256: request.bodySha256, identity: await readIdentity(identityFile) });
      reply(200, { nodeId, appId, subject, token: capability.token, authorization });
    } catch {
      // Neither malformed JSON nor platform credential material enters logs.
      reply(503, { error: 'space_signer_unavailable' });
    }
  });
  server.requestTimeout = 5000; server.headersTimeout = 5000;
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(socketPath, resolve); });
  await fs.chmod(socketPath, 0o600);
  const installed = await fs.lstat(socketPath);
  return { close: async () => {
    await new Promise(resolve => { server.close(resolve); server.closeAllConnections(); });
    const current = await fs.lstat(socketPath).catch(() => null);
    if (current?.ino === installed.ino && current?.dev === installed.dev) await fs.unlink(socketPath);
  } };
}

export function validateCapability(token, nodeId, appId, now = Date.now(), issuance = null) {
  let claims;
  try {
    if (typeof token !== 'string' || token.length > 65536 || token.split('.').length !== 3) throw new Error();
    claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8'));
  } catch { throw new Error('The platform returned an invalid application capability'); }
  if (!Number.isSafeInteger(claims.exp) || claims.exp * 1000 <= now + 120000) {
    throw new Error('The application capability expires too soon');
  }
  if (token.startsWith('gtr.')) {
    // Scopes of these reference tokens are stored by the platform. Accept
    // only tokens returned directly by the authenticated app mint for this
    // exact node/application; a pasted opaque token has no such provenance.
    if (issuance?.nodeId !== nodeId || issuance?.appId !== appId ||
        !/^ig-[0-9a-f]{12}$/.test(claims.sub ?? '') ||
        !/^[A-Za-z0-9_-]{24,256}$/.test(token.split('.')[2])) {
      throw new Error('The opaque application capability has no matching authenticated issuance');
    }
    return claims.exp * 1000;
  }
  if (claims.typ !== 'gateway-turn' || claims.aud !== nodeId || claims.app?.node !== nodeId ||
      claims.app?.app !== appId || !/^ig_[0-9a-f]{12}$/.test(claims.app?.integration ?? '') ||
      claims.sub !== `ig-${claims.app.integration.slice(3)}` || claims.pay_space !== nodeId ||
      claims.pay_as !== `app:${appId}` || claims.pay_only !== true) {
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
  const identity = await readIdentity(identityFile);
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
  const expiresAt = validateCapability(result.token, nodeId, appId, Date.now(), { nodeId, appId });
  const subject = JSON.parse(Buffer.from(result.token.split('.')[1], 'base64url').toString('utf8')).sub;
  await writeCapability(output, { nodeId, appId, subject, token: result.token });
  return expiresAt;
}

async function main() {
  const { values } = parseArgs({ options: {
    'node-id': { type: 'string' }, 'app-id': { type: 'string' },
    'identity-file': { type: 'string' }, output: { type: 'string' },
    'relay-url': { type: 'string', default: relayDefault },
    watch: { type: 'boolean', default: false }, interval: { type: 'string', default: '240' },
    'signer-socket': { type: 'string' },
    help: { type: 'boolean', default: false },
  } });
  if (values.help) {
    console.log('node scripts/export_space_capability.mjs --node-id NODE --app-id APP --identity-file /private/instance.json --output /private/space.json [--watch] [--interval 240] [--signer-socket /private/signer.sock]');
    return;
  }
  if (!values['node-id'] || !values['app-id'] || !values['identity-file'] || !values.output) {
    throw new Error('Required: --node-id, --app-id, --identity-file, --output');
  }
  const interval = Number(values.interval);
  if (!Number.isFinite(interval) || interval < 30 || interval > 300) throw new Error('Interval must be 30–300 seconds');
  const controller = new AbortController();
  if (values['signer-socket'] && !values.watch) throw new Error('--signer-socket requires --watch');
  const signer = values['signer-socket'] ? await startSigner({ nodeId: values['node-id'], appId: values['app-id'],
    identityFile: resolve(values['identity-file']), capabilityFile: resolve(values.output), socketPath: resolve(values['signer-socket']) }) : null;
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => controller.abort());
  try {
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
  } finally { await signer?.close(); }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => {
    console.error(error instanceof SyntaxError ? 'Invalid authorization JSON' : error.message);
    process.exitCode = 1;
  });
}
