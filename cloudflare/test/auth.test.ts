import assert from 'node:assert/strict';
import { pbkdf2Sync } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import { before, test } from 'node:test';
import {
  authenticate, constantTimeEqual, handleAuth, hashPassword, rateLimit,
  validAccountName, validPassword, verifyPassword,
} from '../src/auth';
import { credential, digest, HttpError, type Env, type Identity } from '../src/types';

// Pure Node, in-memory SQL tests of the real migration and all auth statements.
// This is not a Cloudflare integration test: the adapter implements only the
// D1 prepared-statement/batch surface used here, with SQLite transactions.
const schema = readFileSync(new URL('../migrations/0001_native.sql', import.meta.url), 'utf8');
const password = 'a-long-test-password';
let passwordHash: string;
before(async () => { passwordHash = await hashPassword(password); });

class Statement {
  values: Array<string | number | null> = [];
  constructor(readonly db: DatabaseSync, readonly sql: string) {}
  bind(...values: Array<string | number | null>): Statement {
    const statement = new Statement(this.db, this.sql);
    statement.values = values;
    return statement;
  }
  async first<T>(): Promise<T | null> {
    return (this.db.prepare(this.sql).get(...this.values) as T | undefined) ?? null;
  }
  async all<T>(): Promise<{ success: boolean; results: T[]; meta: { changes: number } }> {
    return { success: true, results: this.db.prepare(this.sql).all(...this.values) as T[], meta: { changes: 0 } };
  }
  execute() {
    const result = this.db.prepare(this.sql).run(...this.values);
    return { success: true, results: [], meta: { changes: Number(result.changes) } };
  }
  async run() { return this.execute(); }
}

function fixture(t: { after: (fn: () => void) => void }) {
  const db = new DatabaseSync(':memory:');
  db.exec(schema);
  const adapter = {
    prepare(sql: string) { return new Statement(db, sql); },
    async batch(statements: Statement[]) {
      db.exec('BEGIN IMMEDIATE');
      try {
        const results = statements.map(statement => statement.execute());
        db.exec('COMMIT');
        return results;
      } catch (error) {
        db.exec('ROLLBACK');
        throw error;
      }
    },
  };
  t.after(() => db.close());
  return { db, env: { DB: adapter } as unknown as Env };
}

const future = () => new Date(Date.now() + 60_000).toISOString();
const past = () => new Date(Date.now() - 60_000).toISOString();
function addUser(db: DatabaseSync, name = 'Alice', status = 'active'): number {
  const now = new Date().toISOString();
  return Number(db.prepare(`INSERT INTO users (name,password_hash,status,created_at,updated_at)
    VALUES (?,?,?,?,?)`).run(name, passwordHash, status, now, now).lastInsertRowid);
}
async function session(db: DatabaseSync, userId: number) {
  const raw = credential('ls_');
  const hash = await digest(raw);
  db.prepare('INSERT INTO login_sessions (hash,user_id,expires_at,created_at) VALUES (?,?,?,?)')
    .run(hash, userId, future(), new Date().toISOString());
  const user = db.prepare('SELECT name,status FROM users WHERE id=?').get(userId)!;
  return { raw, identity: { userId, name: user.name, status: user.status, kind: 'login', hash } as Identity };
}
async function apiToken(db: DatabaseSync, userId: number, id = 'a'.repeat(32)) {
  const raw = credential('gs_');
  const hash = await digest(raw);
  db.prepare('INSERT INTO tokens (id,user_id,hash,expires_at,created_at) VALUES (?,?,?,?,?)')
    .run(id, userId, hash, future(), new Date().toISOString());
  return { raw, hash, id };
}
function request(path: string, method = 'GET', body?: unknown, raw?: string): Request {
  const headers = new Headers({ 'CF-Connecting-IP': '192.0.2.42' });
  if (raw) headers.set('Authorization', `Bearer ${raw}`);
  if (body !== undefined) headers.set('Content-Type', 'application/json');
  return new Request(`https://example.test${path}`, {
    method, headers, ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}
function status(code: number, message?: string) {
  return (error: unknown) => error instanceof HttpError && error.status === code &&
    (message === undefined || error.message === message);
}
async function payload(response: Response | null): Promise<Record<string, any>> {
  assert.ok(response);
  return response.json();
}
function rowCount(db: DatabaseSync, table: string, userId?: number): number {
  const result = userId === undefined
    ? db.prepare(`SELECT count(*) AS count FROM ${table}`).get()
    : db.prepare(`SELECT count(*) AS count FROM ${table} WHERE user_id=?`).get(userId);
  return Number(result!.count);
}

function addContentAndSessions(db: DatabaseSync, userId: number, tokenId: string): void {
  const now = new Date().toISOString();
  db.prepare(`INSERT INTO games (id,owner_id,visibility,metadata,package_key,size,sha256,created_at,updated_at)
    VALUES ('demo',?,'public','{}','demo/key',1,'digest',?,?)`).run(userId, now, now);
  db.prepare(`INSERT INTO player_scores (game_id,user_id,board,value,achieved_at) VALUES ('demo',?,'score',7,?)`).run(userId, now);
  db.prepare(`INSERT INTO score_submissions (game_id,user_id,submission_id,board,mode,value,best_value,achieved_at,created_at)
    VALUES ('demo',?,'submission','score','default',7,7,?,?)`).run(userId, now, now);
  db.prepare(`INSERT INTO play_sessions (hash,user_id,game_id,source_token_id,expires_at,created_at)
    VALUES ('play-hash',?,'demo',?,?,?)`).run(userId, tokenId, future(), now);
  db.prepare(`INSERT INTO device_sessions (hash,code,game_id,expires_at,user_id,source_token_id,created_at)
    VALUES ('device-hash','CODE42','demo',?,?,?,?)`).run(future(), userId, tokenId, now);
}

test('validation uses strict account names and UTF-8 password byte bounds', () => {
  for (const value of ['abc', 'A_9', 'x'.repeat(32), 'valid-name']) assert.ok(validAccountName(value));
  for (const value of ['', 'ab', 'x'.repeat(33), '中文名', 'a.b', ' abc', 'abc\n', null, 123]) assert.ok(!validAccountName(value));
  for (const value of ['x'.repeat(10), 'x'.repeat(128), '界'.repeat(4)]) assert.ok(validPassword(value));
  for (const value of ['', 'x'.repeat(9), 'x'.repeat(129), '界'.repeat(43), null, 123]) assert.ok(!validPassword(value));
});

test('password hash is randomized 600k PBKDF2-SHA256 and matches Node crypto', async () => {
  const encoded = await hashPassword(password);
  assert.ok(encoded !== passwordHash);
  const parts = encoded.split('$');
  assert.equal(parts[0], 'pbkdf2-sha256');
  assert.equal(parts[1], '600000');
  const salt = Buffer.from(parts[2]!, 'base64');
  const derived = Buffer.from(parts[3]!, 'base64');
  assert.equal(salt.length, 16);
  assert.equal(derived.length, 32);
  assert.ok(derived.equals(pbkdf2Sync(password, salt, 600_000, 32, 'sha256')));
  assert.ok(await verifyPassword(encoded, password));
  assert.ok(!await verifyPassword(encoded, 'wrong-long-password'));
  await assert.rejects(hashPassword('short'), status(400));
});

test('weak/malformed password encodings fail closed, never downgrade work factor', async () => {
  for (const encoded of ['', passwordHash.replace('$600000$', '$100000$'), `${passwordHash}=`,
    passwordHash.replace('pbkdf2-sha256', 'sha256')]) {
    assert.ok(!await verifyPassword(encoded, password));
  }
});

test('constant time comparison includes the entire byte array and its length', () => {
  assert.ok(constantTimeEqual(new Uint8Array([1, 2, 3]), new Uint8Array([1, 2, 3])));
  assert.ok(!constantTimeEqual(new Uint8Array([1, 2, 3]), new Uint8Array([1, 2, 4])));
  assert.ok(!constantTimeEqual(new Uint8Array([1, 2]), new Uint8Array([1, 2, 0])));
});

test('invalid bearer syntax is anonymous without querying storage', async () => {
  const env = { DB: { prepare() { throw new Error('must not query'); } } } as unknown as Env;
  for (const value of ['', 'Basic none', 'Bearer gs_short', `Bearer ps_${'a'.repeat(43)}`,
    `bearer gs_${'a'.repeat(43)}`, `Bearer gs_${'a'.repeat(44)}`, `Bearer gs_${'a'.repeat(42)}=`,
    `Bearer gs_${'a'.repeat(43)}, Bearer gs_${'a'.repeat(43)}`]) {
    const req = new Request('https://example.test', { headers: { Authorization: value } });
    assert.equal(await authenticate(req, env), null);
  }
});

test('authentication distinguishes login/API and enforces hash, expiry, status, revocation', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  const api = await apiToken(db, id);
  const loginIdentity = await authenticate(request('/api/v1/me', 'GET', undefined, login.raw), env);
  const apiIdentity = await authenticate(request('/api/v1/me', 'GET', undefined, api.raw), env);
  assert.equal(loginIdentity?.kind, 'login');
  assert.equal(apiIdentity?.kind, 'api');
  assert.equal(apiIdentity?.tokenId, api.id);
  assert.ok(loginIdentity?.hash === await digest(login.raw));
  assert.ok(apiIdentity?.hash === api.hash);
  assert.ok(!JSON.stringify(apiIdentity).includes(api.raw));
  db.prepare("UPDATE users SET status='pending' WHERE id=?").run(id);
  assert.equal(await authenticate(request('/', 'GET', undefined, api.raw), env), null);
  assert.equal((await authenticate(request('/', 'GET', undefined, login.raw), env))?.status, 'pending');
  for (const value of ['rejected', 'deleted']) {
    db.prepare('UPDATE users SET status=? WHERE id=?').run(value, id);
    assert.equal(await authenticate(request('/', 'GET', undefined, login.raw), env), null);
    assert.equal(await authenticate(request('/', 'GET', undefined, api.raw), env), null);
  }
  db.prepare("UPDATE users SET status='active' WHERE id=?").run(id);
  db.prepare('UPDATE tokens SET revoked_at=?').run(new Date().toISOString());
  assert.equal(await authenticate(request('/', 'GET', undefined, api.raw), env), null);
  db.prepare('UPDATE tokens SET revoked_at=NULL,expires_at=?').run(past());
  db.prepare('UPDATE login_sessions SET expires_at=?').run(past());
  assert.equal(await authenticate(request('/', 'GET', undefined, api.raw), env), null);
  assert.equal(await authenticate(request('/', 'GET', undefined, login.raw), env), null);
  db.prepare("UPDATE login_sessions SET expires_at='invalid'").run();
  assert.equal(await authenticate(request('/', 'GET', undefined, login.raw), env), null);
  assert.equal(await authenticate(request('/', 'GET', undefined, credential('gs_')), env), null);
});

test('database failures in authenticate and limiter propagate, never become anonymous', async () => {
  const fault = new Error('database offline');
  const env = { DB: { prepare() { throw fault; } } } as unknown as Env;
  await assert.rejects(authenticate(request('/', 'GET', undefined, credential('ls_')), env), error => error === fault);
  await assert.rejects(authenticate(request('/', 'GET', undefined, credential('gs_')), env), error => error === fault);
  await assert.rejects(rateLimit(env, 'any', 1, 60), error => error === fault);
});

test('D1 limiter is atomic across concurrent attempts, hashes keys and resets expired windows', async t => {
  const { db, env } = fixture(t);
  const key = 'login:ip:192.0.2.42';
  const results = await Promise.allSettled(Array.from({ length: 12 }, () => rateLimit(env, key, 3, 600)));
  assert.equal(results.filter(result => result.status === 'fulfilled').length, 3);
  assert.equal(results.filter(result => result.status === 'rejected' && status(429)(result.reason)).length, 9);
  const row = db.prepare('SELECT * FROM rate_limits').get()!;
  assert.match(String(row.key), /^[a-f0-9]{64}$/);
  assert.ok(!String(row.key).includes('192.0.2.42'));
  assert.equal(row.count, 4); // saturated at limit + 1, not unbounded
  db.prepare('UPDATE rate_limits SET expires_at=0').run();
  await rateLimit(env, key, 3, 600);
  assert.equal(db.prepare('SELECT count FROM rate_limits').get()!.count, 1);
  await rateLimit(env, 'different', 1, 600);
  await assert.rejects(rateLimit(env, 'different', 1, 600), status(429));
});

test('registration returns legacy pending shape, is case-insensitively unique and grants no credentials', async t => {
  const { db, env } = fixture(t);
  const response = await handleAuth(request('/api/v1/auth/register', 'POST', { name: ' New_Player ', password }), env, null);
  assert.equal(response?.status, 201);
  assert.deepEqual(await payload(response), { name: 'New_Player', status: 'pending' });
  assert.equal(response?.headers.get('Cache-Control'), 'private, no-store');
  assert.equal(rowCount(db, 'login_sessions'), 0);
  assert.equal(rowCount(db, 'tokens'), 0);
  const user = db.prepare('SELECT * FROM users').get()!;
  assert.ok(user.password_hash !== password);
  assert.ok(await verifyPassword(String(user.password_hash), password));
  await assert.rejects(handleAuth(request('/api/v1/auth/register', 'POST', { name: 'new_player', password }), env, null), status(409));
  for (const input of [{ name: 'ab', password }, { name: 'valid', password: 'short' }, { name: 'valid', password, status: 'active' }]) {
    await assert.rejects(handleAuth(request('/api/v1/auth/register', 'POST', input), env, null), status(400));
  }
});

test('login returns a 12h hashed session for pending/active; unknown, wrong and deleted are indistinguishable', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db, 'Pending_Player', 'pending');
  const response = await handleAuth(request('/api/v1/auth/login', 'POST', { name: ' pending_player ', password }), env, null);
  assert.equal(response?.status, 200);
  const result = await payload(response);
  assert.equal(result.name, 'Pending_Player');
  assert.equal(result.status, 'pending');
  assert.ok(/^ls_[A-Za-z0-9_-]{43}$/.test(result.sessionToken));
  const row = db.prepare('SELECT * FROM login_sessions').get()!;
  assert.ok(row.hash === await digest(result.sessionToken));
  assert.equal(Date.parse(String(row.expires_at)) - Date.parse(String(row.created_at)), 43_200_000);
  for (const input of [{ name: 'Unknown_Player', password }, { name: 'Pending_Player', password: 'incorrect-password' }]) {
    await assert.rejects(handleAuth(request('/api/v1/auth/login', 'POST', input), env, null), status(401, 'invalid credentials'));
  }
  db.prepare("UPDATE users SET status='deleted' WHERE id=?").run(id);
  await assert.rejects(handleAuth(request('/api/v1/auth/login', 'POST', { name: 'Pending_Player', password }), env, null), status(401, 'invalid credentials'));
  db.prepare("UPDATE users SET status='rejected' WHERE id=?").run(id);
  await assert.rejects(handleAuth(request('/api/v1/auth/login', 'POST', { name: 'Pending_Player', password }), env, null), status(403, 'account was rejected'));
});

test('login/register enforce both trusted client and case-insensitive account rate buckets', async t => {
  const { db, env } = fixture(t);
  await rateLimit(env, 'auth:login:name:alice', 10, 600);
  const storedName = await digest('rate-limit:v1:auth:login:name:alice');
  db.prepare('UPDATE rate_limits SET count=10 WHERE key=?').run(storedName);
  const accountReq = request('/api/v1/auth/login', 'POST', { name: 'ALICE', password });
  accountReq.headers.set('CF-Connecting-IP', '198.51.100.8');
  await assert.rejects(handleAuth(accountReq, env, null), status(429));
  await rateLimit(env, 'auth:register:ip:192.0.2.42', 5, 600);
  const storedIP = await digest('rate-limit:v1:auth:register:ip:192.0.2.42');
  db.prepare('UPDATE rate_limits SET count=5 WHERE key=?').run(storedIP);
  const req = request('/api/v1/auth/register', 'POST', { name: 'Other', password });
  req.headers.set('X-Forwarded-For', '203.0.113.7');
  req.headers.set('X-Real-IP', '203.0.113.7');
  await assert.rejects(handleAuth(req, env, null), status(429));
  assert.equal(rowCount(db, 'users'), 0);
});

test('me and token management keep legacy shapes and never expose stored digests', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  const me = await payload(await handleAuth(request('/api/v1/me'), env, login.identity));
  assert.deepEqual(Object.keys(me).sort(), ['createdAt', 'id', 'name', 'status']);
  assert.equal(me.id, id);
  const response = await handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, login.identity);
  assert.equal(response?.status, 201);
  const issued = await payload(response);
  assert.deepEqual(Object.keys(issued).sort(), ['expiresAt', 'id', 'token']);
  assert.ok(/^gs_[A-Za-z0-9_-]{43}$/.test(issued.token));
  assert.match(issued.id, /^[0-9a-f]{32}$/);
  const row = db.prepare('SELECT * FROM tokens').get()!;
  assert.ok(row.hash === await digest(issued.token));
  assert.equal(Date.parse(String(row.expires_at)) - Date.parse(String(row.created_at)), 720 * 3_600_000);
  const list = await payload(await handleAuth(request('/api/v1/me/tokens'), env, login.identity));
  assert.equal(list.tokens.length, 1);
  assert.deepEqual(Object.keys(list.tokens[0]).sort(), ['createdAt', 'expiresAt', 'id']);
  const apiIdentity = await authenticate(request('/', 'GET', undefined, issued.token), env);
  assert.equal((await handleAuth(request('/api/v1/me'), env, apiIdentity))?.status, 200);
  for (const [path, method, body] of [
    ['/api/v1/me/tokens', 'GET', undefined], ['/api/v1/me/tokens', 'POST', {}],
    [`/api/v1/me/tokens/${issued.id}`, 'DELETE', undefined],
    ['/api/v1/me', 'DELETE', { password, confirmName: 'Alice' }], ['/api/v1/auth/logout', 'POST', undefined],
  ] as const) {
    await assert.rejects(handleAuth(request(path, method, body), env, apiIdentity), status(401));
  }
  const revoked = await handleAuth(request(`/api/v1/me/tokens/${issued.id}`, 'DELETE'), env, login.identity);
  assert.equal(revoked?.status, 204);
  assert.equal(await revoked?.text(), '');
  assert.equal(await authenticate(request('/', 'GET', undefined, issued.token), env), null);
  const after = await payload(await handleAuth(request('/api/v1/me/tokens'), env, login.identity));
  assert.equal(typeof after.tokens[0].revokedAt, 'string');
  await assert.rejects(handleAuth(request(`/api/v1/me/tokens/${issued.id}`, 'DELETE'), env, login.identity), status(404));
});

test('token TTL accepts only integral 1..8760 and pending sessions cannot issue', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  for (const ttlHours of [0, -1, 8761, 1.5, '1', true]) {
    await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', { ttlHours }), env, login.identity), status(400));
  }
  for (const ttlHours of [1, 8760]) {
    const issued = await payload(await handleAuth(request('/api/v1/me/tokens', 'POST', { ttlHours }), env, login.identity));
    const token = db.prepare('SELECT * FROM tokens WHERE id=?').get(issued.id)!;
    assert.equal(Date.parse(String(token.expires_at)) - Date.parse(String(token.created_at)), ttlHours * 3_600_000);
  }
  db.prepare("UPDATE users SET status='pending' WHERE id=?").run(id);
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, { ...login.identity, status: 'pending' }), status(403));
  // Even a previously authenticated active identity cannot bypass a later status change.
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, login.identity), status(403));
});

test('own-token scoping and logout only revoke the current session', async t => {
  const { db, env } = fixture(t);
  const alice = addUser(db);
  const bob = addUser(db, 'Bob');
  const first = await session(db, alice);
  const second = await session(db, alice);
  const otherToken = await apiToken(db, bob);
  await assert.rejects(handleAuth(request(`/api/v1/me/tokens/${otherToken.id}`, 'DELETE'), env, first.identity), status(404));
  assert.ok(await authenticate(request('/', 'GET', undefined, otherToken.raw), env));
  assert.equal((await handleAuth(request('/api/v1/auth/logout', 'POST'), env, first.identity))?.status, 204);
  assert.equal(await authenticate(request('/', 'GET', undefined, first.raw), env), null);
  assert.ok(await authenticate(request('/', 'GET', undefined, second.raw), env));
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, first.identity), status(403));
});

test('closure requires independent name/password confirmation and atomically preserves content', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  const api = await apiToken(db, id);
  addContentAndSessions(db, id, api.id);
  await assert.rejects(handleAuth(request('/api/v1/me', 'DELETE', { password, confirmName: 'Bob' }), env, login.identity), status(400));
  await assert.rejects(handleAuth(request('/api/v1/me', 'DELETE', { password: 'incorrect-password', confirmName: 'Alice' }), env, login.identity), status(403));
  assert.equal(rowCount(db, 'tokens'), 1);
  const response = await handleAuth(request('/api/v1/me', 'DELETE', { password, confirmName: ' alice ' }), env, login.identity);
  assert.equal(response?.status, 204);
  assert.equal(await response?.text(), '');
  for (const table of ['tokens', 'login_sessions', 'play_sessions', 'device_sessions']) assert.equal(rowCount(db, table), 0);
  for (const table of ['users', 'games', 'player_scores', 'score_submissions']) assert.equal(rowCount(db, table), 1);
  const user = db.prepare('SELECT * FROM users WHERE id=?').get(id)!;
  assert.equal(user.name, 'Alice');
  assert.equal(user.status, 'deleted');
  assert.equal(typeof user.deleted_at, 'string');
  assert.ok(user.password_hash !== passwordHash);
  assert.ok(!await verifyPassword(String(user.password_hash), password));
  assert.equal(await authenticate(request('/', 'GET', undefined, login.raw), env), null);
  assert.equal(await authenticate(request('/', 'GET', undefined, api.raw), env), null);
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, login.identity), status(403));
  await assert.rejects(handleAuth(request('/api/v1/auth/register', 'POST', { name: 'alice', password }), env, null), status(409));
});

test('account closure rolls back tombstone and credential deletion when a batch statement fails', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  await apiToken(db, id);
  db.exec("CREATE TRIGGER reject_delete BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT,'test failure'); END;");
  await assert.rejects(handleAuth(request('/api/v1/me', 'DELETE', { password, confirmName: 'Alice' }), env, login.identity));
  assert.equal(db.prepare('SELECT status FROM users WHERE id=?').get(id)!.status, 'active');
  assert.ok(db.prepare('SELECT password_hash FROM users WHERE id=?').get(id)!.password_hash === passwordHash);
  assert.equal(rowCount(db, 'tokens'), 1);
  assert.equal(rowCount(db, 'login_sessions'), 1);
});

test('a close/logout race cannot close with a stale session or delete any credentials', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const first = await session(db, id);
  await session(db, id);
  await apiToken(db, id);
  db.prepare('DELETE FROM login_sessions WHERE hash=?').run(first.identity.hash);
  await assert.rejects(handleAuth(request('/api/v1/me', 'DELETE', { password, confirmName: 'Alice' }), env, first.identity), status(401));
  assert.equal(db.prepare('SELECT status FROM users').get()!.status, 'active');
  assert.equal(rowCount(db, 'tokens'), 1);
  assert.equal(rowCount(db, 'login_sessions'), 1);
});

test('concurrent account closures have one winner and no surviving credentials', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  await apiToken(db, id);
  const results = await Promise.allSettled([1, 2].map(() => handleAuth(
    request('/api/v1/me', 'DELETE', { password, confirmName: 'Alice' }), env, login.identity)));
  assert.equal(results.filter(result => result.status === 'fulfilled' && result.value?.status === 204).length, 1);
  assert.equal(results.filter(result => result.status === 'rejected' && status(401)(result.reason)).length, 1);
  assert.equal(rowCount(db, 'tokens'), 0);
  assert.equal(rowCount(db, 'login_sessions'), 0);
});

test('login issuance rechecks the account after password verification', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const prepare = env.DB.prepare.bind(env.DB);
  env.DB.prepare = ((sql: string) => {
    if (sql.startsWith('INSERT INTO login_sessions')) {
      db.prepare("UPDATE users SET status='deleted' WHERE id=?").run(id);
    }
    return prepare(sql);
  }) as typeof env.DB.prepare;
  await assert.rejects(handleAuth(request('/api/v1/auth/login', 'POST', { name: 'Alice', password }), env, null), status(401));
  assert.equal(rowCount(db, 'login_sessions'), 0);
});

test('token revocation destroys derived play/device credentials without touching content', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  const api = await apiToken(db, id);
  addContentAndSessions(db, id, api.id);
  assert.equal((await handleAuth(request(`/api/v1/me/tokens/${api.id}`, 'DELETE'), env, login.identity))?.status, 204);
  assert.equal(rowCount(db, 'play_sessions'), 0);
  assert.equal(rowCount(db, 'device_sessions'), 0);
  assert.equal(rowCount(db, 'tokens'), 1);
  for (const table of ['games', 'player_scores', 'score_submissions']) assert.equal(rowCount(db, table), 1);
});

test('pending users can close their account without administrator approval', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db, 'Pending', 'pending');
  const login = await session(db, id);
  assert.equal((await handleAuth(request('/api/v1/me', 'DELETE', { password, confirmName: 'Pending' }), env, login.identity))?.status, 204);
  assert.equal(db.prepare('SELECT status FROM users').get()!.status, 'deleted');
  assert.equal(rowCount(db, 'login_sessions'), 0);
});

test('token issuance rechecks current session expiration and account tombstone', async t => {
  const { db, env } = fixture(t);
  const id = addUser(db);
  const login = await session(db, id);
  db.prepare('UPDATE login_sessions SET expires_at=?').run(past());
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, login.identity), status(403));
  db.prepare('UPDATE login_sessions SET expires_at=?').run(future());
  db.prepare('UPDATE users SET deleted_at=? WHERE id=?').run(new Date().toISOString(), id);
  await assert.rejects(handleAuth(request('/api/v1/me/tokens', 'POST', {}), env, login.identity), status(403));
  assert.equal(rowCount(db, 'tokens'), 0);
});

test('absent trusted client header cannot be replaced with a caller supplied forwarding header', async t => {
  const { db, env } = fixture(t);
  await rateLimit(env, 'auth:login:ip:unknown', 20, 600);
  db.prepare('UPDATE rate_limits SET count=20').run();
  const req = request('/api/v1/auth/login', 'POST', { name: 'Alice', password });
  req.headers.delete('CF-Connecting-IP');
  req.headers.set('X-Forwarded-For', '203.0.113.77');
  await assert.rejects(handleAuth(req, env, null), status(429));
});

test('registration and account-management database faults are not disguised as client errors', async () => {
  const fault = new Error('database offline');
  const env = { DB: { prepare() { throw fault; } } } as unknown as Env;
  await assert.rejects(handleAuth(request('/api/v1/auth/register', 'POST', { name: 'Alice', password }), env, null), error => error === fault);
  const identity: Identity = { userId: 1, name: 'Alice', status: 'active', kind: 'login', hash: 'synthetic-session-digest' };
  await assert.rejects(handleAuth(request('/api/v1/me/tokens'), env, identity), error => error === fault);
  await assert.rejects(handleAuth(request('/api/v1/me'), env, identity), error => error === fault);
});

test('auth route boundaries reject methods/oversized bodies and expose no admin endpoint', async t => {
  const { env } = fixture(t);
  assert.equal(await handleAuth(request('/api/v1/admin/approve', 'POST', {}), env, null), null);
  assert.equal(await handleAuth(request('/api/v1/games'), env, null), null);
  await assert.rejects(handleAuth(request('/api/v1/auth/login'), env, null), status(405));
  await assert.rejects(handleAuth(request('/api/v1/me'), env, null), status(401));
  await assert.rejects(handleAuth(request('/api/v1/auth/register', 'POST', { name: 'Alice', password: 'x'.repeat(5000) }), env, null), status(413));
  for (const value of ['null', '[]', '{}{}', '{']) {
    const req = new Request('https://example.test/api/v1/auth/register', { method: 'POST', body: value });
    await assert.rejects(handleAuth(req, env, null), status(400));
  }
});
