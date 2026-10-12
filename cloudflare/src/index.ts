import { authenticate, handleAuth } from './auth';
import { handlePlatform } from './platform';
import { handleLibrary } from './library';
import {
  accountPage, connectPage, deviceConnectPage, homePage, leaderboardPage, pageHeaders,
  registerHTML, loginHTML, renderTemplate, scripts,
} from './pages';
import { Env, HttpError, Identity, ID, json } from './types';

const ROUTES = new Set([
  '/api/v1/auth/register', '/api/v1/auth/login', '/api/v1/auth/logout', '/api/v1/me',
  '/api/v1/me/tokens', '/register', '/login', '/account', '/auth.js', '/connect', '/connect.js',
  '/device-connect', '/device-connect.js', '/sdk/v1/game-stop.js', '/healthz', '/gamesource/v1/health',
  '/gamesource/v1/index.json', '/api/v1/games', '/',
]);

function platformCORS(request: Request): Response | null {
  const parts = new URL(request.url).pathname.split('/');
  if (parts.length !== 6 || parts[1] !== 'api' || parts[2] !== 'v1' || parts[3] !== 'games' || !ID.test(parts[4] ?? '')) return null;
  const method = parts[5] === 'scores' ? 'POST' : parts[5] === 'leaderboard' ? 'GET' : null;
  if (!method) return null;
  const actual = request.headers.get('Access-Control-Request-Method');
  if (request.method !== 'OPTIONS') return null;
  const headers = new Headers({ 'Access-Control-Allow-Origin': '*' });
  headers.append('Vary', 'Access-Control-Request-Method, Access-Control-Request-Headers');
  if (actual !== method) return json({ error: 'unsupported CORS method' }, 405, headers);
  for (const header of (request.headers.get('Access-Control-Request-Headers') ?? '').split(',')) {
    const name = header.trim().toLowerCase();
    if (name !== '' && name !== 'content-type' && name !== 'x-play-token') return json({ error: 'unsupported CORS header' }, 403, headers);
  }
  headers.set('Access-Control-Allow-Methods', method);
  headers.set('Access-Control-Allow-Headers', 'Content-Type, X-Play-Token');
  return new Response(null, { status: 204, headers });
}

/** Only the score and leaderboard endpoints are cross-origin readable. */
function corsFor(request: Request): Headers | null {
  const parts = new URL(request.url).pathname.split('/');
  if (parts.length !== 6 || parts[1] !== 'api' || parts[2] !== 'v1' || parts[3] !== 'games' || !ID.test(parts[4] ?? '')) return null;
  const method = parts[5] === 'scores' ? 'POST' : parts[5] === 'leaderboard' ? 'GET' : null;
  if (!method) return null;
  if (request.method !== method && !(method === 'GET' && request.method === 'HEAD')) return null;
  return new Headers({ 'Access-Control-Allow-Origin': '*' });
}

async function health(env: Env): Promise<Response> {
  try { await env.DB.prepare('SELECT 1').first(); }
  catch { return json({ error: 'not ready' }, 503); }
  return json({ ok: true });
}

async function router(request: Request, env: Env, identity: Identity | null, requestId: string): Promise<Response> {
  const url = new URL(request.url);
  const path = url.pathname;
  const method = request.method;
  const head = method === 'HEAD';
  const read = method === 'GET' || head;

  if (path === '/healthz' || path === '/gamesource/v1/health') return health(env);
  if (path === '/' && read) return homePage(env, head);
  if (path === '/register' && read) return new Response(renderTemplate(registerHTML, {}), { status: 200, headers: withHTML(pageHeaders(true)) });
  if (path === '/login' && read) return new Response(renderTemplate(loginHTML, {}), { status: 200, headers: withHTML(pageHeaders(true)) });
  if (path === '/account' && read) return accountPage(env, identity, head);
  if (path === '/auth.js' && read) return scripts.auth(head);
  if (path === '/connect' && read) return connectPage(request, head);
  if (path === '/connect.js' && read) return scripts.connect(head);
  if (path === '/device-connect' && read) return deviceConnectPage(env, (url.searchParams.get('code') ?? '').trim().toUpperCase(), head);
  if (path === '/device-connect.js' && read) return scripts.deviceConnect(head);
  if (path === '/sdk/v1/game-stop.js' && read) return scripts.sdk(head);
  const board = /^\/leaderboards\/([^/]+)$/.exec(path);
  if (board && read) return leaderboardPage(env, request, decodeURIComponent(board[1]!), head);

  const auth = await handleAuth(request, env, identity);
  if (auth) return auth;
  const platform = await handlePlatform(request, env, identity);
  if (platform) return platform;
  const library = await handleLibrary(request, env, identity);
  if (library) return library;

  // Unmatched paths keep the origin's "unknown route" contract instead of
  // serving an SPA fallback that could mask a typo in an API caller.
  const known = ROUTES.has(path) || /^\/api\/v1\/games(\/|$)/.test(path) || board !== null;
  if (!known) return json({ error: 'not found' }, 404);
  if (!read) return json({ error: 'method not allowed' }, 405);
  return json({ error: 'not found' }, 404);
}

function withHTML(headers: Headers): Headers {
  headers.set('Content-Type', 'text/html; charset=utf-8');
  return headers;
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const requestId = crypto.randomUUID();
    const headers = new Headers({ 'Cache-Control': 'private, no-store', 'X-Content-Type-Options': 'nosniff', 'Vary': 'Authorization, X-Play-Token' });
    const preflight = platformCORS(request);
    if (preflight) return preflight;
    let identity: Identity | null = null;
    try {
      identity = await authenticate(request, env);
    } catch {
      // A database outage is never treated as an anonymous success: callers that
      // need identity still fail closed, and the error is sanitized.
      return json({ error: 'authentication unavailable' }, 503, headers);
    }
    try {
      const response = await router(request, env, identity, requestId);
      const merged = new Headers(response.headers);
      for (const [key, value] of headers) if (!merged.has(key)) merged.set(key, value);
      const cors = corsFor(request);
      if (cors) for (const [key, value] of cors) merged.set(key, value);
      return new Response(response.body, { status: response.status, headers: merged });
    } catch (error) {
      if (error instanceof HttpError && error.status >= 400 && error.status < 500) return json({ error: error.message }, error.status, headers);
      // Never echo driver or runtime errors: they can contain SQL and bindings.
      return json({ error: 'service unavailable' }, 503, headers);
    }
  },
};
