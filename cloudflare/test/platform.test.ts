import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { after, before, test } from 'node:test';
import { Miniflare } from 'miniflare';
import { handlePlatform } from '../src/platform';
import { credential, digest, HttpError, type Env, type Identity } from '../src/types';

// Real local workerd/D1 SQL and transactions, with an ephemeral database. This
// does not exercise a deployed Cloudflare service, router, CORS or browser UI.
let mf: Miniflare;
let DB: D1Database;
let sequence = 0;
before(async () => {
  mf = new Miniflare({
    modules: true, compatibilityDate: '2026-08-06',
    script: 'export default { fetch() { return new Response("local platform tests") } }',
    d1Databases: ['DB'],
  });
  DB = await mf.getD1Database('DB') as unknown as D1Database;
  const schema = readFileSync(new URL('../migrations/0001_native.sql', import.meta.url), 'utf8');
  await DB.batch(schema.split(';').map(sql => sql.trim()).filter(Boolean).map(sql => DB.prepare(sql)));
});
after(async () => { await mf?.dispose(); });

const future = (hours = 3) => new Date(Date.now() + hours * 3_600_000).toISOString();
const past = () => new Date(Date.now() - 60_000).toISOString();

async function fixture() {
  const serial = ++sequence;
  const now = new Date().toISOString();
  const actors: Array<{ raw: string; identity: Identity }> = [];
  for (const name of ['owner', 'player', 'third']) {
    const user = await DB.prepare(`INSERT INTO users (name,status,created_at,updated_at)
      VALUES (?,'active',?,?) RETURNING id`).bind(`${name}-${serial}`, now, now).first<{ id: number }>();
    const raw = credential('gs_');
    const hash = await digest(raw);
    const tokenId = `token-${serial}-${name}`;
    await DB.prepare('INSERT INTO tokens (id,user_id,hash,expires_at,created_at) VALUES (?,?,?,?,?)')
      .bind(tokenId, user!.id, hash, future(), now).run();
    actors.push({ raw, identity: { userId: user!.id, name: `${name}-${serial}`, status: 'active', kind: 'api', tokenId, hash } });
  }
  const [owner, player, third] = actors;
  const game = `public-${serial}`;
  const privateGame = `private-${serial}`;
  for (const [id, visibility] of [[game, 'public'], [privateGame, 'private']]) {
    await DB.prepare(`INSERT INTO games (id,owner_id,visibility,metadata,package_key,size,sha256,created_at,updated_at)
      VALUES (?,?,?,'{}','test-package',1,?,?,?)`).bind(id, owner.identity.userId, visibility, 'a'.repeat(64), now, now).run();
  }
  const env = { DB, PUBLIC_BASE_URL: 'https://platform.example.test', MAX_PACKAGE_BYTES: '536870912' } as Env;
  async function play(actor = player, gameId = game) {
    const raw = credential('ps_');
    await DB.prepare(`INSERT INTO play_sessions (hash,user_id,game_id,source_token_id,expires_at,created_at)
      VALUES (?,?,?,?,?,?)`).bind(await digest(raw), actor.identity.userId, gameId, actor.identity.tokenId!, future(), now).run();
    return raw;
  }
  return { env, owner, player, third, game, privateGame, play, serial };
}

function request(path: string, method = 'POST', body?: unknown, playToken?: string, headers: HeadersInit = {}): Request {
  const h = new Headers(headers);
  if (body !== undefined) h.set('Content-Type', 'application/json');
  if (playToken) h.set('X-Play-Token', playToken);
  return new Request(`https://untrusted-request-host.test${path}`, {
    method, headers: h, ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}
const route = (game: string, name: string) => `/api/v1/games/${game}/${name}`;
const payload = (value = 100, submissionId = 'a'.repeat(16), extra: object = {}) =>
  ({ board: 'score', value, submissionId, ...extra });
async function invoke(req: Request, env: Env, identity: Identity | null = null) {
  const response = await handlePlatform(req, env, identity);
  assert.ok(response);
  return { status: response.status, headers: response.headers, body: await response.json() as any };
}
async function error(status: number, callback: () => Promise<unknown>) {
  await assert.rejects(callback, (err: unknown) => err instanceof HttpError && err.status === status);
}
async function resetLimits() { await DB.prepare('DELETE FROM rate_limits').run(); }
async function device(f: Awaited<ReturnType<typeof fixture>>) {
  return (await invoke(request(route(f.game, 'device-sessions')), f.env)).body;
}
async function authorize(f: Awaited<ReturnType<typeof fixture>>, code: string, identity = f.player.identity) {
  return invoke(request('/api/v1/device-sessions/authorize', 'POST', { userCode: code }), f.env, identity);
}
async function poll(f: Awaited<ReturnType<typeof fixture>>, token: string) {
  return invoke(request(route(f.game, 'device-sessions/poll'), 'POST', { deviceToken: token }), f.env);
}

// Run cases serially: individual cases deliberately submit concurrent requests,
// while the suite shares one ephemeral workerd process to keep startup bounded.
test('native platform contract and atomic transactions', async t => {
  await t.test('routing is explicit, bounded and does not claim unrelated endpoints', async () => {
    const f = await fixture();
    assert.equal(await handlePlatform(request('/api/v1/unrelated'), f.env, null), null);
    await error(405, () => handlePlatform(request(route(f.game, 'scores'), 'GET'), f.env, null));
    await error(405, () => handlePlatform(request(route(f.game, 'leaderboard'), 'POST'), f.env, null));
    await error(404, () => handlePlatform(request(route('BAD-ID', 'scores')), f.env, null));
    await error(414, () => handlePlatform(request(`${route(f.game, 'leaderboard')}?x=${'x'.repeat(4096)}`, 'GET'), f.env, null));
  });

  await t.test('play creation requires active gs identity and a live source token', async () => {
    const f = await fixture();
    const create = (identity: Identity | null, body?: unknown) =>
      handlePlatform(request(route(f.game, 'play-sessions'), 'POST', body), f.env, identity);
    await error(401, () => create(null));
    await error(401, () => create({ ...f.player.identity, kind: 'login' }));
    await error(401, () => create({ ...f.player.identity, status: 'pending' }));
    await error(401, () => create({ ...f.player.identity, tokenId: undefined }));
    await error(400, () => create(f.player.identity, { unexpected: true }));
    await error(413, () => create(f.player.identity, { pad: 'x'.repeat(1024) }));
    const sourceExpiry = future(0.5);
    await DB.prepare('UPDATE tokens SET expires_at=? WHERE id=?').bind(sourceExpiry, f.player.identity.tokenId!).run();
    const created = await invoke(request(route(f.game, 'play-sessions')), f.env, f.player.identity);
    assert.equal(created.status, 201);
    assert.match(created.body.token, /^ps_[A-Za-z0-9_-]{43}$/);
    assert.equal(created.body.expiresAt, sourceExpiry);
    assert.equal(created.body.gameId, f.game);
    assert.equal(created.headers.get('Cache-Control'), 'no-store');
    const stored = await DB.prepare('SELECT * FROM play_sessions WHERE hash=?').bind(await digest(created.body.token)).first();
    assert.ok(stored);
    assert.ok(!JSON.stringify(stored).includes(created.body.token));
    await DB.prepare('UPDATE tokens SET revoked_at=? WHERE id=?').bind(new Date().toISOString(), f.player.identity.tokenId!).run();
    await error(401, () => create(f.player.identity)); // stale identity cannot authorize
  });

  await t.test('private games are owner-only, including scoped tokens and empty boards', async () => {
    const f = await fixture();
    await error(404, () => handlePlatform(request(route(f.privateGame, 'play-sessions')), f.env, f.player.identity));
    const owner = await invoke(request(route(f.privateGame, 'play-sessions')), f.env, f.owner.identity);
    assert.equal(owner.status, 201);
    for (const identity of [null, f.player.identity]) {
      await error(404, () => handlePlatform(request(route(f.privateGame, 'leaderboard'), 'GET'), f.env, identity));
    }
    assert.deepEqual((await invoke(request(route(f.privateGame, 'leaderboard'), 'GET'), f.env, f.owner.identity)).body.entries, []);
    const strangerToken = await f.play(f.player, f.privateGame);
    await error(404, () => handlePlatform(request(route(f.privateGame, 'scores'), 'POST', payload(), strangerToken), f.env, null));
    await invoke(request(route(f.privateGame, 'scores'), 'POST', payload(), owner.body.token), f.env);
    await error(404, () => handlePlatform(request(route(f.privateGame, 'leaderboard'), 'GET'), f.env, null));
    assert.equal((await invoke(request(route(f.privateGame, 'leaderboard'), 'GET'), f.env, f.owner.identity)).body.entries[0].value, 100);
  });

  await t.test('score payloads reject coercions, unknown keys, unsafe values, modes and IDs', async () => {
    const f = await fixture();
    const token = await f.play();
    const submit = (body: unknown, raw = token) => handlePlatform(request(route(f.game, 'scores'), 'POST', body, raw), f.env, null);
    for (const value of [null, true, '10', -1, 0.1, Number.MAX_SAFE_INTEGER + 1]) {
      await error(400, () => submit(payload(value as number)));
    }
    for (const bad of [null, [], {}, { board: 'score', submissionId: 'a'.repeat(16) },
      payload(1, 'a'.repeat(15)), payload(1, 'a'.repeat(65)), payload(1, 'a'.repeat(15) + '.'),
      payload(1, 'a'.repeat(16), { board: 'duration' }), payload(0, 'a'.repeat(16), { board: 'time-ms' }),
      payload(1, 'a'.repeat(16), { mode: null }), payload(1, 'a'.repeat(16), { mode: 1 }),
      payload(1, 'a'.repeat(16), { mode: 'Upper' }), payload(1, 'a'.repeat(16), { mode: 'a'.repeat(65) }),
      payload(1, 'a'.repeat(16), { extra: true })]) await error(400, () => submit(bad));
    await error(413, () => submit({ pad: 'x'.repeat(4096) }));
    for (const raw of [f.player.raw, token + ', ' + token, 'ps_bad', token.slice(0, -1) + '!']) await error(401, () => submit(payload(), raw));
    await error(401, () => handlePlatform(request(route(f.game, 'scores'), 'POST', payload()), f.env, null));
    const accepted = await invoke(request(route(f.game, 'scores'), 'POST', payload(0, '0'.repeat(16)), token), f.env);
    assert.equal(accepted.body.value, 0);
  });

  await t.test('score and time boards retain strict bests and default/explicit modes', async () => {
    const f = await fixture();
    const token = await f.play();
    const submit = (value: number, id: string, extra = {}) => invoke(request(route(f.game, 'scores'), 'POST', payload(value, id.repeat(16), extra), token), f.env);
    const first = await submit(10, 'a');
    assert.equal(first.body.mode, 'default');
    const equal = await submit(10, 'b', { mode: '' });
    const worse = await submit(5, 'c');
    assert.deepEqual(equal.body, first.body);
    assert.deepEqual(worse.body, first.body);
    assert.equal((await submit(Number.MAX_SAFE_INTEGER, 'd')).body.value, Number.MAX_SAFE_INTEGER);
    assert.equal((await submit(500, 'e', { board: 'time-ms' })).body.value, 500);
    assert.equal((await submit(800, 'f', { board: 'time-ms' })).body.value, 500);
    assert.equal((await submit(1, 'g', { board: 'time-ms' })).body.value, 1);
    assert.equal((await submit(50, 'h', { mode: 'hard' })).body.mode, 'hard');
    const rows = await DB.prepare('SELECT board,mode,value FROM player_scores WHERE game_id=? ORDER BY board,mode').bind(f.game).all();
    assert.equal(rows.results.length, 3);
  });

  await t.test('concurrent identical submissions return one immutable response snapshot', async () => {
    const f = await fixture();
    const token = await f.play();
    const body = payload(100, 'same-logical-id-01');
    const submit = () => invoke(request(route(f.game, 'scores'), 'POST', body, token), f.env);
    const responses = await Promise.all(Array.from({ length: 12 }, submit));
    for (const response of responses) assert.deepEqual(response.body, responses[0].body);
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM score_submissions WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 1);
    const improved = await invoke(request(route(f.game, 'scores'), 'POST', payload(200, 'next-logical-id-02'), token), f.env);
    assert.equal(improved.body.value, 200);
    assert.deepEqual((await submit()).body, responses[0].body); // original snapshot, not latest best
    assert.equal((await DB.prepare('SELECT value FROM player_scores WHERE game_id=?').bind(f.game).first<{ value: number }>())!.value, 200);
  });

  await t.test('concurrent mismatched replay has one winner, conflicts never alter bests', async () => {
    const f = await fixture();
    const token = await f.play();
    const values = [10, 900];
    const results = await Promise.allSettled(values.map(value => invoke(
      request(route(f.game, 'scores'), 'POST', payload(value, 'conflict-id-00001'), token), f.env)));
    const fulfilled = results.filter(result => result.status === 'fulfilled');
    const rejected = results.filter(result => result.status === 'rejected');
    assert.equal(fulfilled.length, 1); assert.equal(rejected.length, 1);
    assert.ok(rejected[0].reason instanceof HttpError); assert.equal(rejected[0].reason.status, 409);
    const saved = await DB.prepare('SELECT value FROM player_scores WHERE game_id=?').bind(f.game).first<{ value: number }>();
    assert.equal(saved!.value, fulfilled[0].value.body.value);
    for (const extra of [{ mode: 'hard' }, { board: 'time-ms' }]) {
      await error(409, () => handlePlatform(request(route(f.game, 'scores'), 'POST', payload(saved!.value, 'conflict-id-00001', extra), token), f.env, null));
    }
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM player_scores WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 1);
  });

  await t.test('distinct concurrent submissions do not lose a higher score or faster time', async () => {
    const f = await fixture();
    const token = await f.play();
    const values = [50, 2, 800, 17, 399, 1];
    await Promise.all(values.flatMap((value, i) => ['score', 'time-ms'].map(board => invoke(
      request(route(f.game, 'scores'), 'POST', payload(value, `${board}-${String(i).padStart(16, '0')}`, { board }), token), f.env))));
    const rows = (await DB.prepare('SELECT board,value FROM player_scores WHERE game_id=? ORDER BY board').bind(f.game).all()).results;
    assert.deepEqual(rows, [{ board: 'score', value: 800 }, { board: 'time-ms', value: 1 }]);
  });

  await t.test('a failed D1 batch rolls back both idempotency reservation and best score', async () => {
    const f = await fixture();
    const token = await f.play();
    const broken = { ...f.env, DB: new Proxy(DB, { get(target, property) {
      if (property === 'batch') return (statements: D1PreparedStatement[]) =>
        target.batch([...statements, target.prepare('SELECT missing_platform_test_column FROM games')]);
      const value = Reflect.get(target, property); return typeof value === 'function' ? value.bind(target) : value;
    } }) };
    await error(503, () => handlePlatform(request(route(f.game, 'scores'), 'POST', payload(), token), broken, null));
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM score_submissions WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 0);
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM player_scores WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 0);
  });

  await t.test('every score checks game binding, current user and both credential lifetimes', async () => {
    const f = await fixture();
    const token = await f.play();
    const submit = () => handlePlatform(request(route(f.game, 'scores'), 'POST', payload(), token), f.env, null);
    await submit(); // also ensure replay is not an authentication bypass
    await DB.prepare('UPDATE tokens SET revoked_at=? WHERE id=?').bind(new Date().toISOString(), f.player.identity.tokenId!).run();
    await error(401, submit);
    await DB.prepare('UPDATE tokens SET revoked_at=NULL,expires_at=? WHERE id=?').bind(past(), f.player.identity.tokenId!).run();
    await error(401, submit);
    await DB.prepare('UPDATE tokens SET expires_at=? WHERE id=?').bind(future(), f.player.identity.tokenId!).run();
    for (const status of ['pending', 'rejected', 'deleted']) {
      await DB.prepare('UPDATE users SET status=? WHERE id=?').bind(status, f.player.identity.userId).run();
      await error(401, submit);
    }
    await DB.prepare("UPDATE users SET status='active' WHERE id=?").bind(f.player.identity.userId).run();
    await DB.prepare('UPDATE play_sessions SET expires_at=? WHERE hash=?').bind(past(), await digest(token)).run();
    await error(401, submit);
    const fresh = await f.play();
    await error(401, () => handlePlatform(request(route(f.privateGame, 'scores'), 'POST', payload(), fresh), f.env, null));
    await DB.prepare("UPDATE games SET visibility='private' WHERE id=?").bind(f.game).run();
    await error(404, () => handlePlatform(request(route(f.game, 'scores'), 'POST', payload(), fresh), f.env, null));
  });

  await t.test('leaderboard bounds and stable competition ranks are correct', async () => {
    const f = await fixture();
    const tokens = await Promise.all([f.owner, f.player, f.third].map(actor => f.play(actor)));
    for (let i = 0; i < tokens.length; i++) {
      await invoke(request(route(f.game, 'scores'), 'POST', payload(i < 2 ? 100 : 50, 'rank-id-000000001'), tokens[i]), f.env);
      await invoke(request(route(f.game, 'scores'), 'POST', payload(i < 2 ? 25 : 50, 'rank-id-000000002', { board: 'time-ms' }), tokens[i]), f.env);
    }
    // Pin timestamps to make tie-break order deterministic independent of clock resolution.
    await DB.prepare('UPDATE player_scores SET achieved_at=? WHERE game_id=? AND user_id=?')
      .bind('2026-01-01T00:00:00.000Z', f.game, f.owner.identity.userId).run();
    for (const board of ['score', 'time-ms']) {
      const result = await invoke(request(`${route(f.game, 'leaderboard')}?board=${board}`, 'GET'), f.env);
      assert.deepEqual(result.body.entries.map((entry: any) => entry.rank), [1, 1, 3]);
      assert.equal(result.body.entries[0].playerName, f.owner.identity.name);
      assert.deepEqual(Object.keys(result.body.entries[0]).sort(), ['achievedAt', 'playerName', 'rank', 'value']);
    }
    assert.equal((await invoke(request(`${route(f.game, 'leaderboard')}?limit=1`, 'GET'), f.env)).body.entries.length, 1);
    assert.deepEqual((await invoke(request(`${route(f.game, 'leaderboard')}?mode=unplayed`, 'GET'), f.env)).body.entries, []);
    for (const query of ['limit=0', 'limit=101', 'limit=-1', 'limit=1.5', 'limit=', 'limit=1e2', 'limit=1&limit=2',
      'board=bogus', 'board=score&board=score', 'mode=BAD', 'mode=a&mode=a']) {
      await error(400, () => handlePlatform(request(`${route(f.game, 'leaderboard')}?${query}`, 'GET'), f.env, null));
    }
    await DB.prepare('UPDATE games SET deleted=1 WHERE id=?').bind(f.game).run();
    await error(404, () => handlePlatform(request(route(f.game, 'leaderboard'), 'GET'), f.env, f.owner.identity));
  });

  await t.test('device creation uses only configured HTTPS origin and stores no plaintext secret', async () => {
    await resetLimits();
    const f = await fixture();
    for (const origin of ['', 'http://platform.example.test', 'https://user:pass@example.test', 'https://example.test/path', 'https://example.test/?x=1']) {
      await error(503, () => handlePlatform(request(route(f.game, 'device-sessions')), { ...f.env, PUBLIC_BASE_URL: origin }, null));
    }
    const before = Date.now();
    const created = await invoke(request(route(f.game, 'device-sessions'), 'POST', {}, undefined,
      { Host: 'attacker.invalid', Origin: 'https://attacker.invalid', 'X-Forwarded-Host': 'attacker.invalid' }), f.env);
    const d = created.body;
    assert.equal(created.status, 201); assert.equal(d.pollInterval, 5);
    assert.match(d.deviceToken, /^gd_[A-Za-z0-9_-]{43}$/);
    assert.match(d.userCode, /^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{8}$/);
    assert.equal(d.verificationUrl, `https://platform.example.test/device-connect?code=${d.userCode}`);
    assert.ok(Date.parse(d.expiresAt) >= before + 600_000 && Date.parse(d.expiresAt) <= Date.now() + 600_000);
    const saved = await DB.prepare('SELECT * FROM device_sessions WHERE hash=?').bind(await digest(d.deviceToken)).first();
    assert.ok(saved); assert.ok(!JSON.stringify(saved).includes(d.deviceToken));
    await error(404, () => handlePlatform(request(route(f.privateGame, 'device-sessions')), f.env, f.owner.identity));
    await error(400, () => handlePlatform(request(route(f.game, 'device-sessions'), 'POST', { extra: true }), f.env, null));
    await error(413, () => handlePlatform(request(route(f.game, 'device-sessions'), 'POST', { pad: 'a'.repeat(1024) }), f.env, null));
  });

  await t.test('device creation capacity is atomic under concurrency', async () => {
    await resetLimits();
    const f = await fixture();
    const results = await Promise.allSettled(Array.from({ length: 14 }, () => device(f)));
    assert.equal(results.filter(result => result.status === 'fulfilled').length, 10);
    for (const result of results) if (result.status === 'rejected') {
      assert.ok(result.reason instanceof HttpError); assert.equal(result.reason.status, 429);
    }
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM device_sessions WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 10);
  });

  await t.test('pending device polls throttle and authorization requires gs, not a browser login', async () => {
    await resetLimits();
    const f = await fixture();
    const d = await device(f);
    const waiting = await poll(f, d.deviceToken);
    assert.equal(waiting.status, 202); assert.deepEqual(waiting.body, { status: 'pending' });
    assert.equal(waiting.headers.get('Retry-After'), '5');
    await error(429, () => poll(f, d.deviceToken));
    await error(401, () => handlePlatform(request('/api/v1/device-sessions/authorize', 'POST', { userCode: d.userCode }), f.env, null));
    await error(401, () => authorize(f, d.userCode, { ...f.player.identity, kind: 'login' }));
    await error(404, () => authorize(f, '12345678'));
    await error(400, () => handlePlatform(request('/api/v1/device-sessions/authorize', 'POST', { userCode: d.userCode, extra: true }), f.env, f.player.identity));
    assert.deepEqual((await authorize(f, d.userCode)).body, { authorized: true });
    await error(409, () => authorize(f, d.userCode, f.third.identity));
    await resetLimits();
    const done = await poll(f, d.deviceToken);
    assert.equal(done.status, 200); assert.match(done.body.token, /^ps_[A-Za-z0-9_-]{43}$/);
    await invoke(request(route(f.game, 'scores'), 'POST', payload(), done.body.token), f.env);
    await resetLimits();
    await error(410, () => poll(f, d.deviceToken));
  });

  await t.test('atomic device consume remains one-use even with concurrent polls past the limiter', async () => {
    await resetLimits();
    const f = await fixture();
    const d = await device(f);
    await authorize(f, d.userCode);
    // Bypass ONLY rate-limit storage so all contenders reach the real D1 batch.
    // This proves safety comes from SQL, not an isolate mutex or the rate limiter.
    const concurrentEnv = { ...f.env, DB: new Proxy(DB, { get(target, property) {
      if (property === 'prepare') return (sql: string) => {
        if (/INSERT INTO rate_limits/.test(sql)) {
          const allowed = { bind() { return allowed; }, async first() { return { count: 1 }; } };
          return allowed;
        }
        return target.prepare(sql);
      };
      const value = Reflect.get(target, property); return typeof value === 'function' ? value.bind(target) : value;
    } }) };
    const results = await Promise.allSettled(Array.from({ length: 12 }, () => invoke(
      request(route(f.game, 'device-sessions/poll'), 'POST', { deviceToken: d.deviceToken }), concurrentEnv)));
    assert.equal(results.filter(result => result.status === 'fulfilled').length, 1);
    for (const result of results) if (result.status === 'rejected') {
      assert.ok(result.reason instanceof HttpError); assert.equal(result.reason.status, 410);
    }
    const count = await DB.prepare('SELECT COUNT(*) AS n FROM play_sessions WHERE game_id=?').bind(f.game).first<{ n: number }>();
    assert.equal(count!.n, 1);
    assert.ok((await DB.prepare('SELECT consumed_at FROM device_sessions WHERE hash=?').bind(await digest(d.deviceToken)).first())!.consumed_at);
  });

  await t.test('atomic authorization has one winner, rejects stale source, private and expired devices', async () => {
    await resetLimits();
    const f = await fixture();
    const d = await device(f);
    const results = await Promise.allSettled([authorize(f, d.userCode), authorize(f, d.userCode, f.third.identity)]);
    assert.equal(results.filter(result => result.status === 'fulfilled').length, 1);
    const rejected = results.find(result => result.status === 'rejected');
    assert.ok(rejected && rejected.status === 'rejected' && rejected.reason.status === 409);
    const expired = await device(f);
    await DB.prepare('UPDATE device_sessions SET expires_at=? WHERE hash=?').bind(past(), await digest(expired.deviceToken)).run();
    await error(410, () => poll(f, expired.deviceToken));
    await error(404, () => authorize(f, expired.userCode));
    const stale = await device(f);
    await DB.prepare('UPDATE tokens SET revoked_at=? WHERE id=?').bind(new Date().toISOString(), f.player.identity.tokenId!).run();
    await error(401, () => authorize(f, stale.userCode));
    await DB.prepare('UPDATE tokens SET revoked_at=NULL WHERE id=?').bind(f.player.identity.tokenId!).run();
    await DB.prepare("UPDATE games SET visibility='private' WHERE id=?").bind(f.game).run();
    await error(404, () => authorize(f, stale.userCode));
    await error(404, () => poll(f, stale.deviceToken));
  });

  await t.test('poll revalidates source expiry/revocation and active user before any consumption', async () => {
    await resetLimits();
    const f = await fixture();
    const d = await device(f);
    await authorize(f, d.userCode);
    await DB.prepare('UPDATE tokens SET expires_at=? WHERE id=?').bind(past(), f.player.identity.tokenId!).run();
    await error(401, () => poll(f, d.deviceToken));
    await resetLimits();
    await DB.prepare('UPDATE tokens SET expires_at=?,revoked_at=? WHERE id=?')
      .bind(future(), new Date().toISOString(), f.player.identity.tokenId!).run();
    await error(401, () => poll(f, d.deviceToken));
    await resetLimits();
    await DB.prepare('UPDATE tokens SET revoked_at=NULL WHERE id=?').bind(f.player.identity.tokenId!).run();
    await DB.prepare("UPDATE users SET status='deleted' WHERE id=?").bind(f.player.identity.userId).run();
    await error(401, () => poll(f, d.deviceToken));
    assert.equal((await DB.prepare('SELECT consumed_at FROM device_sessions WHERE hash=?').bind(await digest(d.deviceToken)).first())!.consumed_at, null);
    assert.equal((await DB.prepare('SELECT COUNT(*) AS n FROM play_sessions WHERE game_id=?').bind(f.game).first<{ n: number }>())!.n, 0);
  });

  await t.test('poll rejects malformed bodies/tokens and binds the device secret to its game', async () => {
    await resetLimits();
    const f = await fixture();
    const d = await device(f);
    for (const token of [null, 'gd_short', f.player.raw, d.deviceToken + ',extra']) {
      await error(410, () => poll(f, token));
    }
    await error(400, () => handlePlatform(request(route(f.game, 'device-sessions/poll'), 'POST', { deviceToken: d.deviceToken, extra: 1 }), f.env, null));
    await error(413, () => handlePlatform(request(route(f.game, 'device-sessions/poll'), 'POST', { deviceToken: 'a'.repeat(1024) }), f.env, null));
    await error(410, () => handlePlatform(request(route(f.privateGame, 'device-sessions/poll'), 'POST', { deviceToken: d.deviceToken }), f.env, null));
  });
});
