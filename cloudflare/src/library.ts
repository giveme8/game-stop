import { inspectBundle, inspectPCK, validateMetadata, MAX_BUFFERED_PACKAGE } from './package';
import { inspectStoredPCK } from './large-pck';
import { signedObjectURL } from './r2-sign';
import { sha256 } from '@noble/hashes/sha2.js';
import {
  Env, GameRow, HttpError, ID, Identity, digest, hex, json, nowISO, readJSON,
  requireAPI, visibleGame,
} from './types';

const MAX_PACKAGE = 512 * 1024 * 1024;
const PART_SIZE = 8 * 1024 * 1024;
const METADATA_LIMIT = 64 * 1024;
const ASSET_PROCESS = new Set(['cover.png', 'cover.jpg', 'cover.webp']);

export function maxPackageBytes(env: Env): number {
  const configured = Number(env.MAX_PACKAGE_BYTES);
  return Number.isSafeInteger(configured) && configured > 0 && configured <= MAX_PACKAGE ? configured : MAX_PACKAGE;
}
export function bufferedPackageBytes(env: Env): number {
  return Math.min(MAX_BUFFERED_PACKAGE, maxPackageBytes(env));
}
function packageSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}
/** Download metadata for a package object: an opaque binary that saves as `<id>.pck`. */
function packageMetadata(id: string): R2HTTPMetadata {
  // The id is already validated by the caller; this only builds a filename.
  return { contentType: 'application/octet-stream', contentDisposition: `attachment; filename="${id}.pck"`, cacheControl: 'private, no-store' };
}
export function packageSuffix(row: Pick<GameRow, 'format'>): string {
  return row.format === 'pck' ? 'package.pck' : 'package.game';
}
export function assetURL(base: string, id: string, name: string): string {
  return `${base}/api/v1/games/${id}/assets/${name}`;
}

interface RepresentationOptions { base: string; includeScreenshots?: boolean; }

/**
 * Public JSON shape for one game. Absolute URLs point straight at R2 with a
 * short-lived signature when the instance is configured for direct delivery —
 * the same contract the Go implementation fulfils with OSS. The Worker's own
 * `/api/v1/games/{id}/package.pck` route stays available and redirects.
 */
export async function representation(env: Env, row: GameRow, options: RepresentationOptions): Promise<Record<string, unknown>> {
  const meta = parseMetadata(row.metadata);
  const format = row.format === '' ? 'pck' : row.format;
  const direct = await signedObjectURL(env, row.package_key);
  const out: Record<string, unknown> = {
    id: row.id, ownerId: row.owner_id, title: meta.title, subtitle: meta.subtitle,
    description: meta.description, tags: meta.tags, accent: meta.accent, engine: meta.engine,
    visibility: row.visibility, status: 'ready', createdAt: row.created_at, updatedAt: row.updated_at,
    package: {
      url: direct ?? `${options.base}/api/v1/games/${row.id}/${packageSuffix(row)}`,
      size: row.size, sha256: row.sha256, format,
    },
  };
  if (row.received_sha256) out.received = { size: row.received_size, sha256: row.received_sha256 };
  const assets = parseAssets(row.assets);
  if (assets.cover) {
    out.cover = (await signedObjectURL(env, `assets/${row.id}/${assets.cover.name}`)) ?? assetURL(options.base, row.id, assets.cover.name);
    out.coverSha256 = assets.cover.sha256;
  }
  if (options.includeScreenshots !== false && assets.screenshots.length) {
    out.screenshots = await Promise.all(assets.screenshots.map(async s =>
      (await signedObjectURL(env, `assets/${row.id}/${s.name}`)) ?? assetURL(options.base, row.id, s.name)));
  }
  const source = parseSource(row.source);
  if (source.repo) out.source = source;
  return out;
}

interface StoredAsset { name: string; sha256: string; }
export function parseAssets(raw: string): { cover: StoredAsset | null; screenshots: StoredAsset[] } {
  try {
    const value = JSON.parse(raw || '{}') as { cover?: StoredAsset; screenshots?: StoredAsset[] };
    return { cover: value.cover ?? null, screenshots: Array.isArray(value.screenshots) ? value.screenshots : [] };
  } catch { return { cover: null, screenshots: [] }; }
}
export function parseMetadata(raw: string): ReturnType<typeof validateMetadata> {
  try { return validateMetadata(JSON.parse(raw)); }
  catch { return { title: '', subtitle: '', description: '', tags: [], accent: '', engine: { name: 'godot', version: '4.0' }, visibility: '', modes: [] }; }
}
function parseSource(raw: string): Record<string, string> {
  try { const value = JSON.parse(raw || '{}'); return value && typeof value === 'object' ? value : {}; } catch { return {}; }
}

export async function publicGames(env: Env): Promise<GameRow[]> {
  const result = await env.DB.prepare('SELECT * FROM games WHERE deleted=0 AND visibility=? ORDER BY id ASC').bind('public').all<GameRow>();
  return result.results ?? [];
}
async function visibleGames(env: Env, identity: Identity | null): Promise<GameRow[]> {
  if (!identity) return publicGames(env);
  const result = await env.DB.prepare('SELECT * FROM games WHERE deleted=0 AND (visibility=? OR owner_id=?) ORDER BY id ASC').bind('public', identity.userId).all<GameRow>();
  return result.results ?? [];
}

function metadataHeader(request: Request): unknown {
  const raw = request.headers.get('X-Game-Metadata');
  if (!raw) throw new HttpError(400, 'invalid metadata');
  const encoded = raw.length > Math.ceil(METADATA_LIMIT / 3) * 4 + 8 ? '' : raw;
  if (!encoded || !/^[A-Za-z0-9+/]*={0,2}$/.test(encoded)) throw new HttpError(400, 'invalid metadata');
  let bytes: Uint8Array;
  try { bytes = Uint8Array.from(atob(encoded), c => c.charCodeAt(0)); } catch { throw new HttpError(400, 'invalid metadata'); }
  if (bytes.length > METADATA_LIMIT) throw new HttpError(400, 'invalid metadata');
  try { return JSON.parse(new TextDecoder('utf-8', { fatal: true, ignoreBOM: false }).decode(bytes)); }
  catch { throw new HttpError(400, 'invalid metadata'); }
}

/** Stream the request body into R2, returning the byte count and SHA-256. */
async function storeStream(env: Env, key: string, gameID: string, body: ReadableStream<Uint8Array>, limit: number): Promise<{ size: number; sha256: string }> {
  const hash = sha256.create();
  const upload = await env.PACKAGES.createMultipartUpload(key, { httpMetadata: packageMetadata(gameID) });
  const parts: R2UploadedPart[] = [];
  const reader = body.getReader();
  // Network read boundaries are arbitrary. R2 requires all non-final parts to
  // have the same size, so assemble exactly PART_SIZE bytes before each write.
  const buffered = new Uint8Array(PART_SIZE);
  let bufferedSize = 0; let size = 0; let partNumber = 1;
  const flush = async () => {
    if (!bufferedSize) return;
    // Await consumption before reusing the buffer. Only the final part may be
    // shorter; passing a view also avoids copying an entire part at flush time.
    parts.push(await upload.uploadPart(partNumber++, buffered.subarray(0, bufferedSize)));
    bufferedSize = 0;
  };
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!value?.length) continue;
      size += value.length;
      if (size > limit) throw new HttpError(413, 'package exceeds native SaaS limit');
      hash.update(value);
      let offset = 0;
      while (offset < value.length) {
        const take = Math.min(PART_SIZE - bufferedSize, value.length - offset);
        buffered.set(value.subarray(offset, offset + take), bufferedSize);
        bufferedSize += take; offset += take;
        if (bufferedSize === PART_SIZE) await flush();
      }
    }
    await flush();
    if (size === 0) throw new HttpError(400, 'upload incomplete');
    await upload.complete(parts);
  } catch (error) {
    try { await upload.abort(); } catch { /* the object was never published */ }
    await reader.cancel().catch(() => {});
    throw error;
  } finally { reader.releaseLock(); }
  return { size, sha256: hex(hash.digest()) };
}

/** Server-side copy; R2 has no rename, so stream the validated staging object. */
async function copyObject(env: Env, from: string, to: string): Promise<void> {
  const source = await env.PACKAGES.get(from);
  if (!source) throw new HttpError(503, 'upload storage unavailable');
  await env.PACKAGES.put(to, source.body, { httpMetadata: packageMetadata(to.split('/')[1] ?? 'package') });
}

interface GameWrite { metadata: ReturnType<typeof validateMetadata>; source: Record<string, string>; }

async function upsertGame(env: Env, id: string, ownerId: number, write: GameWrite, pkg: { key: string; size: number; sha256: string; format: string }, received: { size: number; sha256: string }, assetsJSON: string, visibility: string): Promise<void> {
  const stamp = nowISO();
  // The INSERT..ON CONFLICT plus the WHERE clause keeps the original owner: a
  // conflicting writer can never take over an ID, and deleted tombstones keep
  // their owner (re-upload by the owner clears the tombstone).
  await env.DB.prepare(
    `INSERT INTO games (id,owner_id,visibility,metadata,package_key,size,sha256,format,assets,source,deleted,created_at,updated_at,received_size,received_sha256)
     VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?,?,?)
     ON CONFLICT(id) DO UPDATE SET visibility=excluded.visibility, metadata=excluded.metadata, package_key=excluded.package_key,
       size=excluded.size, sha256=excluded.sha256, format=excluded.format, assets=excluded.assets, source=excluded.source,
       deleted=0, updated_at=excluded.updated_at, received_size=excluded.received_size, received_sha256=excluded.received_sha256
     WHERE games.owner_id=excluded.owner_id`,
  ).bind(id, ownerId, visibility, JSON.stringify(write.metadata), pkg.key, pkg.size, pkg.sha256, pkg.format, assetsJSON, JSON.stringify(write.source), stamp, stamp, received.size, received.sha256).run();
  const check = await env.DB.prepare('SELECT owner_id FROM games WHERE id=?').bind(id).first<{ owner_id: number }>();
  if (!check || check.owner_id !== ownerId) throw new HttpError(409, 'game id belongs to another user or conflicting write');
}

async function replaceAssets(env: Env, id: string, assets: Array<{ name: string; ext: string; data: Uint8Array }>, keep: string[]): Promise<string> {
  const stored: StoredAsset[] = [];
  const list = await env.PACKAGES.list({ prefix: `assets/${id}/` });
  for (const object of list.objects) if (!keep.includes(object.key)) await env.PACKAGES.delete(object.key);
  for (let index = 0; index < assets.length; index++) {
    const asset = assets[index]!;
    const name = index === 0 ? `cover${asset.ext}` : `shot-${index}${asset.ext}`;
    const key = `assets/${id}/${name}`;
    await env.PACKAGES.put(key, asset.data, { httpMetadata: { contentType: imageContentType(asset.ext), cacheControl: 'private, no-store' } });
    stored.push({ name, sha256: hex(sha256(asset.data)) });
  }
  return JSON.stringify({ cover: stored[0] ?? null, screenshots: stored.slice(1) });
}
function imageContentType(ext: string): string {
  return ext === '.png' ? 'image/png' : ext === '.jpg' ? 'image/jpeg' : ext === '.webp' ? 'image/webp' : 'application/octet-stream';
}

async function uploadBundle(env: Env, request: Request, id: string, identity: Identity, existing: GameRow | null): Promise<Response> {
  if (request.headers.get('X-Game-Metadata')) throw new HttpError(400, 'metadata must live inside the bundle');
  const limit = bufferedPackageBytes(env);
  const declared = request.headers.get('Content-Length');
  if (declared && Number(declared) > limit) throw new HttpError(413, 'native bundle limit is 32 MiB; publish a bare PCK for larger games');
  const body = await readBody(request, limit);
  const plan = await inspectBundle(body, id);
  const stamp = nowISO();
  const name = `games/${id}/${plan.engineVersion.replace(/[^0-9.]/g, '')}-${hex(sha256(plan.pck)).slice(0, 16)}.pck`;
  await env.PACKAGES.put(name, plan.pck, { httpMetadata: packageMetadata(id) });
  const visibility = existing ? existing.visibility : 'private';
  const assetsJSON = await replaceAssets(env, id, plan.assets, []);
  await upsertGame(env, id, identity.userId,
    { metadata: plan.metadata, source: plan.source },
    { key: name, size: plan.pck.length, sha256: hex(sha256(plan.pck)), format: 'pck' },
    { size: body.length, sha256: hex(sha256(body)) }, assetsJSON, visibility);
  void stamp;
  const row = await env.DB.prepare('SELECT * FROM games WHERE id=?').bind(id).first<GameRow>();
  return json(await representation(env, row!, { base: env.PUBLIC_BASE_URL }), existing ? 200 : 201);
}

async function readBody(request: Request, limit: number): Promise<Uint8Array> {
  if (!request.body) throw new HttpError(400, 'upload incomplete');
  const reader = request.body.getReader(); const chunks: Uint8Array[] = []; let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read(); if (done) break;
      size += value.length;
      if (size > limit) throw new HttpError(413, 'native bundle limit is 32 MiB; publish a bare PCK for larger games');
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  if (!size) throw new HttpError(400, 'upload incomplete');
  const body = new Uint8Array(size); let at = 0;
  for (const chunk of chunks) { body.set(chunk, at); at += chunk.length; }
  return body;
}

async function uploadPCK(env: Env, request: Request, id: string, identity: Identity, existing: GameRow | null): Promise<Response> {
  const metadata = validateMetadata(metadataHeader(request));
  const limit = maxPackageBytes(env);
  const declared = request.headers.get('Content-Length');
  if (declared && Number(declared) > limit) throw new HttpError(413, `package exceeds native SaaS limit of ${limit} bytes`);
  const staging = `uploads/${id}-${crypto.randomUUID()}.part`;
  let stored: { size: number; sha256: string };
  try {
    stored = await storeStream(env, staging, id, request.body ?? new ReadableStream(), limit);
    const inspected = await inspectStoredPCK(env, staging, stored.size);
    if (inspected.sha256 !== stored.sha256) throw new HttpError(400, 'upload changed while being read');
    if (metadata.engine.version !== inspected.engineVersion) throw new HttpError(400, 'engine version mismatch');
    const name = `games/${id}/${inspected.engineVersion}-${stored.sha256.slice(0, 16)}.pck`;
    await copyObject(env, staging, name);
    await env.PACKAGES.delete(staging);
    const assetsJSON = existing ? existing.assets : JSON.stringify({ cover: null, screenshots: [] });
    await upsertGame(env, id, identity.userId, { metadata, source: {} },
      { key: name, size: stored.size, sha256: stored.sha256, format: 'pck' }, stored, assetsJSON,
      existing ? existing.visibility : 'private');
  } catch (error) {
    await env.PACKAGES.delete(staging).catch(() => {});
    throw error;
  }
  const row = await env.DB.prepare('SELECT * FROM games WHERE id=?').bind(id).first<GameRow>();
  return json(await representation(env, row!, { base: env.PUBLIC_BASE_URL }), existing ? 200 : 201);
}

async function put(request: Request, env: Env, identity: Identity, id: string): Promise<Response> {
  requireAPI(identity);
  if (!ID.test(id)) throw new HttpError(400, 'invalid game id');
  const existing = await env.DB.prepare('SELECT * FROM games WHERE id=?').bind(id).first<GameRow>();
  if (existing && existing.owner_id !== identity.userId) throw new HttpError(409, 'game id belongs to another user or conflicting write');
  const sniffed = await sniff(request, 4);
  const head = sniffed.head;
  if (head.length < 4) throw new HttpError(400, 'upload incomplete');
  const isBundle = head[0] === 0x50 && head[1] === 0x4b && head[2] === 0x03 && head[3] === 0x04;
  const restored = withBody(request, sniffed.remaining());
  return isBundle ? uploadBundle(env, restored, id, identity, existing) : uploadPCK(env, restored, id, identity, existing);
}

/**
 * Read `count` bytes to sniff the magic number, then rebuild a request whose
 * body is `head + remaining`. The bytes already read are never dropped: a single
 * read can return far more than the 4 sniff bytes.
 */
async function sniff(request: Request, count: number): Promise<{ head: Uint8Array; remaining: () => ReadableStream<Uint8Array> | null }> {
  if (!request.body) return { head: new Uint8Array(), remaining: () => null };
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = []; let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (value?.length) { chunks.push(value); size += value.length; }
    if (size >= count) break;
  }
  const head = new Uint8Array(Math.min(size, count)); let at = 0;
  for (const chunk of chunks) { if (at >= head.length) break; const take = Math.min(chunk.length, head.length - at); head.set(chunk.subarray(0, take), at); at += take; }
  const leftovers = chunks.map(chunk => chunk);
  return {
    head,
    remaining: () => new ReadableStream<Uint8Array>({
      start(controller) { for (const chunk of leftovers) controller.enqueue(chunk); },
      async pull(controller) {
        const { done, value } = await reader.read();
        if (done) { controller.close(); return; }
        controller.enqueue(value);
      },
      cancel(reason) { return reader.cancel(reason); },
    }),
  };
}
function withBody(request: Request, body: ReadableStream<Uint8Array> | null): Request {
  if (!body) return request;
  return new Request(request.url, { method: request.method, headers: request.headers, body, duplex: 'half' } as RequestInit);
}

async function patch(request: Request, env: Env, identity: Identity, id: string): Promise<Response> {
  requireAPI(identity);
  const row = await visibleGame(env, id, identity);
  if (row.owner_id !== identity.userId) throw new HttpError(404, 'game not found');
  const body = await readJSON(request, 32768);
  const current = parseMetadata(row.metadata);
  const next = validateMetadata({
    ...current,
    title: body.title ?? current.title, subtitle: body.subtitle ?? current.subtitle,
    description: body.description ?? current.description, tags: body.tags ?? current.tags,
    accent: body.accent ?? current.accent, engine: current.engine,
    visibility: body.visibility ?? current.visibility, modes: current.modes,
  });
  const visibility = body.visibility === undefined ? row.visibility : next.visibility || row.visibility;
  await env.DB.prepare('UPDATE games SET metadata=?, visibility=?, updated_at=? WHERE id=? AND owner_id=? AND deleted=0')
    .bind(JSON.stringify(next), visibility, nowISO(), id, identity.userId).run();
  const updated = await env.DB.prepare('SELECT * FROM games WHERE id=?').bind(id).first<GameRow>();
  return json(await representation(env, updated!, { base: env.PUBLIC_BASE_URL }));
}

async function remove(env: Env, identity: Identity, id: string): Promise<Response> {
  requireAPI(identity);
  await env.DB.prepare('UPDATE games SET deleted=1, updated_at=? WHERE id=? AND owner_id=?').bind(nowISO(), id, identity.userId).run();
  return new Response(null, { status: 204 });
}

/**
 * Objects are served from R2 through a short-lived signature when the instance
 * is configured for direct delivery: proxying a large body through the Worker
 * measured ~500x slower and stalled. `Range`, `HEAD` and `If-None-Match` are
 * all honoured by R2 itself after the redirect. Streaming here remains the
 * fallback so an unconfigured instance still works.
 */
async function download(request: Request, env: Env, identity: Identity | null, id: string, head: boolean): Promise<Response> {
  const row = await visibleGame(env, id, identity);
  const key = row.package_key;
  const direct = await signedObjectURL(env, key);
  if (direct) {
    const headers = new Headers({ Location: direct, 'Cache-Control': 'private, no-store', 'X-Checksum-SHA256': row.sha256 });
    return new Response(null, { status: 302, headers });
  }
  const range = request.headers.get('Range') ?? undefined;
  const object = await env.PACKAGES.get(key, range ? { range: request.headers } : undefined);
  if (!object) throw new HttpError(503, 'package unavailable');
  const headers = new Headers();
  object.writeHttpMetadata(headers);
  headers.set('Content-Type', 'application/octet-stream');
  headers.set('Content-Disposition', `attachment; filename="${row.id}${row.format === 'pck' ? '.pck' : '.game'}"`);
  headers.set('Accept-Ranges', 'bytes');
  headers.set('ETag', `"${row.sha256}"`);
  headers.set('X-Checksum-SHA256', row.sha256);
  if (range && 'range' in object && object.range) {
    const offset = 'offset' in object.range ? Number(object.range.offset) : 0;
    const length = 'length' in object.range ? Number(object.range.length) : object.size - offset;
    headers.set('Content-Range', `bytes ${offset}-${offset + length - 1}/${object.size}`);
    headers.set('Content-Length', String(length));
    return new Response(head ? null : object.body, { status: 206, headers });
  }
  headers.set('Content-Length', String(object.size));
  return new Response(head ? null : object.body, { status: 200, headers });
}

async function asset(env: Env, identity: Identity | null, id: string, name: string, head: boolean): Promise<Response> {
  if (!/^[a-z0-9][a-z0-9.-]{0,63}$/.test(name)) throw new HttpError(404, 'asset not found');
  const row = await visibleGame(env, id, identity);
  const assets = parseAssets(row.assets);
  const known = [assets.cover, ...assets.screenshots].some(a => a?.name === name);
  if (!known) throw new HttpError(404, 'asset not found');
  const direct = await signedObjectURL(env, `assets/${id}/${name}`);
  if (direct) return new Response(null, { status: 302, headers: { Location: direct, 'Cache-Control': 'private, no-store' } });
  const object = await env.PACKAGES.get(`assets/${id}/${name}`);
  if (!object) throw new HttpError(404, 'asset not found');
  const headers = new Headers();
  object.writeHttpMetadata(headers);
  headers.set('Content-Length', String(object.size));
  headers.set('Cache-Control', 'private, no-store');
  headers.set('X-Checksum-SHA256', row.assets.includes(name) ? (assets.cover?.name === name ? assets.cover.sha256 : '') : '');
  return new Response(head ? null : object.body, { status: 200, headers });
}

async function catalog(request: Request, env: Env, head: boolean): Promise<Response> {
  const rows = await publicGames(env);
  let modified = 0;
  for (const row of rows) modified = Math.max(modified, Date.parse(row.updated_at) || 0);
  const body = JSON.stringify({
    protocol: 'gamesource/1', sourceName: 'Game Stop', ttlSeconds: 0,
    generatedAt: new Date(modified).toISOString(),
    games: await Promise.all(rows.map(row => representation(env, row, { base: env.PUBLIC_BASE_URL }))),
  });
  const etag = `"${hex(sha256(new TextEncoder().encode(body)))}"`;
  const headers = new Headers({ 'Content-Type': 'application/json; charset=utf-8', ETag: etag, 'Last-Modified': new Date(modified).toUTCString() });
  for (const candidate of (request.headers.get('If-None-Match') ?? '').split(',')) {
    const value = candidate.trim();
    if (value === '*' || value.replace(/^W\//, '') === etag) return new Response(null, { status: 304, headers });
  }
  headers.set('Content-Length', String(new TextEncoder().encode(body).length));
  return new Response(head ? null : body, { status: 200, headers });
}

export async function handleLibrary(request: Request, env: Env, identity: Identity | null): Promise<Response | null> {
  const url = new URL(request.url);
  const path = url.pathname;
  const method = request.method;
  const head = method === 'HEAD';
  if (path === '/gamesource/v1/index.json' && (method === 'GET' || head)) return catalog(request, env, head);
  if (path === '/api/v1/games' && method === 'GET') {
    const rows = await visibleGames(env, identity);
    return json({ games: await Promise.all(rows.map(row => representation(env, row, { base: env.PUBLIC_BASE_URL }))) });
  }
  const game = /^\/api\/v1\/games\/([^/]+)$/.exec(path);
  if (game) {
    const id = decodeURIComponent(game[1]!);
    if (method === 'GET') {
      const row = await visibleGame(env, id, identity);
      return json(await representation(env, row, { base: env.PUBLIC_BASE_URL }));
    }
    if (method === 'PUT' && identity) return put(request, env, identity, id);
    if (method === 'PATCH' && identity) return patch(request, env, identity, id);
    if (method === 'DELETE' && identity) return remove(env, identity, id);
    return null;
  }
  const pkg = /^\/api\/v1\/games\/([^/]+)\/(package\.pck|package\.game)$/.exec(path);
  if (pkg && (method === 'GET' || head)) return download(request, env, identity, decodeURIComponent(pkg[1]!), head);
  const file = /^\/api\/v1\/games\/([^/]+)\/assets\/([^/]+)$/.exec(path);
  if (file && (method === 'GET' || head)) return asset(env, identity, decodeURIComponent(file[1]!), decodeURIComponent(file[2]!), head);
  return null;
}

export { inspectPCK, MAX_BUFFERED_PACKAGE, packageSize, ASSET_PROCESS };
