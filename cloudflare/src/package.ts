import { md5 } from '@noble/hashes/legacy.js';
import { sha256 } from '@noble/hashes/sha2.js';
import { Inflate } from 'fflate';
import { HttpError } from './types';

// Native Workers have a 128 MiB isolate budget, not the Go server's memory model.
// This buffered API deliberately does NOT implement the 512 MiB range/stream API.
export const MAX_BUFFERED_PACKAGE = 32 * 1024 * 1024;
const MAX_EXPANDED = 768 * 1024 * 1024;
const MAX_META = 64 * 1024;
const MAX_COVER = 4 * 1024 * 1024;
const MAX_SHOT = 8 * 1024 * 1024;
const ID = /^[a-z0-9][a-z0-9-]{0,63}$/;
const encoder = new TextEncoder();
const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });
const executable = new Set('.so .abc .hap .har .hsp .dex .apk .exe .dll .dylib .com .bat .cmd .ps1 .msi .wasm .gdextension'.split(' '));
function invalid(what = 'package'): never { throw new HttpError(400, `invalid or unsafe ${what}`); }
function bufferedLimit(): never {
  throw new HttpError(413, 'native buffered package limit is conservatively 32 MiB for the upload and combined selected expanded content; larger PCKs require separate streaming/range validation');
}
function validString(value: unknown): value is string {
  return typeof value === 'string' && !/[\uD800-\uDFFF]/u.test(value);
}
function decode(bytes: Uint8Array): string {
  try { return utf8.decode(bytes); } catch { return invalid('UTF-8'); }
}
function withoutPrefix(name: string): string { return name.startsWith('res://') ? name.slice(6) : name; }
// Go strings.ToLower is per-codepoint, unlike JS's context-sensitive final sigma.
function lower(name: string): string { return Array.from(name, c => c === '\u0130' ? 'i' : c.toLowerCase()).join(''); }
function canonical(name: string): string { return lower(withoutPrefix(name).normalize('NFC')); }

/** Same path policy as the Go PCK validator; never a filesystem extraction path. */
export function safePath(name: string): boolean {
  if (!validString(name)) return false;
  const s = withoutPrefix(name);
  if (!s || /[\\:%\p{Cc}]/u.test(s) || s.startsWith('/')) return false;
  const parts = s.split('/');
  if (parts.some(p => !p || p === '.' || p === '..' || /[ .]$/.test(p))) return false;
  const base = lower(parts[parts.length - 1]);
  const dot = base.lastIndexOf('.');
  return !executable.has(dot < 0 ? '' : base.slice(dot)) && !base.includes('.so.');
}

class Bytes {
  view: DataView;
  constructor(readonly data: Uint8Array) { this.view = new DataView(data.buffer, data.byteOffset, data.byteLength); }
  range(at: number, length: number, end = this.data.length): void {
    if (!Number.isSafeInteger(at) || !Number.isSafeInteger(length) || at < 0 || length < 0 || at > end || length > end - at) invalid();
  }
  u16(at: number): number { this.range(at, 2); return this.view.getUint16(at, true); }
  u32(at: number): number { this.range(at, 4); return this.view.getUint32(at, true); }
  u64(at: number): number {
    this.range(at, 8);
    const n = this.view.getBigUint64(at, true);
    if (n > BigInt(Number.MAX_SAFE_INTEGER)) invalid('PCK integer');
    return Number(n);
  }
}

/** Standalone, unencrypted Godot formats 2/3/4, including every payload MD5. */
export async function inspectPCK(data: Uint8Array): Promise<{ engineVersion: string }> {
  if (data.length > MAX_BUFFERED_PACKAGE) bufferedLimit();
  const b = new Bytes(data);
  if (data.length < 100 || b.u32(0) !== 0x43504447) invalid('PCK');
  const format = b.u32(4), major = b.u32(8), minor = b.u32(12);
  if (format < 2 || format > 4 || major !== 4 || minor > 999 || (b.u32(20) & ~2) !== 0) invalid('PCK');
  const base = b.u64(24), headerEnd = format === 2 ? 96 : 104;
  const directory = format === 2 ? 96 : b.u64(32);
  if (directory < headerEnd || directory > data.length - 4 || base > data.length) invalid('PCK');
  const count = b.u32(directory);
  if (!count || count > 100000 || count > Math.floor((data.length - directory - 4) / 41)) invalid('PCK');
  let pos = directory + 4;
  // A32MiB directory can otherwise retain another64MiB of decoded UTF-16
  // names. Keep fixed-size cryptographic fingerprints, plus packed numeric
  // records, so path flooding does not exhaust a128MiB Worker isolate.
  const seen = new Set<string>();
  const starts = new Uint32Array(count), ends = new Uint32Array(count), sums = new Uint32Array(count);
  for (let i = 0; i < count; i++) {
    const length = b.u32(pos); pos += 4;
    if (!length || length > 4096) invalid('PCK path');
    b.range(pos, length + 36);
    let pathEnd = pos + length;
    while (pathEnd > pos && data[pathEnd - 1] === 0) pathEnd--;
    const name = decode(data.subarray(pos, pathEnd)); pos += length;
    if (!safePath(name)) invalid('PCK path');
    const key = Array.from(sha256(encoder.encode(canonical(name))), n => String.fromCharCode(n)).join('');
    if (seen.has(key)) invalid('duplicate PCK path');
    seen.add(key);
    const offset = b.u64(pos), size = b.u64(pos + 8), sum = pos + 16;
    if (b.u32(pos + 32) !== 0 || offset > data.length - base) invalid('PCK');
    const start = base + offset;
    if (start < headerEnd || size > data.length - start) invalid('PCK bounds');
    starts[i] = start; ends[i] = start + size; sums[i] = sum; pos += 36;
  }
  const order = Uint32Array.from({ length: count }, (_, i) => i);
  order.sort((a, c) => starts[a] - starts[c]);
  let previous = 0;
  for (const index of order) {
    const start = starts[index], end = ends[index];
    if (start < previous || (start < pos && end > directory)) invalid('PCK overlap');
    previous = end;
    const hash = md5(data.subarray(start, end));
    for (let i = 0; i < 16; i++) if (hash[i] !== data[sums[index] + i]) invalid('PCK payload MD5');
  }
  return { engineVersion: `${major}.${minor}` };
}

type ObjectValue = Record<string, unknown>;
function object(value: unknown): ObjectValue {
  if (!value || typeof value !== 'object' || Array.isArray(value)) invalid('metadata');
  return value as ObjectValue;
}
function string(value: unknown, max: number, required = false): string {
  if (value == null && !required) return '';
  if (!validString(value) || (required && !value) || encoder.encode(value).length > max) invalid('metadata string');
  return value;
}
function array(value: unknown, max: number): unknown[] {
  if (value == null) return [];
  if (!Array.isArray(value) || value.length > max) invalid('metadata array');
  return value;
}
interface Metadata {
  title: string; subtitle: string; description: string; tags: string[]; accent: string;
  engine: { name: string; version: string }; visibility: string;
  modes: Array<{ id: string; label: string }>;
}
/** Throws on invalid metadata and returns an allowlisted copy (never server fields). */
export function validateMetadata(value: unknown): Metadata {
  const m = object(value), engine = object(m.engine);
  const name = string(engine.name, 16, true), version = string(engine.version, 5, true);
  if (name !== 'godot' || !/^4\.[0-9]{1,3}$/.test(version)) invalid('engine');
  const accent = string(m.accent, 7), visibility = string(m.visibility, 7);
  if (accent && !/^#[0-9a-fA-F]{6}$/.test(accent)) invalid('accent');
  if (visibility && visibility !== 'public' && visibility !== 'private') invalid('visibility');
  const seen = new Set<string>();
  const modes = array(m.modes, 24).map(value => {
    const mode = object(value), id = string(mode.id, 64, true), label = string(mode.label, 48, true);
    if (!ID.test(id) || seen.has(id) || Array.from(label).length > 12) invalid('modes');
    seen.add(id); return { id, label };
  });
  return {
    title: string(m.title, 512, true), subtitle: string(m.subtitle, 1024), description: string(m.description, 8192),
    tags: array(m.tags, 16).map(t => string(t, 128)), accent,
    engine: { name, version }, visibility, modes,
  };
}
function validateSource(value: unknown): Record<string, string> {
  const s = value == null ? {} : object(value);
  const source = { repo: string(s.repo, 512), ref: string(s.ref, 128), commit: string(s.commit, 40), license: string(s.license, 64) };
  if (source.commit && !/^[0-9a-fA-F]{40}$/.test(source.commit)) invalid('source');
  if (source.repo) {
    try {
      const url = new URL(source.repo);
      if (!/^https?:\/\//.test(source.repo) || !['https:', 'http:'].includes(url.protocol) || !url.host || url.username || url.password || source.repo.slice(source.repo.indexOf('://') + 3).split(/[/?#]/, 1)[0].includes('@') || /[\\\p{Cc}\s]/u.test(source.repo)) invalid('source');
    } catch { invalid('source'); }
  }
  return source;
}

/** Magic-number sniff only, matching Go; does not claim to decode the image. */
export function imageExtension(bytes: Uint8Array): string {
  if (bytes.length >= 8 && [137, 80, 78, 71, 13, 10, 26, 10].every((n, i) => bytes[i] === n)) return '.png';
  if (bytes.length >= 3 && bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) return '.jpg';
  if (bytes.length >= 12 && bytes[0] === 82 && bytes[1] === 73 && bytes[2] === 70 && bytes[3] === 70 && bytes[8] === 87 && bytes[9] === 69 && bytes[10] === 66 && bytes[11] === 80) return '.webp';
  return '';
}

interface ZipEntry { name: string; directory: boolean; method: number; crc: number; size: number; compressed: number; start: number; }
function extras(b: Bytes, at: number, length: number): void {
  const end = at + length;
  while (at < end) {
    b.range(at, 4, end);
    const kind = b.u16(at), size = b.u16(at + 2); at += 4;
    b.range(at, size, end);
    // ZIP64, alternate path encodings, Unix link fields, and encryption extensions
    // introduce unsupported interpretations. Do not silently fall back to ZIP32.
    if ([0x0001, 0x7075, 0x000d, 0x756e, 0x0017, 0x9901].includes(kind)) invalid('unsupported ZIP extra field');
    at += size;
  }
}
function zipDirectory(data: Uint8Array): { files: Map<string, ZipEntry>; total: number } {
  const b = new Bytes(data);
  if (data.length < 22 || b.u32(0) !== 0x04034b50) invalid('ZIP');
  let end = -1;
  for (let at = data.length - 22; at >= Math.max(0, data.length - 65557); at--) {
    if (b.u32(at) === 0x06054b50 && at + 22 + b.u16(at + 20) === data.length) { end = at; break; }
  }
  if (end < 0 || b.u16(end + 4) || b.u16(end + 6)) invalid('ZIP directory');
  const count = b.u16(end + 10), centralSize = b.u32(end + 12), central = b.u32(end + 16);
  if (!count || count > 512 || count !== b.u16(end + 8) || central === 0xffffffff || centralSize === 0xffffffff || central + centralSize !== end) invalid('ZIP directory');
  b.range(central, centralSize, end);
  const entries: ZipEntry[] = [], intervals: Array<{ start: number; end: number }> = [];
  const paths = new Set<string>(), filePaths = new Set<string>();
  let pos = central, total = 0;
  for (let i = 0; i < count; i++) {
    b.range(pos, 46, end);
    if (b.u32(pos) !== 0x02014b50) invalid('ZIP central header');
    const version = b.u16(pos + 6), flags = b.u16(pos + 8), method = b.u16(pos + 10), crc = b.u32(pos + 16);
    const compressed = b.u32(pos + 20), size = b.u32(pos + 24), nameSize = b.u16(pos + 28), extraSize = b.u16(pos + 30), commentSize = b.u16(pos + 32);
    const attrs = b.u32(pos + 38), local = b.u32(pos + 42), type = (attrs >>> 16) & 0xf000;
    if (version < 10 || version > 20 || (flags & ~0x080e) !== 0 || (method !== 0 && method !== 8) || (method === 0 && (flags & 6)) || b.u16(pos + 34) || compressed === 0xffffffff || size === 0xffffffff || local === 0xffffffff) invalid('unsupported or encrypted ZIP');
    b.range(pos + 46, nameSize + extraSize + commentSize, end);
    // Keep decoded/canonical ZIP path tables bounded too, not just payloads.
    if (nameSize === 0 || nameSize > 4096) invalid('ZIP path length');
    const rawName = data.subarray(pos + 46, pos + 46 + nameSize), name = decode(rawName);
    const directory = name.endsWith('/');
    if (!safePath(directory ? name.slice(0, -1) : name)) invalid('ZIP path');
    if ((type !== 0 && type !== 0x8000 && type !== 0x4000) || ((type === 0x4000 || (attrs & 16) !== 0) && !directory) || (directory && type === 0x8000)) invalid('ZIP link or special file');
    if (directory && (size || compressed || crc || method !== 0)) invalid('ZIP directory payload');
    const key = canonical(directory ? name.slice(0, -1) : name);
    if (paths.has(key)) invalid('duplicate ZIP path');
    paths.add(key); if (!directory) filePaths.add(key);
    extras(b, pos + 46 + nameSize, extraSize);
    if ((compressed === 0 && size > 0) || size > compressed * 200 || (method === 0 && size !== compressed)) invalid('ZIP compression ratio');
    total += size;
    if (total > MAX_EXPANDED) invalid('ZIP expanded total');

    b.range(local, 30, central);
    if (b.u32(local) !== 0x04034b50 || b.u16(local + 4) !== version || b.u16(local + 6) !== flags || b.u16(local + 8) !== method || b.u16(local + 26) !== nameSize) invalid('ZIP local header mismatch');
    const localExtra = b.u16(local + 28), start = local + 30 + nameSize + localExtra;
    b.range(local + 30, nameSize + localExtra, central);
    if (!rawName.every((v, n) => v === data[local + 30 + n])) invalid('ZIP local path mismatch');
    extras(b, local + 30 + nameSize, localExtra);
    const localCRC = b.u32(local + 14), localCompressed = b.u32(local + 18), localSize = b.u32(local + 22);
    const descriptor = (flags & 8) !== 0;
    if (descriptor ? ((localCRC !== 0 && localCRC !== crc) || (localCompressed !== 0 && localCompressed !== compressed) || (localSize !== 0 && localSize !== size)) : (localCRC !== crc || localCompressed !== compressed || localSize !== size)) invalid('ZIP local sizes mismatch');
    b.range(start, compressed, central);
    let recordEnd = start + compressed;
    if (descriptor) {
      b.range(recordEnd, 12, central);
      const signed = b.u32(recordEnd) === 0x08074b50;
      const descriptorStart = recordEnd + (signed ? 4 : 0);
      b.range(descriptorStart, 12, central);
      if (b.u32(descriptorStart) !== crc || b.u32(descriptorStart + 4) !== compressed || b.u32(descriptorStart + 8) !== size) invalid('ZIP data descriptor');
      recordEnd = descriptorStart + 12;
    }
    intervals.push({ start: local, end: recordEnd });
    entries.push({ name, directory, method, crc, compressed, size, start });
    pos += 46 + nameSize + extraSize + commentSize;
  }
  if (pos !== end) invalid('ZIP directory size');
  intervals.sort((a, c) => a.start - c.start);
  let previous = 0;
  for (const span of intervals) {
    // Contiguous records reject overlaps, hidden local entries, SFX prefixes and
    // ambiguous gaps. No record (even an empty directory) may alias another.
    if (span.start !== previous) invalid('ZIP overlap or hidden record');
    previous = span.end;
  }
  if (previous !== central) invalid('ZIP record bounds');
  // Binary-search descendants instead of repeatedly joining every ancestor of
  // attacker-controlled deeply nested paths (quadratic in path length).
  const sortedPaths = [...paths].sort();
  for (const file of filePaths) {
    const prefix = `${file}/`;
    let lo = 0, hi = sortedPaths.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (sortedPaths[mid] < prefix) lo = mid + 1; else hi = mid;
    }
    if (sortedPaths[lo]?.startsWith(prefix)) invalid('ZIP file/directory collision');
  }
  return { files: new Map(entries.filter(e => !e.directory).map(e => [e.name, e])), total };
}

const crcTable = Uint32Array.from({ length: 256 }, (_, i) => {
  let n = i; for (let j = 0; j < 8; j++) n = (n >>> 1) ^ ((n & 1) ? 0xedb88320 : 0); return n >>> 0;
});
function updateCRC(crc: number, data: Uint8Array): number {
  for (const n of data) crc = (crc >>> 8) ^ crcTable[(crc ^ n) & 255];
  return crc;
}
function expand(data: Uint8Array, e: ZipEntry, retain = true): Uint8Array {
  // No unchecked archive-wide unzip or declared-size-only inflateSync({out}):
  // a malicious stream can emit MORE than its central size; some fixed-output
  // inflaters silently truncate it. Small input pushes bound fflate's scratch
  // expansion, and the callback independently checks every emitted byte.
  const compressed = data.subarray(e.start, e.start + e.compressed);
  let result: Uint8Array, used = 0, crc = 0xffffffff;
  if (e.method === 0) { result = compressed; used = result.length; crc = updateCRC(crc, result); }
  else {
    result = new Uint8Array(retain ? e.size : 0);
    const inflater = new Inflate(chunk => {
      if (chunk.length > e.size - used) invalid('ZIP actual expanded size');
      crc = updateCRC(crc, chunk);
      if (retain) result.set(chunk, used);
      used += chunk.length;
    });
    try {
      if (!compressed.length) invalid('empty deflate stream');
      for (let offset = 0; offset < compressed.length; offset += 1024) {
        inflater.push(compressed.subarray(offset, Math.min(offset + 1024, compressed.length)), offset + 1024 >= compressed.length);
      }
    } catch (error) {
      if (error instanceof HttpError) throw error;
      invalid('ZIP deflate stream');
    }
  }
  if (used !== e.size || ((crc ^ 0xffffffff) >>> 0) !== e.crc) invalid('ZIP size or CRC32');
  return result;
}

/** Validate the complete ZIP structure before inflating even game.json. */
export async function inspectBundle(data: Uint8Array, wantID: string): Promise<{
  metadata: any; source: any; pck: Uint8Array;
  assets: Array<{ name: string; ext: string; data: Uint8Array }>; engineVersion: string;
}> {
  if (data.length > MAX_BUFFERED_PACKAGE) bufferedLimit();
  if (!ID.test(wantID)) invalid('bundle ID');
  const { files, total } = zipDirectory(data);
  const get = (name: string, limit: number): ZipEntry => {
    if (!safePath(name)) invalid('bundle reference');
    const e = files.get(name);
    if (!e) invalid('missing bundle entry');
    if (e.size > limit) {
      if (limit === MAX_BUFFERED_PACKAGE) bufferedLimit();
      invalid('bundle entry size');
    }
    return e;
  };
  const metaEntry = get('game.json', MAX_META), raw = expand(data, metaEntry);
  let m: ObjectValue;
  try { m = object(JSON.parse(decode(raw))); } catch { invalid('bundle metadata JSON'); }
  if (m.id !== wantID) invalid('bundle ID');
  // Visibility and all server-owned attributes in a .game are ignored, never
  // used to publish. The upload route preserves visibility or defaults private.
  const metadata = validateMetadata({ ...m, visibility: undefined });
  const source = validateSource(m.source);
  const entry = string(m.entry, 65535) || 'content/game.pck';
  if (!entry.endsWith('.pck')) invalid('bundle PCK entry');
  const pckEntry = get(entry, MAX_BUFFERED_PACKAGE), a = object(m.assets);
  const assetEntries = [get(string(a.cover, 65535, true), MAX_COVER), ...array(a.screenshots, 8).map(n => get(string(n, 65535, true), MAX_SHOT))];
  const selected = new Set([metaEntry, pckEntry, ...assetEntries]);
  let retained = 0;
  for (const e of selected) retained += e.size;
  if (retained > MAX_BUFFERED_PACKAGE) bufferedLimit();
  // For small archives, CRC-check even unused documentation/content without
  // retaining it. Large unselected entries remain statically checked only.
  if (total <= MAX_BUFFERED_PACKAGE) {
    for (const e of files.values()) if (!selected.has(e)) expand(data, e, false);
  }
  const pck = expand(data, pckEntry), { engineVersion } = await inspectPCK(pck);
  if (metadata.engine.version !== engineVersion) invalid('bundle engine version mismatch');
  const cache = new Map<ZipEntry, Uint8Array>();
  const assets = assetEntries.map(e => {
    let bytes = cache.get(e);
    if (!bytes) { bytes = expand(data, e); cache.set(e, bytes); }
    const ext = imageExtension(bytes);
    if (!ext) invalid('bundle image');
    return { name: e.name, ext, data: bytes };
  });
  return { metadata, source, pck, assets, engineVersion };
}
