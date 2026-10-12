export interface Env {
  DB: D1Database;
  PACKAGES: R2Bucket;
  PUBLIC_BASE_URL: string;
  MAX_PACKAGE_BYTES: string;
  // Optional direct-delivery settings. Without them the Worker streams objects
  // itself, which is far slower for large packages.
  R2_ACCOUNT_ID?: string;
  R2_BUCKET?: string;
  R2_URL_TTL?: string;
  R2_ACCESS_KEY_ID?: string;
  R2_SECRET_ACCESS_KEY?: string;
}
export interface Identity {
  userId: number;
  name: string;
  status: string;
  kind: 'api' | 'login';
  tokenId?: string;
  hash: string;
}
export interface GameRow {
  id: string; owner_id: number; visibility: string; metadata: string;
  package_key: string; size: number; sha256: string; format: string;
  assets: string; source: string; deleted: number;
  created_at: string; updated_at: string;
  received_size: number; received_sha256: string;
}
export class HttpError extends Error {
  constructor(public status: number, message: string) { super(message); }
}
export const ID = /^[a-z0-9][a-z0-9-]{0,63}$/;
export const nowISO = () => new Date().toISOString();
export function json(value: unknown, status = 200, headers?: HeadersInit): Response {
  const h = new Headers(headers); h.set('Content-Type', 'application/json; charset=utf-8');
  return new Response(JSON.stringify(value), { status, headers: h });
}
export async function readJSON(request: Request, max = 65536): Promise<any> {
  if (!request.body) throw new HttpError(400, 'JSON body required');
  const reader = request.body.getReader(); const chunks: Uint8Array[] = []; let size = 0;
  try {
    for (;;) { const {done,value} = await reader.read(); if (done) break; size += value.length;
      if (size > max) { await reader.cancel(); throw new HttpError(413, 'request too large'); } chunks.push(value); }
  } finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size); let offset = 0; for (const chunk of chunks) { bytes.set(chunk,offset); offset += chunk.length; }
  try { const value = JSON.parse(new TextDecoder('utf-8',{fatal:true,ignoreBOM:false}).decode(bytes));
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(); return value;
  } catch { throw new HttpError(400,'invalid JSON object'); }
}
export const hex = (bytes: Uint8Array) => Array.from(bytes, b => b.toString(16).padStart(2,'0')).join('');
export async function digest(value: string | Uint8Array): Promise<string> {
  const bytes = typeof value === 'string' ? new TextEncoder().encode(value) : value;
  return hex(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes as BufferSource)));
}
export function randomHex(count = 16): string { return hex(crypto.getRandomValues(new Uint8Array(count))); }
export function credential(prefix: string): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return prefix + btoa(String.fromCharCode(...bytes)).replaceAll('+','-').replaceAll('/','_').replaceAll('=','');
}
export function requireAPI(identity: Identity | null): Identity {
  if (!identity || identity.kind !== 'api' || identity.status !== 'active') throw new HttpError(401,'API token required'); return identity;
}
export function requireLogin(identity: Identity | null): Identity {
  if (!identity || identity.kind !== 'login') throw new HttpError(401,'login session required'); return identity;
}
export async function visibleGame(env: Env, id: string, identity: Identity | null): Promise<GameRow> {
  if (!ID.test(id)) throw new HttpError(404,'game not found');
  const g = await env.DB.prepare('SELECT * FROM games WHERE id=? AND deleted=0').bind(id).first<GameRow>();
  if (!g || (g.visibility !== 'public' && g.owner_id !== identity?.userId)) throw new HttpError(404,'game not found'); return g;
}
export function escapeHTML(value: unknown): string {
  return String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]!));
}
