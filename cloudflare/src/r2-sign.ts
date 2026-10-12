import { Env } from './types';

// AWS Signature Version 4 query-string presigning for the R2 S3 endpoint.
// R2 supports presigned GETs, which lets the client download straight from the
// edge instead of streaming the object through this Worker. Region is always
// "auto" and the service is always "s3" for R2.
const ALGORITHM = 'AWS4-HMAC-SHA256';
const SERVICE = 's3';
const REGION = 'auto';

const encoder = new TextEncoder();

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
}

/** RFC 3986 encoding: unreserved characters only, so `/` must be encoded by callers. */
export function uriEncode(value: string): string {
  return Array.from(encoder.encode(value), byte => {
    const char = String.fromCharCode(byte);
    return /[A-Za-z0-9\-._~]/.test(char) ? char : `%${byte.toString(16).toUpperCase().padStart(2, '0')}`;
  }).join('');
}

/** Encode an object key for a canonical URI, keeping the path separators. */
export function encodeKey(key: string): string {
  return key.split('/').map(uriEncode).join('/');
}

async function hmac(key: Uint8Array, message: string): Promise<Uint8Array> {
  const cryptoKey = await crypto.subtle.importKey('raw', key as BufferSource, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  return new Uint8Array(await crypto.subtle.sign('HMAC', cryptoKey, encoder.encode(message)));
}
async function sha256Hex(message: string): Promise<string> {
  return hex(new Uint8Array(await crypto.subtle.digest('SHA-256', encoder.encode(message) as BufferSource)));
}

export interface PresignOptions {
  accountId: string;
  accessKeyId: string;
  secretAccessKey: string;
  bucket: string;
  key: string;
  expiresIn: number;
  /** Injected for deterministic tests. */
  now?: Date;
}

/**
 * Presigned GET URL for one object. The result is a bearer capability for the
 * object until it expires, so it must never be logged or cached publicly.
 */
export async function presignGet(options: PresignOptions): Promise<string> {
  const host = `${options.accountId}.r2.cloudflarestorage.com`;
  const at = options.now ?? new Date();
  const amzDate = at.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '');
  const dateStamp = amzDate.slice(0, 8);
  const scope = `${dateStamp}/${REGION}/${SERVICE}/aws4_request`;
  const canonicalUri = `/${uriEncode(options.bucket)}/${encodeKey(options.key)}`;
  const query = [
    ['X-Amz-Algorithm', ALGORITHM],
    ['X-Amz-Credential', `${options.accessKeyId}/${scope}`],
    ['X-Amz-Date', amzDate],
    ['X-Amz-Expires', String(options.expiresIn)],
    ['X-Amz-SignedHeaders', 'host'],
  ] as const;
  const canonicalQuery = query
    .map(([name, value]) => `${uriEncode(name)}=${uriEncode(value)}`)
    .sort()
    .join('&');
  const canonicalRequest = [
    'GET', canonicalUri, canonicalQuery, `host:${host}`, '', 'host', 'UNSIGNED-PAYLOAD',
  ].join('\n');
  const stringToSign = [ALGORITHM, amzDate, scope, await sha256Hex(canonicalRequest)].join('\n');
  const signingKey = await hmac(
    await hmac(await hmac(await hmac(encoder.encode(`AWS4${options.secretAccessKey}`), dateStamp), REGION), SERVICE),
    'aws4_request',
  );
  const signature = hex(await hmac(signingKey, stringToSign));
  return `https://${host}${canonicalUri}?${canonicalQuery}&X-Amz-Signature=${signature}`;
}

export interface SignedURLConfig {
  accountId: string; accessKeyId: string; secretAccessKey: string; bucket: string; ttl: number;
}

/** Returns null when the instance is not configured for direct R2 delivery. */
export function signedURLConfig(env: Env): SignedURLConfig | null {
  const accountId = env.R2_ACCOUNT_ID ?? '';
  const accessKeyId = env.R2_ACCESS_KEY_ID ?? '';
  const secretAccessKey = env.R2_SECRET_ACCESS_KEY ?? '';
  const bucket = env.R2_BUCKET ?? '';
  const ttl = Number(env.R2_URL_TTL ?? '300');
  if (!accountId || !accessKeyId || !secretAccessKey || !bucket) return null;
  if (!Number.isSafeInteger(ttl) || ttl < 30 || ttl > 604800) return null;
  return { accountId, accessKeyId, secretAccessKey, bucket, ttl };
}

export async function signedObjectURL(env: Env, key: string): Promise<string | null> {
  const config = signedURLConfig(env);
  if (!config) return null;
  return presignGet({ ...config, key, expiresIn: config.ttl });
}
