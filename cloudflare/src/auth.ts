import { pbkdf2Async } from '@noble/hashes/pbkdf2.js';
import { sha256 } from '@noble/hashes/sha2.js';
import {
  credential, digest, HttpError, json, nowISO, randomHex, readJSON,
  type Env, type Identity,
} from './types';

const NAME = /^[a-zA-Z0-9_-]{3,32}$/;
const BEARER = /^Bearer ((?:gs_|ls_)[A-Za-z0-9_-]{43})$/;
const TOKEN_ID = /^[0-9a-f]{32}$/;
const ITERATIONS = 600_000;
const SESSION_MS = 12 * 60 * 60 * 1000;
const AUTH_WINDOW = 600;
const encoder = new TextEncoder();
// A fixed dummy salt/key costs the same 600,000 iterations as a real account.
// This is not a real account or a credential, and it never grants access.
const DUMMY_HASH = 'pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
const AUTH_HEADERS = { 'Cache-Control': 'private, no-store', 'X-Content-Type-Options': 'nosniff', Vary: 'Authorization' };

interface UserRow {
  id: number;
  name: string;
  status: string;
  password_hash: string;
  created_at: string;
  approved_at: string | null;
  deleted_at: string | null;
}

export function validAccountName(value: unknown): value is string {
  return typeof value === 'string' && NAME.test(value);
}

/** Match the Go server's UTF-8 byte limits, not JavaScript's UTF-16 length. */
export function validPassword(value: unknown): value is string {
  if (typeof value !== 'string' || value.length > 128) return false;
  const length = encoder.encode(value).length;
  return length >= 10 && length <= 128;
}

function base64(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes)).replaceAll('=', '');
}

/** No data-dependent early return; password comparisons always use 32 bytes. */
export function constantTimeEqual(a: Uint8Array, b: Uint8Array): boolean {
  let different = a.length ^ b.length;
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    different |= (a[i] ?? 0) ^ (b[i] ?? 0);
  }
  return different === 0;
}

function parsePasswordHash(encoded: string): { salt: Uint8Array; key: Uint8Array } | null {
  // This is a new, isolated account store: no imported or weaker legacy hashes.
  const match = /^pbkdf2-sha256\$600000\$([A-Za-z0-9+/]{22})\$([A-Za-z0-9+/]{43})$/.exec(encoded);
  if (!match) return null;
  const salt = Uint8Array.from(atob(match[1]!), c => c.charCodeAt(0));
  const key = Uint8Array.from(atob(match[2]!), c => c.charCodeAt(0));
  // Reject noncanonical encodings (including nonzero unused base64 bits).
  if (base64(salt) !== match[1] || base64(key) !== match[2]) return null;
  return { salt, key };
}

export async function hashPassword(password: string): Promise<string> {
  if (!validPassword(password)) throw new HttpError(400, 'invalid password');
  const salt = crypto.getRandomValues(new Uint8Array(16));
  // Workers WebCrypto caps PBKDF2 at 100,000 iterations. Do NOT weaken this to
  // fit that limit. noble's async implementation supports the full work factor.
  const key = await pbkdf2Async(sha256, encoder.encode(password), salt, { c: ITERATIONS, dkLen: 32 });
  return `pbkdf2-sha256$${ITERATIONS}$${base64(salt)}$${base64(key)}`;
}

export async function verifyPassword(encoded: string, password: string): Promise<boolean> {
  const parsed = parsePasswordHash(encoded);
  const target = parsed ?? parsePasswordHash(DUMMY_HASH)!;
  // Malformed/absent hashes do dummy work too; they cannot reveal tombstones or
  // password-less accounts through an inexpensive early rejection.
  const got = await pbkdf2Async(sha256, encoder.encode(password), target.salt, { c: ITERATIONS, dkLen: 32 });
  const equal = constantTimeEqual(got, target.key);
  return parsed !== null && equal;
}

/** Invalid credentials are anonymous; storage faults MUST propagate to the router. */
export async function authenticate(request: Request, env: Env): Promise<Identity | null> {
  const match = BEARER.exec(request.headers.get('Authorization') ?? '');
  if (!match) return null;
  const raw = match[1]!;
  const hash = await digest(raw);
  const kind = raw.startsWith('gs_') ? 'api' : 'login';
  const now = nowISO();
  const row = kind === 'api'
    ? await env.DB.prepare(`SELECT u.id, u.name, u.status, t.id AS token_id
        FROM tokens t JOIN users u ON u.id=t.user_id
        WHERE t.hash=? AND t.revoked_at IS NULL AND julianday(t.expires_at)>julianday(?)
          AND u.status='active' AND u.deleted_at IS NULL`).bind(hash, now)
      .first<{ id: number; name: string; status: string; token_id?: string }>()
    : await env.DB.prepare(`SELECT u.id, u.name, u.status
        FROM login_sessions s JOIN users u ON u.id=s.user_id
        WHERE s.hash=? AND julianday(s.expires_at)>julianday(?)
          AND u.status IN ('pending','active') AND u.deleted_at IS NULL`).bind(hash, now)
      .first<{ id: number; name: string; status: string; token_id?: string }>();
  if (!row) return null;
  return { userId: row.id, name: row.name, status: row.status, kind, hash,
    ...(kind === 'api' ? { tokenId: row.token_id } : {}) };
}

/** Atomic, database-shared fixed window. Never store client IPs or account keys. */
export async function rateLimit(env: Env, key: string, limit: number, windowSeconds: number): Promise<void> {
  if (!Number.isSafeInteger(limit) || limit < 1 || limit >= Number.MAX_SAFE_INTEGER ||
      !Number.isSafeInteger(windowSeconds) || windowSeconds < 1 || windowSeconds > 31_536_000) {
    throw new Error('invalid rate limit configuration');
  }
  const now = Math.floor(Date.now() / 1000);
  const storedKey = await digest(`rate-limit:v1:${key}`);
  const row = await env.DB.prepare(`INSERT INTO rate_limits (key,count,expires_at) VALUES (?,1,?)
    ON CONFLICT(key) DO UPDATE SET
      count=CASE WHEN rate_limits.expires_at<=? THEN 1 ELSE MIN(rate_limits.count+1,?) END,
      expires_at=CASE WHEN rate_limits.expires_at<=? THEN excluded.expires_at ELSE rate_limits.expires_at END
    RETURNING count`).bind(storedKey, now + windowSeconds, now, limit + 1, now).first<{ count: number }>();
  if (!row) throw new Error('rate limit storage unavailable');
  if (row.count > limit) throw new HttpError(429, 'too many attempts');
}

async function limitAttempt(request: Request, env: Env, action: string, name: string, clientLimit: number, accountLimit: number): Promise<void> {
  // Cloudflare overwrites CF-Connecting-IP at ingress. Never consult arbitrary
  // X-Forwarded-For/X-Real-IP headers. Missing ingress information shares a bucket.
  const client = request.headers.get('CF-Connecting-IP')?.trim().toLowerCase() || 'unknown';
  await rateLimit(env, `auth:${action}:ip:${client}`, clientLimit, AUTH_WINDOW);
  await rateLimit(env, `auth:${action}:name:${name.toLowerCase()}`, accountLimit, AUTH_WINDOW);
}

async function body(request: Request, fields: string[]): Promise<Record<string, unknown>> {
  const value = await readJSON(request, 4096) as Record<string, unknown>;
  if (Object.keys(value).some(key => !fields.includes(key))) throw new HttpError(400, 'invalid request');
  return value;
}

function response(value: unknown, status = 200): Response { return json(value, status, AUTH_HEADERS); }
function empty(): Response { return new Response(null, { status: 204, headers: AUTH_HEADERS }); }
function method(request: Request, allowed: string[]): void {
  if (!allowed.includes(request.method)) throw new HttpError(405, 'method not allowed');
}
function loginIdentity(identity: Identity | null): Identity {
  if (!identity || identity.kind !== 'login' || !['pending', 'active'].includes(identity.status)) {
    throw new HttpError(401, 'sign-in required');
  }
  return identity;
}
function checked(results: D1Result[]): void {
  if (results.some(result => !result.success)) throw new Error('authentication storage unavailable');
}

async function register(request: Request, env: Env): Promise<Response> {
  const input = await body(request, ['name', 'password']);
  const name = typeof input.name === 'string' ? input.name.trim() : '';
  if (!validAccountName(name) || !validPassword(input.password)) {
    throw new HttpError(400, 'invalid account name or password');
  }
  await limitAttempt(request, env, 'register', name, 5, 5);
  const passwordHash = await hashPassword(input.password);
  const now = nowISO();
  const row = await env.DB.prepare(`INSERT INTO users (name,password_hash,status,created_at,updated_at)
    VALUES (?,?,'pending',?,?) ON CONFLICT(name) DO NOTHING RETURNING name,status`)
    .bind(name, passwordHash, now, now).first<{ name: string; status: string }>();
  if (!row) throw new HttpError(409, 'account name is already taken');
  return response({ name: row.name, status: row.status }, 201);
}

async function login(request: Request, env: Env): Promise<Response> {
  const input = await body(request, ['name', 'password']);
  const name = typeof input.name === 'string' ? input.name.trim() : '';
  if (!validAccountName(name) || !validPassword(input.password)) throw new HttpError(401, 'invalid credentials');
  await limitAttempt(request, env, 'login', name, 20, 10);
  const user = await env.DB.prepare('SELECT * FROM users WHERE name=?').bind(name).first<UserRow>();
  const passwordOK = await verifyPassword(user?.password_hash ?? DUMMY_HASH, input.password);
  if (!user || !passwordOK || user.status === 'deleted' || user.deleted_at !== null) {
    throw new HttpError(401, 'invalid credentials');
  }
  if (user.status === 'rejected') throw new HttpError(403, 'account was rejected');
  if (!['pending', 'active'].includes(user.status)) throw new HttpError(401, 'invalid credentials');
  const raw = credential('ls_');
  const hash = await digest(raw);
  const now = nowISO();
  const expiresAt = new Date(Date.parse(now) + SESSION_MS).toISOString();
  // The password check took time: recheck status AND the checked hash in the
  // write itself so a concurrent close/rejection cannot resurrect a session.
  const result = await env.DB.prepare(`INSERT INTO login_sessions (hash,user_id,expires_at,created_at)
    SELECT ?,id,?,? FROM users WHERE id=? AND password_hash=?
      AND status IN ('pending','active') AND deleted_at IS NULL`)
    .bind(hash, expiresAt, now, user.id, user.password_hash).run();
  checked([result]);
  if (result.meta.changes !== 1) throw new HttpError(401, 'invalid credentials');
  return response({ name: user.name, status: user.status, sessionToken: raw, expiresAt });
}

async function me(env: Env, identity: Identity | null): Promise<Response> {
  if (!identity) throw new HttpError(401, 'token required');
  const user = await env.DB.prepare(`SELECT id,name,status,created_at,approved_at FROM users
    WHERE id=? AND deleted_at IS NULL AND status IN ('pending','active')`).bind(identity.userId).first<UserRow>();
  if (!user || (identity.kind === 'api' && user.status !== 'active')) throw new HttpError(401, 'token required');
  return response({ id: user.id, name: user.name, status: user.status, createdAt: user.created_at,
    ...(user.approved_at ? { approvedAt: user.approved_at } : {}) });
}

async function listTokens(env: Env, identity: Identity | null): Promise<Response> {
  const actor = loginIdentity(identity);
  const result = await env.DB.prepare(`SELECT id,created_at,expires_at,revoked_at FROM tokens
    WHERE user_id=? ORDER BY created_at DESC,id ASC`).bind(actor.userId)
    .all<{ id: string; created_at: string; expires_at: string; revoked_at: string | null }>();
  checked([result]);
  return response({ tokens: result.results.map(token => ({ id: token.id, createdAt: token.created_at,
    expiresAt: token.expires_at, ...(token.revoked_at ? { revokedAt: token.revoked_at } : {}) })) });
}

async function createToken(request: Request, env: Env, identity: Identity | null): Promise<Response> {
  const actor = loginIdentity(identity);
  const input = await body(request, ['ttlHours']);
  const hours = input.ttlHours ?? 720;
  if (typeof hours !== 'number' || !Number.isInteger(hours) || hours < 1 || hours > 8760) {
    throw new HttpError(400, 'ttlHours must be between 1 and 8760');
  }
  if (actor.status !== 'active') throw new HttpError(403, 'account is waiting for administrator approval');
  await limitAttempt(request, env, 'token', actor.name, 30, 20);
  const id = randomHex();
  const raw = credential('gs_');
  const hash = await digest(raw);
  const now = nowISO();
  const expiresAt = new Date(Date.parse(now) + hours * 3_600_000).toISOString();
  const result = await env.DB.prepare(`INSERT INTO tokens (id,user_id,hash,expires_at,created_at)
    SELECT ?,u.id,?,?,? FROM users u WHERE u.id=? AND u.status='active' AND u.deleted_at IS NULL
      AND EXISTS (SELECT 1 FROM login_sessions s WHERE s.user_id=u.id AND s.hash=?
        AND julianday(s.expires_at)>julianday(?))`)
    .bind(id, hash, expiresAt, now, actor.userId, actor.hash, now).run();
  checked([result]);
  if (result.meta.changes !== 1) throw new HttpError(403, 'account cannot issue token');
  return response({ id, token: raw, expiresAt }, 201);
}

async function revokeToken(env: Env, identity: Identity | null, id: string): Promise<Response> {
  const actor = loginIdentity(identity);
  if (!TOKEN_ID.test(id)) throw new HttpError(404, 'token not found');
  const now = nowISO();
  const results = await env.DB.batch([
    env.DB.prepare('UPDATE tokens SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL')
      .bind(now, id, actor.userId),
    env.DB.prepare('DELETE FROM play_sessions WHERE source_token_id=? AND user_id=?').bind(id, actor.userId),
    env.DB.prepare('DELETE FROM device_sessions WHERE source_token_id=? AND user_id=?').bind(id, actor.userId),
  ]);
  checked(results);
  if (results[0]!.meta.changes !== 1) throw new HttpError(404, 'token not found');
  return empty();
}

async function closeAccount(request: Request, env: Env, identity: Identity | null): Promise<Response> {
  const actor = loginIdentity(identity);
  const input = await body(request, ['password', 'confirmName']);
  const user = await env.DB.prepare(`SELECT * FROM users WHERE id=? AND deleted_at IS NULL
    AND status IN ('pending','active')`).bind(actor.userId).first<UserRow>();
  if (!user) throw new HttpError(401, 'sign-in required');
  if (typeof input.confirmName !== 'string' || input.confirmName.trim().toLowerCase() !== user.name.toLowerCase()) {
    throw new HttpError(400, 'confirmation does not match the account name');
  }
  await limitAttempt(request, env, 'close', user.name, 5, 5);
  if (!validPassword(input.password) || !await verifyPassword(user.password_hash, input.password)) {
    throw new HttpError(403, 'password does not match');
  }
  const replacement = await hashPassword(credential(''));
  const now = nowISO();
  // D1 batch is transactional and serialized. A random replacement hash acts as
  // a per-close marker: if the guarded UPDATE loses a race, NONE of its deletes
  // may run. Children go before tokens to satisfy the play-session foreign key.
  const closedByThisBatch = `EXISTS (SELECT 1 FROM users WHERE id=? AND status='deleted' AND password_hash=?)`;
  const results = await env.DB.batch([
    env.DB.prepare(`UPDATE users SET status='deleted',password_hash=?,deleted_at=?,updated_at=?
      WHERE id=? AND password_hash=? AND status IN ('pending','active') AND deleted_at IS NULL
        AND EXISTS (SELECT 1 FROM login_sessions WHERE user_id=? AND hash=? AND julianday(expires_at)>julianday(?))`)
      .bind(replacement, now, now, user.id, user.password_hash, user.id, actor.hash, now),
    ...['play_sessions', 'device_sessions', 'tokens', 'login_sessions'].map(table =>
      env.DB.prepare(`DELETE FROM ${table} WHERE user_id=? AND ${closedByThisBatch}`)
        .bind(user.id, user.id, replacement)),
  ]);
  checked(results);
  if (results[0]!.meta.changes !== 1) throw new HttpError(401, 'sign-in required');
  return empty();
}

/** Account routes only; the parent router owns other paths and sanitized errors. */
export async function handleAuth(request: Request, env: Env, identity: Identity | null): Promise<Response | null> {
  const path = new URL(request.url).pathname;
  switch (path) {
    case '/api/v1/auth/register':
      method(request, ['POST']); return register(request, env);
    case '/api/v1/auth/login':
      method(request, ['POST']); return login(request, env);
    case '/api/v1/auth/logout': {
      method(request, ['POST']);
      const actor = loginIdentity(identity);
      const result = await env.DB.prepare('DELETE FROM login_sessions WHERE hash=? AND user_id=?')
        .bind(actor.hash, actor.userId).run();
      checked([result]);
      return empty();
    }
    case '/api/v1/me':
      method(request, ['GET', 'DELETE']);
      return request.method === 'GET' ? me(env, identity) : closeAccount(request, env, identity);
    case '/api/v1/me/tokens':
      method(request, ['GET', 'POST']);
      return request.method === 'GET' ? listTokens(env, identity) : createToken(request, env, identity);
    default: {
      const tokenPath = /^\/api\/v1\/me\/tokens\/([^/]+)$/.exec(path);
      if (!tokenPath) return null;
      method(request, ['DELETE']);
      return revokeToken(env, identity, tokenPath[1]!);
    }
  }
}
