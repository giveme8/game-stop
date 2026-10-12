import { rateLimit } from './auth';
import { credential, digest, HttpError, ID, json, readJSON, requireAPI, visibleGame } from './types';
import type { Env, Identity } from './types';

const PLAY_TOKEN = /^ps_[A-Za-z0-9_-]{43}$/;
const DEVICE_TOKEN = /^gd_[A-Za-z0-9_-]{43}$/;
const SUBMISSION = /^[A-Za-z0-9_-]{16,64}$/;
const DEVICE_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
const DEVICE_CODE = /^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{8}$/;
const MINUTE = 60_000;
const PRIVATE_HEADERS = { 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer' };

type Board = 'score' | 'time-ms';
interface ScoreInput { board: Board; mode: string; value: number; submissionId: string }
interface SubmissionRow { board: Board; mode: string; value: number; best_value: number; achieved_at: string }
interface DeviceRow {
  expires_at: string; consumed_at: string | null; authorized_at: string | null;
  visible_game: string | null; valid_source: number;
}

function reply(value: unknown, status = 200, headers?: HeadersInit): Response {
  const h = new Headers(PRIVATE_HEADERS);
  if (headers) new Headers(headers).forEach((value, name) => h.set(name, value));
  return json(value, status, h);
}

function fields(value: Record<string, unknown>, allowed: string[]): void {
  if (Object.keys(value).some(key => !allowed.includes(key))) throw new HttpError(400, 'unexpected request field');
}

// Creation endpoints accept no payload or {}. Read incrementally even when the
// client omits/forges Content-Length; never use an unbounded request.text().
async function emptyBody(request: Request): Promise<void> {
  if (!request.body) return;
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > 1024) { await reader.cancel(); throw new HttpError(413, 'request too large'); }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
  let text: string;
  try { text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: false }).decode(bytes).trim(); }
  catch { throw new HttpError(400, 'invalid request body'); }
  if (text !== '' && text !== '{}') throw new HttpError(400, 'request body must be empty');
}

function mode(value: unknown): string {
  if (value === undefined || value === '') return 'default';
  if (typeof value !== 'string' || !ID.test(value)) throw new HttpError(400, 'invalid mode');
  return value;
}

function board(value: unknown): Board {
  if (value !== 'score' && value !== 'time-ms') throw new HttpError(400, 'invalid board');
  return value;
}

async function scoreInput(request: Request): Promise<ScoreInput> {
  const input = await readJSON(request, 4096);
  fields(input, ['board', 'mode', 'value', 'submissionId']);
  const b = board(input.board);
  if (!Number.isSafeInteger(input.value) || input.value < 0 || (b === 'time-ms' && input.value === 0)
      || typeof input.submissionId !== 'string' || !SUBMISSION.test(input.submissionId)) {
    throw new HttpError(400, 'invalid score');
  }
  return { board: b, mode: mode(input.mode), value: input.value, submissionId: input.submissionId };
}

function api(identity: Identity | null): Identity & { tokenId: string } {
  const source = requireAPI(identity);
  if (!source.tokenId) throw new HttpError(401, 'API token required');
  return source as Identity & { tokenId: string };
}

function baseURL(env: Env): string {
  // Do not derive credential-bearing links from Host, Origin, or forwarded headers.
  let url: URL;
  try { url = new URL(env.PUBLIC_BASE_URL); }
  catch { throw new HttpError(503, 'platform URL unavailable'); }
  if (url.protocol !== 'https:' || url.username || url.password
      || (env.PUBLIC_BASE_URL !== url.origin && env.PUBLIC_BASE_URL !== `${url.origin}/`)) {
    throw new HttpError(503, 'platform URL unavailable');
  }
  return url.origin;
}

async function ipKey(request: Request): Promise<string> {
  // CF-Connecting-IP is supplied by the Workers edge. Never trust X-Forwarded-For.
  return digest((request.headers.get('CF-Connecting-IP') || 'unknown').slice(0, 128));
}

const SOURCE_FROM = `FROM tokens t JOIN users u ON u.id=t.user_id`;
const SOURCE_VALID = `t.id=? AND t.user_id=? AND t.hash=? AND t.revoked_at IS NULL
  AND t.expires_at>? AND u.status='active'`;
const PLAY_FROM = `FROM play_sessions p
  JOIN tokens t ON t.id=p.source_token_id AND t.user_id=p.user_id
  JOIN users u ON u.id=p.user_id`;
const PLAY_VALID = `p.hash=? AND p.game_id=? AND p.expires_at>?
  AND t.revoked_at IS NULL AND t.expires_at>? AND u.status='active'`;
const GAME_VISIBLE = `g.deleted=0 AND (g.visibility='public' OR g.owner_id=p.user_id)`;

async function createPlay(request: Request, env: Env, identity: Identity | null, gameId: string): Promise<Response> {
  const source = api(identity);
  await emptyBody(request);
  await rateLimit(env, `platform:play:${source.hash}`, 60, 60);
  await visibleGame(env, gameId, source);
  const token = credential('ps_');
  const hash = await digest(token);
  const now = new Date().toISOString();
  const expires = new Date(Date.parse(now) + 60 * MINUTE).toISOString();
  const sourceArgs = [source.tokenId, source.userId, source.hash, now];
  // Repeat authorization in the transaction: a preflight read is not a lock.
  const result = await env.DB.batch([
    env.DB.prepare(`INSERT INTO play_sessions (hash,user_id,game_id,source_token_id,expires_at,created_at)
      SELECT ?,u.id,g.id,t.id,MIN(t.expires_at,?),? ${SOURCE_FROM}
      JOIN games g ON g.id=? AND g.deleted=0 AND (g.visibility='public' OR g.owner_id=u.id)
      WHERE ${SOURCE_VALID} RETURNING expires_at`).bind(hash, expires, now, gameId, ...sourceArgs),
    env.DB.prepare(`SELECT t.id ${SOURCE_FROM} WHERE ${SOURCE_VALID}`).bind(...sourceArgs),
  ]);
  const created = result[0].results[0] as { expires_at: string } | undefined;
  if (!created) {
    if (!result[1].results.length) throw new HttpError(401, 'API token required');
    throw new HttpError(404, 'game not found');
  }
  return reply({ gameId, token, expiresAt: created.expires_at }, 201);
}

async function submitScore(request: Request, env: Env, gameId: string): Promise<Response> {
  const raw = request.headers.get('X-Play-Token');
  // Fetch combines duplicate headers with commas, which this exact shape rejects.
  if (!raw || !PLAY_TOKEN.test(raw)) throw new HttpError(401, 'play token required');
  const input = await scoreInput(request);
  const hash = await digest(raw);
  await rateLimit(env, `platform:score:${hash}`, 120, 60);
  const now = new Date().toISOString();
  const playArgs = [hash, gameId, now, now];
  const improves = `(s.user_id IS NULL OR (?='score' AND ?>s.value) OR (?='time-ms' AND ?<s.value))`;
  const improveArgs = [input.board, input.value, input.board, input.value];
  // D1 batch is the transaction/serialization boundary. The FIRST write reserves
  // the logical submission and computes its immutable response from the current
  // best. A conflicting request cannot update that snapshot or another board/mode.
  // Later identical retries may re-apply the snapshot, but strict improvement
  // prevents them from lowering/re-timestamping a newer best. No JS read/write gap.
  const result = await env.DB.batch([
    env.DB.prepare(`SELECT p.user_id ${PLAY_FROM} WHERE ${PLAY_VALID}`).bind(...playArgs),
    env.DB.prepare(`INSERT INTO score_submissions
      (game_id,user_id,submission_id,board,mode,value,best_value,achieved_at,created_at)
      SELECT g.id,p.user_id,?,?,?,?,CASE WHEN ${improves} THEN ? ELSE s.value END,
        CASE WHEN ${improves} THEN ? ELSE s.achieved_at END,?
      ${PLAY_FROM} JOIN games g ON g.id=p.game_id
      LEFT JOIN player_scores s ON s.game_id=p.game_id AND s.user_id=p.user_id AND s.board=? AND s.mode=?
      WHERE ${PLAY_VALID} AND ${GAME_VISIBLE}
      ON CONFLICT(game_id,user_id,submission_id) DO NOTHING`).bind(
        input.submissionId, input.board, input.mode, input.value,
        ...improveArgs, input.value, ...improveArgs, now, now,
        input.board, input.mode, ...playArgs),
    env.DB.prepare(`INSERT INTO player_scores (game_id,user_id,board,mode,value,achieved_at)
      SELECT r.game_id,r.user_id,r.board,r.mode,r.best_value,r.achieved_at
      ${PLAY_FROM} JOIN games g ON g.id=p.game_id
      JOIN score_submissions r ON r.game_id=p.game_id AND r.user_id=p.user_id
      WHERE ${PLAY_VALID} AND ${GAME_VISIBLE}
        AND r.submission_id=? AND r.board=? AND r.mode=? AND r.value=?
      ON CONFLICT(game_id,user_id,board,mode) DO UPDATE SET value=excluded.value,achieved_at=excluded.achieved_at
      WHERE (excluded.board='score' AND excluded.value>player_scores.value)
         OR (excluded.board='time-ms' AND excluded.value<player_scores.value)`).bind(
        ...playArgs, input.submissionId, input.board, input.mode, input.value),
    env.DB.prepare(`SELECT r.board,r.mode,r.value,r.best_value,r.achieved_at
      ${PLAY_FROM} JOIN games g ON g.id=p.game_id
      JOIN score_submissions r ON r.game_id=p.game_id AND r.user_id=p.user_id
      WHERE ${PLAY_VALID} AND ${GAME_VISIBLE} AND r.submission_id=?`).bind(...playArgs, input.submissionId),
  ]);
  if (!result[0].results.length) throw new HttpError(401, 'invalid play token');
  const saved = result[3].results[0] as SubmissionRow | undefined;
  if (!saved) throw new HttpError(404, 'game not found');
  if (saved.board !== input.board || saved.mode !== input.mode || saved.value !== input.value) {
    throw new HttpError(409, 'submission id already used with different payload');
  }
  return reply({ gameId, board: saved.board, mode: saved.mode, value: saved.best_value, achievedAt: saved.achieved_at });
}

async function leaderboard(request: Request, env: Env, identity: Identity | null, gameId: string): Promise<Response> {
  const query = new URL(request.url).searchParams;
  for (const key of ['board', 'mode', 'limit']) {
    if (query.getAll(key).length > 1) throw new HttpError(400, 'duplicate leaderboard parameter');
  }
  const b = board(query.get('board') || 'score');
  const m = mode(query.get('mode') ?? undefined);
  const rawLimit = query.get('limit');
  const limit = rawLimit === null ? 20 : Number(rawLimit);
  if ((rawLimit !== null && !/^[0-9]{1,3}$/.test(rawLimit)) || !Number.isInteger(limit) || limit < 1 || limit > 100) {
    throw new HttpError(400, 'invalid limit');
  }
  await rateLimit(env, `platform:leaderboard:${await ipKey(request)}`, 240, 60);
  // A single joined read checks visibility and obtains data. The LEFT JOIN keeps
  // one sentinel row for an accessible empty board, but no row for a private game.
  const result = await env.DB.prepare(`SELECT p.user_id,u.name AS playerName,p.value,p.achieved_at AS achievedAt
    FROM games g LEFT JOIN player_scores p ON p.game_id=g.id AND p.board=? AND p.mode=?
    LEFT JOIN users u ON u.id=p.user_id
    WHERE g.id=? AND g.deleted=0 AND (g.visibility='public' OR g.owner_id=?)
    ORDER BY p.value ${b === 'score' ? 'DESC' : 'ASC'},p.achieved_at ASC,p.user_id ASC LIMIT ?`)
    .bind(b, m, gameId, identity?.userId ?? 0, limit)
    .all<{ user_id: number | null; playerName: string; value: number; achievedAt: string }>();
  if (!result.results.length) throw new HttpError(404, 'game not found');
  let previous: number | undefined;
  let rank = 0;
  const entries = result.results.filter(row => row.user_id !== null).map((row, index) => {
    if (row.value !== previous) rank = index + 1;
    previous = row.value;
    return { rank, playerName: row.playerName, value: row.value, achievedAt: row.achievedAt };
  });
  return reply({ gameId, board: b, mode: m, entries });
}

async function createDevice(request: Request, env: Env, gameId: string): Promise<Response> {
  await emptyBody(request);
  const origin = baseURL(env);
  await rateLimit(env, 'platform:device:create:global', 120, 60);
  await rateLimit(env, `platform:device:create:${await ipKey(request)}`, 20, 60);
  await visibleGame(env, gameId, null); // Pairing is deliberately public-game only.
  for (let attempt = 0; attempt < 3; attempt++) {
    const token = credential('gd_');
    const hash = await digest(token);
    const code = Array.from(crypto.getRandomValues(new Uint8Array(8)), value => DEVICE_ALPHABET[value & 31]).join('');
    const now = new Date().toISOString();
    const expires = new Date(Date.parse(now) + 10 * MINUTE).toISOString();
    const result = await env.DB.batch([
      env.DB.prepare('DELETE FROM device_sessions WHERE expires_at<=? OR consumed_at IS NOT NULL').bind(now),
      env.DB.prepare(`INSERT INTO device_sessions (hash,code,game_id,expires_at,created_at)
        SELECT ?,?,g.id,?,? FROM games g WHERE g.id=? AND g.deleted=0 AND g.visibility='public'
          AND (SELECT COUNT(*) FROM device_sessions WHERE consumed_at IS NULL AND expires_at>?)<100
          AND (SELECT COUNT(*) FROM device_sessions WHERE game_id=? AND consumed_at IS NULL AND expires_at>?)<10
        ON CONFLICT DO NOTHING RETURNING code`).bind(hash, code, expires, now, gameId, now, gameId, now),
      env.DB.prepare(`SELECT id FROM games WHERE id=? AND deleted=0 AND visibility='public'`).bind(gameId),
      env.DB.prepare(`SELECT COUNT(*) AS total,COALESCE(SUM(game_id=?),0) AS per_game
        FROM device_sessions WHERE consumed_at IS NULL AND expires_at>?`).bind(gameId, now),
    ]);
    if (result[1].results.length) {
      return reply({ deviceToken: token, userCode: code, verificationUrl: `${origin}/device-connect?code=${code}`,
        expiresAt: expires, pollInterval: 5 }, 201);
    }
    if (!result[2].results.length) throw new HttpError(404, 'game not found');
    const count = result[3].results[0] as { total: number; per_game: number };
    if (count.total >= 100 || count.per_game >= 10) throw new HttpError(429, 'device pairing capacity reached');
    // Only a random code/hash collision remains. Retry a bounded number of times.
  }
  throw new HttpError(503, 'device pairing unavailable');
}

async function authorizeDevice(request: Request, env: Env, identity: Identity | null): Promise<Response> {
  const source = api(identity);
  const input = await readJSON(request, 1024);
  fields(input, ['userCode']);
  if (typeof input.userCode !== 'string' || !DEVICE_CODE.test(input.userCode)) {
    throw new HttpError(404, 'device code or public game not found');
  }
  await rateLimit(env, `platform:device:authorize:${source.hash}`, 20, 60);
  await rateLimit(env, `platform:device:authorize:ip:${await ipKey(request)}`, 40, 60);
  const now = new Date().toISOString();
  const sourceArgs = [source.tokenId, source.userId, source.hash, now];
  const result = await env.DB.batch([
    env.DB.prepare(`UPDATE device_sessions SET user_id=?,source_token_id=?,authorized_at=?
      WHERE code=? AND expires_at>? AND consumed_at IS NULL AND authorized_at IS NULL
        AND EXISTS (SELECT 1 FROM games g WHERE g.id=device_sessions.game_id AND g.deleted=0 AND g.visibility='public')
        AND EXISTS (SELECT 1 ${SOURCE_FROM} WHERE ${SOURCE_VALID}) RETURNING game_id`)
      .bind(source.userId, source.tokenId, now, input.userCode, now, ...sourceArgs),
    env.DB.prepare(`SELECT d.expires_at,d.consumed_at,d.authorized_at,g.id AS visible_game
      FROM device_sessions d LEFT JOIN games g ON g.id=d.game_id AND g.deleted=0 AND g.visibility='public'
      WHERE d.code=?`).bind(input.userCode),
    env.DB.prepare(`SELECT t.id ${SOURCE_FROM} WHERE ${SOURCE_VALID}`).bind(...sourceArgs),
  ]);
  if (result[0].results.length) return reply({ authorized: true });
  if (!result[2].results.length) throw new HttpError(401, 'API token required');
  const device = result[1].results[0] as DeviceRow | undefined;
  if (!device || !device.visible_game || device.expires_at <= now) throw new HttpError(404, 'device code or public game not found');
  if (device.consumed_at) throw new HttpError(410, 'device session expired or consumed; pair again');
  if (device.authorized_at) throw new HttpError(409, 'device already authorized');
  throw new HttpError(503, 'device pairing unavailable');
}

async function pollDevice(request: Request, env: Env, gameId: string): Promise<Response> {
  const input = await readJSON(request, 1024);
  fields(input, ['deviceToken']);
  if (typeof input.deviceToken !== 'string' || !DEVICE_TOKEN.test(input.deviceToken)) {
    throw new HttpError(410, 'device session expired or consumed; pair again');
  }
  const hash = await digest(input.deviceToken);
  await rateLimit(env, `platform:device:poll:${hash}`, 1, 5);
  const token = credential('ps_');
  const playHash = await digest(token);
  const now = new Date().toISOString();
  const expires = new Date(Date.parse(now) + 60 * MINUTE).toISOString();
  // The INSERT and consume UPDATE are one transaction. Only an unconsumed row
  // can mint a hash. The UPDATE is guarded by that unique newly minted hash, not
  // by a timestamp or connection-local changes(). Concurrent polls cannot both
  // mint credentials. No plaintext play/device token is ever written to D1.
  const result = await env.DB.batch([
    env.DB.prepare(`INSERT INTO play_sessions (hash,user_id,game_id,source_token_id,expires_at,created_at)
      SELECT ?,d.user_id,d.game_id,d.source_token_id,MIN(t.expires_at,?),?
      FROM device_sessions d JOIN tokens t ON t.id=d.source_token_id AND t.user_id=d.user_id
      JOIN users u ON u.id=d.user_id JOIN games g ON g.id=d.game_id
      WHERE d.hash=? AND d.game_id=? AND d.expires_at>? AND d.consumed_at IS NULL AND d.authorized_at IS NOT NULL
        AND t.revoked_at IS NULL AND t.expires_at>? AND u.status='active' AND g.deleted=0 AND g.visibility='public'`)
      .bind(playHash, expires, now, hash, gameId, now, now),
    env.DB.prepare(`UPDATE device_sessions SET consumed_at=? WHERE hash=? AND game_id=? AND consumed_at IS NULL
      AND EXISTS (SELECT 1 FROM play_sessions p WHERE p.hash=? AND p.game_id=device_sessions.game_id
        AND p.user_id=device_sessions.user_id AND p.source_token_id=device_sessions.source_token_id) RETURNING hash`)
      .bind(now, hash, gameId, playHash),
    env.DB.prepare('SELECT expires_at FROM play_sessions WHERE hash=?').bind(playHash),
    env.DB.prepare(`SELECT d.expires_at,d.consumed_at,d.authorized_at,g.id AS visible_game,
      CASE WHEN t.revoked_at IS NULL AND t.expires_at>? AND u.status='active' THEN 1 ELSE 0 END AS valid_source
      FROM device_sessions d LEFT JOIN games g ON g.id=d.game_id AND g.deleted=0 AND g.visibility='public'
      LEFT JOIN tokens t ON t.id=d.source_token_id AND t.user_id=d.user_id LEFT JOIN users u ON u.id=d.user_id
      WHERE d.hash=? AND d.game_id=?`).bind(now, hash, gameId),
  ]);
  const created = result[2].results[0] as { expires_at: string } | undefined;
  if (created && result[1].results.length === 1) return reply({ gameId, token, expiresAt: created.expires_at });
  const device = result[3].results[0] as DeviceRow | undefined;
  if (!device || device.expires_at <= now || device.consumed_at) throw new HttpError(410, 'device session expired or consumed; pair again');
  if (!device.visible_game) throw new HttpError(404, 'game not found');
  if (!device.authorized_at) return reply({ status: 'pending' }, 202, { 'Retry-After': '5' });
  if (!device.valid_source) throw new HttpError(401, 'authorization no longer valid; pair again');
  throw new HttpError(503, 'device pairing unavailable');
}

/** Native platform routes. The caller owns error serialization, CORS and UI. */
export async function handlePlatform(request: Request, env: Env, identity: Identity | null): Promise<Response | null> {
  const path = new URL(request.url).pathname;
  const authorize = path === '/api/v1/device-sessions/authorize';
  const match = /^\/api\/v1\/games\/([^/]+)\/(play-sessions|scores|leaderboard|device-sessions(?:\/poll)?)$/.exec(path);
  if (!authorize && !match) return null;
  if (request.url.length > 4096) throw new HttpError(414, 'request URL too long');
  if (request.method !== (match?.[2] === 'leaderboard' ? 'GET' : 'POST')) throw new HttpError(405, 'method not allowed');
  if (match && !ID.test(match[1])) throw new HttpError(404, 'game not found');
  try {
    if (authorize) return await authorizeDevice(request, env, identity);
    const [, gameId, endpoint] = match!;
    switch (endpoint) {
      case 'play-sessions': return await createPlay(request, env, identity, gameId);
      case 'scores': return await submitScore(request, env, gameId);
      case 'leaderboard': return await leaderboard(request, env, identity, gameId);
      case 'device-sessions': return await createDevice(request, env, gameId);
      case 'device-sessions/poll': return await pollDevice(request, env, gameId);
      default: return null;
    }
  } catch (error) {
    if (error instanceof HttpError) throw error;
    // Never let D1 SQL/errors or token-bearing request details escape to clients.
    throw new HttpError(503, 'platform unavailable');
  }
}
