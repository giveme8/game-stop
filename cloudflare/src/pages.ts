import { Env, GameRow, HttpError, ID, Identity, escapeHTML, json, nowISO, randomHex } from './types';
import { parseAssets, parseMetadata, publicGames, representation, assetURL } from './library';
import { renderTemplate } from './template';
import homeHTML from '../pages/home.html';
import registerHTML from '../pages/register.html';
import loginHTML from '../pages/login.html';
import accountHTML from '../pages/account.html';
import connectHTML from '../pages/connect.html';
import deviceConnectHTML from '../pages/device_connect.html';
import leaderboardHTML from '../pages/leaderboard.html';
import authJS from '../assets/auth.js';
import connectJS from '../assets/connect.js';
import deviceConnectJS from '../assets/device_connect.js';
import sdkJS from '../assets/game-stop.js';

const STATE_PATTERN = /^[0-9a-f]{32}$/;
const MODE_PATTERN = ID;

export const pageHeaders = (html: boolean): Headers => {
  const headers = new Headers({ 'Cache-Control': 'private, no-store', 'Referrer-Policy': 'no-referrer' });
  headers.set('Content-Security-Policy', html
    ? "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
    : "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'");
  headers.set('X-Frame-Options', 'DENY');
  headers.set('X-Content-Type-Options', 'nosniff');
  return headers;
};
const connectHeaders = (): Headers => {
  const headers = pageHeaders(true);
  headers.set('Cross-Origin-Opener-Policy', 'unsafe-none');
  return headers;
};
const html = (body: string, status = 200, headers = pageHeaders(true), head = false): Response =>
  new Response(head ? null : body, { status, headers: withType(headers, 'text/html; charset=utf-8') });
function withType(headers: Headers, type: string): Headers { headers.set('Content-Type', type); return headers; }
const text = (body: string, type: string, headers = pageHeaders(false), head = false): Response =>
  new Response(head ? null : body, { status: 200, headers: withType(headers, type) });

/** Same policy as the Go service: HTTPS, or HTTP only for loopback/localhost/private IPv4. */
export function validConnectOrigin(raw: string): boolean {
  let url: URL;
  try { url = new URL(raw); } catch { return false; }
  if (url.username || url.password || url.search || url.hash || url.pathname !== '/' || url.host === '') return false;
  if (raw.includes('\\') || /\s/.test(raw)) return false;
  const host = url.hostname;
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return false;
  if (url.protocol === 'https:') return true;
  if (host === 'localhost') return true;
  const ipv4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host);
  if (!ipv4) return false;
  const octets = ipv4.slice(1).map(Number);
  if (octets.some(n => n > 255)) return false;
  if (host !== octets.join('.')) return false;
  const [a = 0, b = 0] = octets;
  return a === 10 || a === 127 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 169 && b === 254);
}

export async function homePage(env: Env, head: boolean): Promise<Response> {
  let games: Array<Record<string, unknown>> = [];
  let unavailable = false;
  let status = 200;
  try {
    const rows = await publicGames(env);
    games = rows.filter(row => ID.test(row.id)).map(row => {
      const meta = parseMetadata(row.metadata);
      return {
        ID: row.id, Title: meta.title, Subtitle: meta.subtitle,
        Description: meta.description || '下载游戏内容包，在兼容的 Godot 播放器中体验。',
        Tags: meta.tags, Engine: meta.engine.version, Size: sizeLabel(row.size),
        DownloadURL: `/api/v1/games/${row.id}/package.pck`,
      };
    });
  } catch { unavailable = true; status = 503; }
  return html(renderTemplate(homeHTML, { Games: games, Unavailable: unavailable }), status, pageHeaders(false), head);
}
function sizeLabel(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}

export async function accountPage(env: Env, identity: Identity | null, head: boolean): Promise<Response> {
  interface TokenRow { id: string; created_at: string; expires_at: string; revoked_at: string | null }
  const data: Record<string, unknown> = {
    SignedIn: false, Unavailable: false, Name: '', StatusText: '', Pending: false, Rejected: false,
    CanIssue: false, Tokens: [], CreatedAt: '', ApprovedAt: null,
  };
  if (identity && identity.kind === 'login') {
    try {
      const user = await env.DB.prepare('SELECT * FROM users WHERE id=?').bind(identity.userId)
        .first<{ name: string; status: string; created_at: string; approved_at: string | null }>();
      if (!user || user.status === 'deleted') { data.Unavailable = true; }
      else {
        const tokens = await env.DB.prepare('SELECT id,created_at,expires_at,revoked_at FROM tokens WHERE user_id=? ORDER BY created_at DESC')
          .bind(identity.userId).all<TokenRow>();
        data.SignedIn = true;
        data.Name = user.name;
        data.CreatedAt = user.created_at;
        data.ApprovedAt = user.approved_at;
        data.StatusText = statusText(user.status);
        data.Pending = user.status === 'pending';
        data.Rejected = user.status === 'rejected';
        data.CanIssue = user.status === 'active';
        data.Tokens = (tokens.results ?? []).map(token => ({
          ID: token.id, CreatedAt: token.created_at, ExpiresAt: token.expires_at, RevokedAt: token.revoked_at,
        }));
      }
    } catch { data.Unavailable = true; }
  }
  return html(renderTemplate(accountHTML, data), 200, pageHeaders(true), head);
}
function statusText(status: string): string {
  switch (status) {
    case 'active': return '账号有效：可以签发 Token 并管理自己的游戏。';
    case 'pending': return '等待管理员审核：通过后才能签发 Token。';
    case 'rejected': return '账号已被拒绝：不能签发 Token。';
    default: return '账号状态未知。';
  }
}

export async function leaderboardPage(env: Env, request: Request, id: string, head: boolean): Promise<Response> {
  const query = new URL(request.url).searchParams;
  const data: Record<string, unknown> = {
    ID: id, Title: '', Board: 'score', Mode: 'default', Modes: [], Boards: [], ModeLabel: '',
    Entries: [], Unavailable: false, NotFound: false, InvalidBoard: false,
    BoardName: (board: string) => (board === 'time-ms' ? '用时榜' : '分数榜'),
    ModeName: (mode: string) => mode,
  };
  if (!ID.test(id)) { data.NotFound = true; return html(renderTemplate(leaderboardHTML, data), 404, pageHeaders(false), head); }
  let status = 200;
  try {
    const row = await env.DB.prepare('SELECT * FROM games WHERE id=? AND deleted=0 AND visibility=?').bind(id, 'public').first<GameRow>();
    if (!row) { data.NotFound = true; return html(renderTemplate(leaderboardHTML, data), 404, pageHeaders(false), head); }
    const meta = parseMetadata(row.metadata);
    data.Title = meta.title;
    const labels = new Map(meta.modes.map(m => [m.id, m.label]));
    data.ModeName = (mode: string) => labels.get(mode) || mode;
    const mode = MODE_PATTERN.test(query.get('mode') ?? '') && query.getAll('mode').length <= 1 ? query.get('mode')! : 'default';
    const board = query.get('board') === 'time-ms' ? 'time-ms' : 'score';
    const modes = await env.DB.prepare('SELECT DISTINCT mode FROM player_scores WHERE game_id=?').bind(id).all<{ mode: string }>();
    const boards = await env.DB.prepare('SELECT DISTINCT board FROM player_scores WHERE game_id=?').bind(id).all<{ board: string }>();
    const order = new Map(meta.modes.map((m, index) => [m.id, index]));
    const known = (modes.results ?? []).map(m => m.mode).filter(m => MODE_PATTERN.test(m));
    const modeList = [...new Set([...known, mode])].sort((a, b) => {
      const [ai, bi] = [order.get(a), order.get(b)];
      if (ai !== undefined && bi !== undefined) return ai - bi;
      if (ai !== undefined) return -1;
      if (bi !== undefined) return 1;
      return a < b ? -1 : a > b ? 1 : 0;
    });
    const boardList = [...new Set((boards.results ?? []).map(b => b.board).filter(b => b === 'score' || b === 'time-ms'))];
    if (!boardList.includes(board)) boardList.push(board);
    data.Board = board; data.Mode = mode; data.Modes = modeList; data.Boards = boardList; data.ModeLabel = (data.ModeName as (m: string) => string)(mode);
    const order2 = query.get('board') === 'time-ms' ? 'ASC' : 'DESC';
    const entries = await env.DB.prepare(
      `SELECT s.value AS value, s.achieved_at AS achieved_at, u.name AS player_name
       FROM player_scores s JOIN users u ON u.id=s.user_id
       WHERE s.game_id=? AND s.board=? AND s.mode=? ORDER BY s.value ${order2}, s.achieved_at ASC LIMIT 100`,
    ).bind(id, board, mode).all<{ value: number; achieved_at: string; player_name: string }>();
    let rank = 0; let previous: number | null = null; let position = 0;
    data.Entries = (entries.results ?? []).map(entry => {
      position++;
      if (previous === null || entry.value !== previous) { rank = position; previous = entry.value; }
      return { Rank: rank, PlayerName: entry.player_name, Value: entry.value, AchievedAt: entry.achieved_at };
    });
  } catch { data.Unavailable = true; status = 503; }
  return html(renderTemplate(leaderboardHTML, data), status, pageHeaders(false), head);
}

export async function connectPage(request: Request, head: boolean): Promise<Response> {
  const parameters = new URL(request.url).searchParams;
  const allowed = new Set(['gameId', 'mode', 'origin', 'state']);
  for (const key of parameters.keys()) if (!allowed.has(key)) return json({ error: 'invalid connect parameters' }, 400);
  for (const key of allowed) if (parameters.getAll(key).length > 1) return json({ error: 'invalid connect parameters' }, 400);
  const game = parameters.get('gameId') ?? '';
  const mode = (parameters.get('mode') ?? '') || 'default';
  const origin = parameters.get('origin') ?? '';
  const state = parameters.get('state') ?? '';
  if (!ID.test(game) || !MODE_PATTERN.test(mode) || !validConnectOrigin(origin) || !STATE_PATTERN.test(state)) {
    return json({ error: 'invalid connect parameters' }, 400);
  }
  const data = { GameID: game, Mode: mode, Origin: origin, State: state, LAN: origin.startsWith('http:') };
  return html(renderTemplate(connectHTML, data), 200, connectHeaders(), head);
}

export async function deviceConnectPage(env: Env, code: string, head: boolean): Promise<Response> {
  const headers = pageHeaders(true);
  if (!/^[A-Z0-9]{4}-?[A-Z0-9]{4}$/.test(code)) return html(renderTemplate(deviceConnectHTML, { Title: '', Code: '' }), 404, headers, head);
  const session = await env.DB.prepare('SELECT game_id, expires_at, consumed_at FROM device_sessions WHERE code=?').bind(code.toUpperCase()).first<{ game_id: string; expires_at: string; consumed_at: string | null }>();
  if (!session) return html(renderTemplate(deviceConnectHTML, { Title: '', Code: code }), 404, headers, head);
  const row = await env.DB.prepare('SELECT metadata FROM games WHERE id=? AND deleted=0 AND visibility=?').bind(session.game_id, 'public').first<{ metadata: string }>();
  const title = row ? parseMetadata(row.metadata).title : '';
  return html(renderTemplate(deviceConnectHTML, { Title: title || session.game_id, Code: code.toUpperCase() }), 200, headers, head);
}

export const scripts = {
  auth: (head: boolean) => text(authJS, 'text/javascript; charset=utf-8', pageHeaders(true), head),
  connect: (head: boolean) => text(connectJS, 'text/javascript; charset=utf-8', connectHeaders(), head),
  deviceConnect: (head: boolean) => text(deviceConnectJS, 'text/javascript; charset=utf-8', pageHeaders(false), head),
  sdk: (head: boolean) => {
    const headers = new Headers({ 'Content-Type': 'text/javascript; charset=utf-8', 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'private, no-store', 'X-Content-Type-Options': 'nosniff' });
    return new Response(head ? null : sdkJS, { status: 200, headers });
  },
};
export { registerHTML, loginHTML, renderTemplate, HttpError, nowISO, randomHex, escapeHTML, parseAssets, representation, assetURL };
