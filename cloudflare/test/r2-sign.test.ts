import assert from 'node:assert/strict';
import { test } from 'node:test';
import { encodeKey, presignGet, signedObjectURL, signedURLConfig, uriEncode } from '../src/r2-sign';
import type { Env } from '../src/types';

const base = {
  accountId: 'bf2b0e6b408c36dce92d77b452b1d5fa',
  accessKeyId: 'AKIDEXAMPLE',
  secretAccessKey: 'secret-example',
  bucket: 'game-stop-saas-packages',
  now: new Date('2026-10-12T07:30:00Z'),
};

test('uri encoding follows RFC 3986 and encodes separators', () => {
  assert.equal(uriEncode('abcXYZ012-._~'), 'abcXYZ012-._~');
  assert.equal(uriEncode('/'), '%2F');
  assert.equal(uriEncode('a b'), 'a%20b');
  assert.equal(uriEncode('游戏'), '%E6%B8%B8%E6%88%8F');
  assert.equal(uriEncode('*'), '%2A');
  assert.equal(uriEncode('~'), '~');
});

test('object keys keep their path separators but encode each segment', () => {
  assert.equal(encodeKey('games/demo/4.7-ab.pck'), 'games/demo/4.7-ab.pck');
  assert.equal(encodeKey('assets/a b/cover.png'), 'assets/a%20b/cover.png');
  assert.equal(encodeKey('games/../x'), 'games/..%2Fx'.replace('%2F', '/'));
});

test('presigned URL carries the required SigV4 query parameters', async () => {
  const url = new URL(await presignGet({ ...base, key: 'games/demo/game.pck', expiresIn: 300 }));
  assert.equal(url.protocol, 'https:');
  assert.equal(url.host, `${base.accountId}.r2.cloudflarestorage.com`);
  assert.equal(url.pathname, `/${base.bucket}/games/demo/game.pck`);
  assert.equal(url.searchParams.get('X-Amz-Algorithm'), 'AWS4-HMAC-SHA256');
  assert.equal(url.searchParams.get('X-Amz-Expires'), '300');
  assert.equal(url.searchParams.get('X-Amz-SignedHeaders'), 'host');
  assert.equal(url.searchParams.get('X-Amz-Date'), '20261012T073000Z');
  assert.equal(url.searchParams.get('X-Amz-Credential'), `AKIDEXAMPLE/20261012/auto/s3/aws4_request`);
  assert.match(url.searchParams.get('X-Amz-Signature') ?? '', /^[0-9a-f]{64}$/);
});

test('signature is deterministic for fixed inputs and changes with any input', async () => {
  const first = await presignGet({ ...base, key: 'games/demo/game.pck', expiresIn: 300 });
  const again = await presignGet({ ...base, key: 'games/demo/game.pck', expiresIn: 300 });
  assert.equal(first, again);
  const otherKey = await presignGet({ ...base, key: 'games/demo/other.pck', expiresIn: 300 });
  const otherSecret = await presignGet({ ...base, secretAccessKey: 'other', key: 'games/demo/game.pck', expiresIn: 300 });
  const otherTime = await presignGet({ ...base, now: new Date('2026-10-12T07:30:01Z'), key: 'games/demo/game.pck', expiresIn: 300 });
  const otherTTL = await presignGet({ ...base, key: 'games/demo/game.pck', expiresIn: 301 });
  for (const variant of [otherKey, otherSecret, otherTime, otherTTL]) assert.notEqual(variant, first);
});

test('signedURLConfig refuses incomplete or out-of-range settings', () => {
  const full = {
    R2_ACCOUNT_ID: 'a', R2_BUCKET: 'b', R2_ACCESS_KEY_ID: 'c', R2_SECRET_ACCESS_KEY: 'd', R2_URL_TTL: '300',
  } as unknown as Env;
  assert.ok(signedURLConfig(full));
  for (const key of ['R2_ACCOUNT_ID', 'R2_BUCKET', 'R2_ACCESS_KEY_ID', 'R2_SECRET_ACCESS_KEY'] as const) {
    assert.equal(signedURLConfig({ ...full, [key]: '' } as Env), null, `${key} must be required`);
  }
  assert.equal(signedURLConfig({ ...full, R2_URL_TTL: '10' } as Env), null);
  assert.equal(signedURLConfig({ ...full, R2_URL_TTL: '999999999' } as Env), null);
  assert.equal(signedURLConfig({ ...full, R2_URL_TTL: 'abc' } as Env), null);
});

test('signedObjectURL returns null without configuration and a URL with it', async () => {
  assert.equal(await signedObjectURL({} as Env, 'assets/a/cover.webp'), null);
  const url = await signedObjectURL({
    R2_ACCOUNT_ID: 'acct', R2_BUCKET: 'bucket', R2_ACCESS_KEY_ID: 'key', R2_SECRET_ACCESS_KEY: 'secret', R2_URL_TTL: '120',
  } as Env, 'assets/a/cover.webp');
  assert.ok(url?.startsWith('https://acct.r2.cloudflarestorage.com/bucket/assets/a/cover.webp?'));
  assert.equal(new URL(url!).searchParams.get('X-Amz-Expires'), '120');
});
