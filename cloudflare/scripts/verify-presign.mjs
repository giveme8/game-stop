#!/usr/bin/env node
/**
 * Verify the SigV4 presigner against the real R2 S3 endpoint and measure the
 * direct download path. Read-only: presigned GETs never modify an object.
 *
 * Usage:
 *   R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... \
 *   R2_BUCKET=game-stop-saas-packages KEY=games/.../x.pck \
 *   node scripts/verify-presign.mjs
 *
 * Secrets are read from the environment and never printed.
 */
import { createHash, createHmac } from 'node:crypto';

const { R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET, KEY } = process.env;
for (const [name, value] of Object.entries({ R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET, KEY })) {
  if (!value) throw new Error(`${name} is required`);
}

const hex = bytes => Buffer.from(bytes).toString('hex');
const sha256hex = value => createHash('sha256').update(value).digest('hex');
const hmac = (key, value) => createHmac('sha256', key).update(value).digest();
const uriEncode = value => Array.from(Buffer.from(value), byte => {
  const char = String.fromCharCode(byte);
  return /[A-Za-z0-9\-._~]/.test(char) ? char : `%${byte.toString(16).toUpperCase().padStart(2, '0')}`;
}).join('');

const host = `${R2_ACCOUNT_ID}.r2.cloudflarestorage.com`;
const now = new Date();
const amzDate = now.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '');
const dateStamp = amzDate.slice(0, 8);
const scope = `${dateStamp}/auto/s3/aws4_request`;
const canonicalUri = `/${uriEncode(R2_BUCKET)}/${KEY.split('/').map(uriEncode).join('/')}`;
const query = [
  ['X-Amz-Algorithm', 'AWS4-HMAC-SHA256'],
  ['X-Amz-Credential', `${R2_ACCESS_KEY_ID}/${scope}`],
  ['X-Amz-Date', amzDate],
  ['X-Amz-Expires', '300'],
  ['X-Amz-SignedHeaders', 'host'],
].map(([n, v]) => `${uriEncode(n)}=${uriEncode(v)}`).sort().join('&');
const canonicalRequest = ['GET', canonicalUri, query, `host:${host}`, '', 'host', 'UNSIGNED-PAYLOAD'].join('\n');
const stringToSign = ['AWS4-HMAC-SHA256', amzDate, scope, sha256hex(canonicalRequest)].join('\n');
const signingKey = hmac(hmac(hmac(hmac(`AWS4${R2_SECRET_ACCESS_KEY}`, dateStamp), 'auto'), 's3'), 'aws4_request');
const url = `https://${host}${canonicalUri}?${query}&X-Amz-Signature=${hex(hmac(signingKey, stringToSign))}`;

console.log(`presigned URL generated for key length ${KEY.length} (not printed)`);
if (process.env.PRESIGN_OUT) {
  const { writeFileSync } = await import('node:fs');
  // The URL is a temporary bearer capability: hand it to the fetcher through a
  // mode-600 file instead of the terminal, command line or logs.
  writeFileSync(process.env.PRESIGN_OUT, `url = "${url}"\n`, { mode: 0o600 });
  console.log(`url written to ${process.env.PRESIGN_OUT} for the fetcher`);
  process.exit(0);
}
const started = Date.now();
const response = await fetch(url, { redirect: 'error' });
if (!response.ok) {
  const body = await response.text();
  throw new Error(`presigned GET failed: ${response.status} ${body.slice(0, 300)}`);
}
const digest = createHash('sha256');
let size = 0;
let firstByteAt = 0;
for await (const chunk of response.body) {
  if (!firstByteAt) firstByteAt = Date.now() - started;
  digest.update(chunk);
  size += chunk.length;
}
const elapsed = (Date.now() - started) / 1000;
console.log(JSON.stringify({
  status: response.status,
  contentType: response.headers.get('content-type'),
  bytes: size,
  declaredLength: response.headers.get('content-length'),
  firstByteMs: firstByteAt,
  seconds: Number(elapsed.toFixed(2)),
  KiBperSecond: Math.round(size / 1024 / elapsed),
  sha256: digest.digest('hex'),
}, null, 2));
