import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { after, before, test } from 'node:test';
import { Miniflare } from 'miniflare';
import { handleLibrary } from '../src/library';
import { HttpError, type Env, type GameRow, type Identity } from '../src/types';

// Real local D1 and R2, synthetic PCKs only; no credentials or hosted games.
let mf: Miniflare;
let DB: D1Database;
let bucket: R2Bucket;
let serial = 0;
before(async () => {
  mf = new Miniflare({
    modules: true, compatibilityDate: '2026-08-06',
    script: 'export default { fetch() { return new Response("local upload tests") } }',
    d1Databases: ['DB'], r2Buckets: ['PACKAGES'],
  });
  DB = await mf.getD1Database('DB') as unknown as D1Database;
  const nativeBucket = await mf.getR2Bucket('PACKAGES') as unknown as R2Bucket;
  // The Node RPC proxy implements `in` by fetching the property. Fetching body
  // transfers its stream out of workerd, so a later remote arrayBuffer() fails.
  // Keep property membership non-consuming, as it is on a native R2 binding.
  // All actual R2 reads, multipart writes, validation and SQL remain real.
  bucket = new Proxy(nativeBucket, { get(target, property) {
    if (property === 'put') return async (key: string, value: Parameters<R2Bucket['put']>[1], options?: R2PutOptions) => {
      // Crossing Node RPC also drops the R2 body's native known-length marker.
      // Materialize only this test-side copy; production keeps its native stream.
      // This harness verifies content/atomicity, not the isolate memory budget.
      if (value instanceof ReadableStream) value = await new Response(value).arrayBuffer();
      return target.put(key, value, options);
    };
    if (property === 'get') return async (...args: Parameters<R2Bucket['get']>) => {
      const object = await target.get(...args);
      return object && new Proxy(object, {
        has(value, name) { return name === 'body' || Reflect.has(value, name); },
        get(value, name) {
          const member = Reflect.get(value, name); return typeof member === 'function' ? member.bind(value) : member;
        },
      });
    };
    const value = Reflect.get(target, property); return typeof value === 'function' ? value.bind(target) : value;
  } });
  const schema = readFileSync(new URL('../migrations/0001_native.sql', import.meta.url), 'utf8');
  await DB.batch(schema.split(';').map(s => s.trim()).filter(Boolean).map(s => DB.prepare(s)));
});
after(async () => { await mf?.dispose(); });

const hash = (data: Uint8Array) => createHash('sha256').update(data).digest('hex');
const MiB = 1024 * 1024;
// Independent format-4 writer: one allowed data file, valid MD5 and 4.7 header.
function pck(size: number): Uint8Array {
  const name = new TextEncoder().encode('res://payload.dat');
  const directorySize = 4 + 4 + name.length + 36;
  const raw = new Uint8Array(size);
  const view = new DataView(raw.buffer);
  const directory = size - directorySize;
  raw.set(new TextEncoder().encode('GDPC'));
  view.setUint32(4, 4, true); view.setUint32(8, 4, true); view.setUint32(12, 7, true);
  view.setUint32(20, 2, true); view.setBigUint64(24, 112n, true); view.setBigUint64(32, BigInt(directory), true);
  for (let i = 112; i < directory; i++) raw[i] = (i * 7 + (i >>> 8)) & 255;
  view.setUint32(directory, 1, true); view.setUint32(directory + 4, name.length, true);
  raw.set(name, directory + 8);
  const record = directory + 8 + name.length;
  view.setBigUint64(record, 0n, true); view.setBigUint64(record + 8, BigInt(directory - 112), true);
  raw.set(createHash('md5').update(raw.subarray(112, directory)).digest(), record + 16);
  return raw;
}

async function fixture() {
  const id = `upload-${++serial}`, now = new Date().toISOString();
  const user = await DB.prepare("INSERT INTO users (name,status,created_at,updated_at) VALUES (?,'active',?,?) RETURNING id")
    .bind(id, now, now).first<{ id: number }>();
  const identity: Identity = { userId: user!.id, name: id, status: 'active', kind: 'api', hash: '' };
  const env = { DB, PACKAGES: bucket, PUBLIC_BASE_URL: 'https://upload.example.test', MAX_PACKAGE_BYTES: String(95 * MiB) } as Env;
  return { id, identity, env };
}

function request(id: string, raw: Uint8Array, pattern: number[], options: { failAfter?: number; declared?: boolean } = {}) {
  let offset = 0, step = 0, cancelled = false;
  const body = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (options.failAfter !== undefined && offset >= options.failAfter) throw new Error('synthetic source failure');
      if (offset === raw.length) { controller.close(); return; }
      const end = Math.min(raw.length, offset + pattern[step++ % pattern.length]);
      controller.enqueue(raw.subarray(offset, end)); offset = end;
    },
    cancel() { cancelled = true; },
  });
  const headers: Record<string, string> = {
    'Content-Type': 'application/octet-stream',
    'X-Game-Metadata': Buffer.from(JSON.stringify({ title: 'Upload fixture', engine: { name: 'godot', version: '4.7' } })).toString('base64'),
  };
  if (options.declared !== false) headers['Content-Length'] = String(raw.length);
  return { value: new Request(`https://upload.example.test/api/v1/games/${id}`, {
    method: 'PUT', headers, body, duplex: 'half',
  } as RequestInit), wasCancelled: () => cancelled };
}

const successCases = [
  { name: 'irregular chunks crossing multiple parts', size: 17 * MiB + 37, pattern: [65536, 19500, 131072, 317] },
  { name: 'below one part', size: 8 * MiB - 1, pattern: [65536, 19500, 317] },
  { name: 'exact multiple with no empty tail part', size: 16 * MiB, pattern: [65536, 19500, 317] },
  { name: 'one-byte tail after a full part', size: 8 * MiB + 1, pattern: [3 * MiB + 5, 65536] },
  { name: 'one incoming chunk larger than two parts', size: 17 * MiB + 37, pattern: [20 * MiB] },
  { name: 'split magic and empty incoming chunks', size: 8 * MiB + 13, pattern: [1, 0, 2, 1, 65536, 317] },
  { name: '95 MiB configured limit with irregular chunks', size: 95 * MiB, pattern: [65536, 19500, 131072, 317] },
];
for (const c of successCases) test(`bare PCK upload preserves bytes: ${c.name}`, async () => {
  const f = await fixture(), raw = pck(c.size);
  const req = request(f.id, raw, c.pattern);
  const response = await handleLibrary(req.value, f.env, f.identity);
  assert.equal(response?.status, 201);
  const receipt = await response!.json() as any;
  assert.equal(receipt.package.sha256, hash(raw)); assert.equal(receipt.package.size, raw.length);
  const row = await DB.prepare('SELECT * FROM games WHERE id=?').bind(f.id).first<GameRow>();
  assert.equal(row!.visibility, 'private'); assert.equal(row!.owner_id, f.identity.userId);
  assert.equal(row!.received_sha256, hash(raw)); assert.equal(row!.received_size, raw.length);
  const object = await bucket.get(row!.package_key);
  assert.ok(object); assert.equal(hash(new Uint8Array(await object.arrayBuffer())), hash(raw));
  assert.equal((await bucket.list({ prefix: `uploads/${f.id}-` })).objects.length, 0);
});

async function existing() {
  const f = await fixture(), raw = pck(1024);
  assert.equal((await handleLibrary(request(f.id, raw, [317]).value, f.env, f.identity))?.status, 201);
  await DB.prepare("UPDATE games SET visibility='public' WHERE id=?").bind(f.id).run();
  const row = await DB.prepare('SELECT * FROM games WHERE id=?').bind(f.id).first<GameRow>();
  return { ...f, raw, row: row! };
}
async function unchanged(f: Awaited<ReturnType<typeof existing>>) {
  assert.deepEqual(await DB.prepare('SELECT * FROM games WHERE id=?').bind(f.id).first(), f.row);
  const object = await bucket.get(f.row.package_key);
  assert.ok(object); assert.equal(hash(new Uint8Array(await object.arrayBuffer())), hash(f.raw));
  assert.equal((await bucket.list({ prefix: `uploads/${f.id}-` })).objects.length, 0);
}

test('successful replacement keeps public visibility and the previous immutable object', async () => {
  const f = await existing(), raw = pck(17 * MiB + 37);
  const response = await handleLibrary(request(f.id, raw, [65536, 19500, 317]).value, f.env, f.identity);
  assert.equal(response?.status, 200);
  const result = await response!.json() as any;
  assert.equal(result.visibility, 'public'); assert.equal(result.ownerId, f.identity.userId);
  assert.equal(result.package.sha256, hash(raw));
  const old = await bucket.get(f.row.package_key);
  assert.ok(old); assert.equal(hash(new Uint8Array(await old.arrayBuffer())), hash(f.raw));
});

test('mid-stream failure aborts the multipart upload and preserves the published version', async () => {
  const f = await existing(), raw = pck(17 * MiB + 37);
  let aborted = false;
  // Delegate all storage to real local R2; observe the required abort boundary.
  const observed = new Proxy(bucket, { get(target, property) {
    if (property === 'createMultipartUpload') return async (...args: Parameters<R2Bucket['createMultipartUpload']>) => {
      const upload = await target.createMultipartUpload(...args);
      return new Proxy(upload, { get(part, name) {
        if (name === 'abort') return async () => { aborted = true; await part.abort(); };
        const value = Reflect.get(part, name); return typeof value === 'function' ? value.bind(part) : value;
      } });
    };
    const value = Reflect.get(target, property); return typeof value === 'function' ? value.bind(target) : value;
  } });
  await assert.rejects(handleLibrary(request(f.id, raw, [65536], { failAfter: 9 * MiB }).value,
    { ...f.env, PACKAGES: observed }, f.identity), /synthetic source failure/);
  assert.ok(aborted); await unchanged(f);
});

test('streamed size limit rejects undeclared oversized bodies and cancels the source', async () => {
  const f = await existing(), raw = pck(17 * MiB + 37);
  const req = request(f.id, raw, [65536, 19500, 317], { declared: false });
  await assert.rejects(handleLibrary(req.value, { ...f.env, MAX_PACKAGE_BYTES: String(9 * MiB) }, f.identity),
    (e: unknown) => e instanceof HttpError && e.status === 413);
  assert.ok(req.wasCancelled()); await unchanged(f);
});

test('PCK MD5 validation still rejects corrupted content after multipart storage', async () => {
  const f = await existing(), raw = pck(17 * MiB + 37); raw[112] ^= 1;
  await assert.rejects(handleLibrary(request(f.id, raw, [65536, 19500, 317]).value, f.env, f.identity),
    (e: unknown) => e instanceof HttpError && e.status === 400);
  await unchanged(f);
});
