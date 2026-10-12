import assert from 'node:assert/strict';
import test from 'node:test';
import { createHash } from 'node:crypto';
import { deflateRawSync } from 'node:zlib';
import { imageExtension, inspectBundle, inspectPCK, MAX_BUFFERED_PACKAGE, safePath, validateMetadata } from '../src/package';

const enc = new TextEncoder();
const text = (s: string) => enc.encode(s);
const png = Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10, 1, 2, 3]);
const view = (b: Uint8Array) => new DataView(b.buffer, b.byteOffset, b.byteLength);
const u32 = (b: Uint8Array, at: number, n: number) => view(b).setUint32(at, n, true);
const u16 = (b: Uint8Array, at: number, n: number) => view(b).setUint16(at, n, true);
const u64 = (b: Uint8Array, at: number, n: number | bigint) => view(b).setBigUint64(at, BigInt(n), true);
const join = (...parts: Uint8Array[]) => {
  const result = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0; for (const p of parts) { result.set(p, at); at += p.length; } return result;
};

// Mirrors Go library_test.go fixture: V2 directory at 96; V3/4 put payload
// first (base112), with the actual directory offset in header byte32.
function pckFixture(format = 2, names = ['res://main.gd'], contents = names.map(() => text('content'))) {
  const encoded = names.map(text), directorySize = 4 + encoded.reduce((n, s) => n + 4 + s.length + 36, 0);
  const header = new Uint8Array(format === 2 ? 96 : 112);
  header.set(text('GDPC')); u32(header, 4, format); u32(header, 8, 4); u32(header, 12, 7); u32(header, 20, 2);
  const payload = join(...contents), directory = new Uint8Array(directorySize);
  const base = format === 2 ? header.length + directory.length : 112;
  u64(header, 24, base); if (format !== 2) u64(header, 32, 112 + payload.length);
  u32(directory, 0, names.length);
  let at = 4, offset = 0;
  const records: number[] = [];
  for (let i = 0; i < names.length; i++) {
    u32(directory, at, encoded[i].length); at += 4;
    directory.set(encoded[i], at); at += encoded[i].length;
    records.push(at); u64(directory, at, offset); u64(directory, at + 8, contents[i].length);
    directory.set(createHash('md5').update(contents[i]).digest(), at + 16);
    at += 36; offset += contents[i].length;
  }
  const dirStart = format === 2 ? 96 : 112 + payload.length;
  return {
    bytes: format === 2 ? join(header, directory, payload) : join(header, payload, directory),
    records: records.map(n => n + dirStart), base, dirStart,
  };
}

function crc32(data: Uint8Array): number {
  let crc = 0xffffffff;
  for (const b of data) {
    crc ^= b; for (let i = 0; i < 8; i++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}
interface Entry {
  name: string; data: Uint8Array; method?: number; flags?: number; attrs?: number;
  extra?: Uint8Array; descriptor?: boolean; unsignedDescriptor?: boolean;
}
// Independent ZIP writer: Node zlib + independent CRC, not fflate's ZIP writer.
function zipFixture(entries: Entry[]) {
  const locals: Uint8Array[] = [], centrals: Uint8Array[] = [];
  const info: Array<{ local: number; central: number; start: number; end: number }> = [];
  let offset = 0, centralOffset = 0;
  for (const e of entries) {
    const name = text(e.name), method = e.method ?? 8, flags = (e.flags ?? 0x800) | (e.descriptor ? 8 : 0);
    const compressed = method === 8 ? deflateRawSync(e.data) : e.data;
    const crc = crc32(e.data), extra = e.extra ?? new Uint8Array();
    const local = new Uint8Array(30 + name.length + extra.length);
    u32(local, 0, 0x04034b50); u16(local, 4, 20); u16(local, 6, flags); u16(local, 8, method);
    if (!e.descriptor) { u32(local, 14, crc); u32(local, 18, compressed.length); u32(local, 22, e.data.length); }
    u16(local, 26, name.length); u16(local, 28, extra.length); local.set(name, 30); local.set(extra, 30 + name.length);
    const descriptor = new Uint8Array(e.descriptor ? (e.unsignedDescriptor ? 12 : 16) : 0);
    if (e.descriptor) {
      const shift = e.unsignedDescriptor ? 0 : 4;
      if (shift) u32(descriptor, 0, 0x08074b50);
      u32(descriptor, shift, crc); u32(descriptor, shift + 4, compressed.length); u32(descriptor, shift + 8, e.data.length);
    }
    const central = new Uint8Array(46 + name.length + extra.length);
    u32(central, 0, 0x02014b50); u16(central, 4, 0x0314); u16(central, 6, 20); u16(central, 8, flags); u16(central, 10, method);
    u32(central, 16, crc); u32(central, 20, compressed.length); u32(central, 24, e.data.length);
    u16(central, 28, name.length); u16(central, 30, extra.length); u32(central, 38, e.attrs ?? 0); u32(central, 42, offset);
    central.set(name, 46); central.set(extra, 46 + name.length);
    info.push({ local: offset, central: centralOffset, start: offset + local.length, end: offset + local.length + compressed.length });
    locals.push(local, compressed, descriptor); centrals.push(central);
    offset += local.length + compressed.length + descriptor.length; centralOffset += central.length;
  }
  const end = new Uint8Array(22);
  u32(end, 0, 0x06054b50); u16(end, 8, entries.length); u16(end, 10, entries.length);
  u32(end, 12, centralOffset); u32(end, 16, offset);
  return { bytes: join(...locals, ...centrals, end), entries: info.map(e => ({ ...e, central: e.central + offset })), central: offset, end: offset + centralOffset };
}
function metadata(patch: Record<string, unknown> = {}) {
  return { id: 'demo-game', title: '演示游戏', engine: { name: 'godot', version: '4.7' },
    source: { repo: 'https://example.com/game', ref: 'v1.0', commit: '0123456789abcdef0123456789abcdef01234567', license: 'MIT' },
    assets: { cover: 'assets/cover.png' }, ...patch };
}
function entries(patch: Record<string, unknown> = {}): Entry[] {
  return [
    { name: 'game.json', data: text(JSON.stringify(metadata(patch))) },
    { name: 'assets/cover.png', data: png },
    { name: 'content/game.pck', data: pckFixture().bytes },
  ];
}
const rejectPCK = (b: Uint8Array) => assert.rejects(inspectPCK(b), /invalid|limit/);
const rejectZIP = (b: Uint8Array) => assert.rejects(inspectBundle(b, 'demo-game'), /invalid|limit/);

for (const format of [2, 3, 4]) test(`PCK accepts Go-compatible format ${format}`, async () => {
  assert.deepEqual(await inspectPCK(pckFixture(format).bytes), { engineVersion: '4.7' });
  assert.deepEqual(await inspectPCK(pckFixture(format, ['res://main.gd\0\0']).bytes), { engineVersion: '4.7' });
});

test('PCK accepts zero length payload and subarray input without offset confusion', async () => {
  const { bytes } = pckFixture(2, ['empty.gd', 'other.gd'], [new Uint8Array(), text('valid')]);
  const padded = join(text('prefix'), bytes, text('suffix'));
  assert.equal((await inspectPCK(padded.subarray(6, 6 + bytes.length))).engineVersion, '4.7');
});

const badPaths = ['../bad', 'res://a/../../evil', 'a\\b', 'C:/bad', '/absolute', 'a//b', 'a/./b', 'evil.DLL', 'lib.so.1', 'a. ', 'a\0b', 'a%2fb', 'foo.gdextension', 'foo\u0085bar', 'res://', 'a/', 'a\ud800'];
for (const name of badPaths) test(`safePath rejects ${JSON.stringify(name)}`, () => assert.equal(safePath(name), false));
for (const name of ['res://main.gd', '汉字/场景.tscn', '.godot/imported/x.ctex', 'image😀.png', 'LICENSE']) test(`safePath accepts ${name}`, () => assert.equal(safePath(name), true));
for (const name of badPaths.filter(n => !n.includes('\ud800'))) test(`PCK rejects path ${JSON.stringify(name)}`, async () => rejectPCK(pckFixture(4, [name]).bytes));

test('PCK rejects invalid UTF-8 instead of replacement decoding', async () => {
  const f = pckFixture(); f.bytes[f.dirStart + 8] = 0xff; await rejectPCK(f.bytes);
});
for (const names of [['main.gd', 'res://MAIN.gd'], ['café.gd', 'cafe\u0301.gd'], ['ΟΣ.gd', 'οσ.gd']]) test(`PCK rejects normalized duplicate ${names}`, async () => rejectPCK(pckFixture(2, names).bytes));

const pckCorruptions: Record<string, (f: ReturnType<typeof pckFixture>) => void> = {
  'unknown format': f => u32(f.bytes, 4, 5), 'wrong engine': f => u32(f.bytes, 8, 3), 'minor overflow': f => u32(f.bytes, 12, 1000),
  'encrypted directory': f => u32(f.bytes, 20, 1), 'sparse flag': f => u32(f.bytes, 20, 4),
  'unknown entry flags': f => u32(f.bytes, f.records[0] + 32, 2),
  'base unsafe integer': f => u64(f.bytes, 24, 2n ** 53n), 'offset unsafe integer': f => u64(f.bytes, f.records[0], 2n ** 63n),
  'length unsafe integer': f => u64(f.bytes, f.records[0] + 8, 2n ** 63n),
  'base beyond file': f => u64(f.bytes, 24, f.bytes.length + 1), 'offset outside': f => u64(f.bytes, f.records[0], f.bytes.length),
  'length outside': f => u64(f.bytes, f.records[0] + 8, f.bytes.length),
  'payload points inside directory': f => { u64(f.bytes, 24, 0); u64(f.bytes, f.records[0], f.dirStart); },
  'payload points inside header': f => { u64(f.bytes, 24, 0); u64(f.bytes, f.records[0], 1); },
  'MD5 mismatch': f => { f.bytes[f.base] ^= 1; },
  'zero file count': f => u32(f.bytes, f.dirStart, 0), 'huge file count': f => u32(f.bytes, f.dirStart, 100001),
  'oversized path': f => u32(f.bytes, f.dirStart + 4, 4097), 'empty path': f => u32(f.bytes, f.dirStart + 4, 0),
  'embedded executable': f => f.bytes.set(text('MZ!!')),
};
for (const [name, mutate] of Object.entries(pckCorruptions)) test(`PCK rejects ${name}`, async () => {
  const f = pckFixture(); mutate(f); await rejectPCK(f.bytes);
});
test('PCK rejects overlapping payloads', async () => {
  const f = pckFixture(2, ['a', 'b']); u64(f.bytes, f.records[1], 0); await rejectPCK(f.bytes);
});
for (const format of [3, 4]) test(`PCK ${format} rejects bad directory/header offsets`, async () => {
  for (const offset of [0n, 96n, 103n, 2n ** 53n, 2n ** 64n - 1n]) {
    const f = pckFixture(format); u64(f.bytes, 32, offset); await rejectPCK(f.bytes);
  }
});
test('PCK rejects every truncated prefix', async () => {
  for (const format of [2, 3, 4]) { const f = pckFixture(format); for (let n = 0; n < f.bytes.length; n++) await rejectPCK(f.bytes.subarray(0, n)); }
});
test('buffered size cap is explicitly 32 MiB, including PCK', async () => {
  assert.equal(MAX_BUFFERED_PACKAGE, 32 * 1024 * 1024);
  const large = new Uint8Array(MAX_BUFFERED_PACKAGE + 1);
  await assert.rejects(inspectPCK(large), /conservatively 32 MiB/);
  await assert.rejects(inspectBundle(large, 'demo-game'), /conservatively 32 MiB/);
});

test('metadata sanitizes unknown platform fields; lengths are UTF-8 bytes', () => {
  const m = validateMetadata(metadata({ title: '中'.repeat(170), tags: [''], visibility: 'public', ownerId: 123, modes: [{ id: 'easy', label: '轻松' }] }));
  assert.equal(m.title.length, 170); assert.equal(m.visibility, 'public'); assert.equal(m.modes[0].label, '轻松'); assert.ok(!('ownerId' in m));
  assert.throws(() => validateMetadata(metadata({ title: '中'.repeat(171) })), /metadata/);
});
for (const patch of [
  { title: '' }, { title: 'x'.repeat(513) }, { subtitle: 'x'.repeat(1025) }, { description: 'x'.repeat(8193) },
  { tags: Array(17).fill('a') }, { tags: ['中'.repeat(43)] }, { title: 2 }, { title: '\ud800' },
  { tags: 'x' }, { accent: 'red' }, { visibility: 'privatex' }, { visibility: 'friends' },
  { engine: { name: 'unity', version: '4.7' } }, { engine: { name: 'godot', version: '4.1000' } },
  { engine: { name: 'godot', version: '4' } }, { modes: [{ id: 'easy', label: '' }] },
  { modes: [{ id: 'Easy', label: 'easy' }] }, { modes: [{ id: 'easy', label: 'a'.repeat(13) }] },
  { modes: [{ id: 'easy', label: 'a' }, { id: 'easy', label: 'b' }] },
]) test(`metadata rejects ${JSON.stringify(patch).slice(0, 70)}`, () => assert.throws(() => validateMetadata(metadata(patch)), /invalid/));

test('image extensions use magic, not paths', () => {
  assert.equal(imageExtension(png), '.png'); assert.equal(imageExtension(Uint8Array.from([255, 216, 255])), '.jpg');
  assert.equal(imageExtension(text('RIFFxxxxWEBP')), '.webp'); assert.equal(imageExtension(text('<svg/>')), '');
});

for (const method of [0, 8]) test(`bundle accepts valid ${method ? 'deflate' : 'store'} ZIP and preserves source`, async () => {
  const raw = zipFixture(entries({ visibility: 'public', ownerId: 42, modes: [{ id: 'easy', label: '轻松' }] }).map(e => ({ ...e, method })));
  const b = await inspectBundle(raw.bytes, 'demo-game');
  assert.equal(b.engineVersion, '4.7'); assert.equal(b.metadata.title, '演示游戏'); assert.equal(b.metadata.visibility, '');
  assert.equal(b.metadata.modes[0].label, '轻松'); assert.ok(!('ownerId' in b.metadata));
  assert.equal(b.source.repo, 'https://example.com/game'); assert.deepEqual(b.pck, pckFixture().bytes);
  assert.equal(b.assets[0].name, 'assets/cover.png'); assert.equal(b.assets[0].ext, '.png'); assert.deepEqual(b.assets[0].data, png);
});
for (const unsignedDescriptor of [false, true]) test(`ZIP supports ${unsignedDescriptor ? 'unsigned' : 'signed'} streaming descriptors`, async () => {
  await inspectBundle(zipFixture(entries().map(e => ({ ...e, descriptor: true, unsignedDescriptor }))).bytes, 'demo-game');
});
test('ZIP supports genuine empty directory entries and unused text', async () => {
  const es = entries(); es.splice(1, 0, { name: 'assets/', data: new Uint8Array(), method: 0, attrs: 0x41ed0010 });
  es.push({ name: 'README.md', data: text('hello') }); await inspectBundle(zipFixture(es).bytes, 'demo-game');
});
test('ZIP accepts bounded realistic image and optional screenshots', async () => {
  const image = new Uint8Array(300 * 1024); image.set(png);
  for (let i = png.length; i < image.length; i++) image[i] = (i * 7 + (i >>> 8)) & 255;
  const es = entries({ assets: { cover: 'assets/cover.png', screenshots: ['assets/shot.png'] } });
  es[1].data = image; es.push({ name: 'assets/shot.png', data: png });
  const b = await inspectBundle(zipFixture(es).bytes, 'demo-game'); assert.equal(b.assets.length, 2); assert.equal(b.assets[0].data.length, image.length);
});
for (const name of ['../evil', '/evil', 'x.dll', 'lib.so.1', 'a//b', 'a%2f', 'bad\\x', 'assets/bad.']) test(`ZIP rejects path ${name}`, async () => {
  await rejectZIP(zipFixture([...entries(), { name, data: text('payload') }]).bytes);
});
for (const name of ['GAME.json', 'res://game.json', 'assets/cover.png/child']) test(`ZIP rejects duplicate/file-directory collision ${name}`, async () => {
  await rejectZIP(zipFixture([...entries(), { name, data: text('payload') }]).bytes);
});
test('ZIP rejects Unicode normalized aliases and duplicate directory records', async () => {
  for (const names of [['café', 'cafe\u0301'], ['assets/', 'ASSETS/']]) {
    await rejectZIP(zipFixture([...entries(), ...names.map(name => ({ name, data: new Uint8Array(), method: 0 }))]).bytes);
  }
});
for (const attrs of [0xa1ff0000, 0x21ff0000, 0x11ff0000]) test(`ZIP rejects Unix link/special mode ${attrs.toString(16)}`, async () => {
  await rejectZIP(zipFixture([...entries(), { name: 'assets/link', data: text('target'), attrs }]).bytes);
});
for (const flags of [1, 0x40, 0x2000, 0x10]) test(`ZIP rejects encryption/unsupported flag ${flags}`, async () => {
  await rejectZIP(zipFixture(entries().map(e => ({ ...e, flags }))).bytes);
});
test('ZIP rejects encrypted directory entries too', async () => {
  await rejectZIP(zipFixture([...entries(), { name: 'assets/', data: new Uint8Array(), method: 0, flags: 1 }]).bytes);
});
for (const kind of [1, 0x7075, 0x000d, 0x756e, 0x0017, 0x9901]) test(`ZIP fails closed for extra field ${kind.toString(16)}`, async () => {
  const extra = new Uint8Array(4); u16(extra, 0, kind);
  await rejectZIP(zipFixture(entries().map(e => ({ ...e, extra }))).bytes);
});
test('ZIP rejects real compression bomb before inflate', async () => {
  await assert.rejects(inspectBundle(zipFixture([...entries(), { name: 'bomb.bin', data: new Uint8Array(1024 * 1024) }]).bytes, 'demo-game'), /compression ratio/);
});
test('ZIP bounds actual output even when the central and local sizes lie', async () => {
  const es = entries(); es.push({ name: 'bomb.bin', data: new Uint8Array(1024 * 1024) }); const z = zipFixture(es), e = z.entries[3];
  u32(z.bytes, e.local + 22, 1000); u32(z.bytes, e.central + 24, 1000);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /actual expanded size/);
});
test('ZIP rejects CRC mismatch on selected and unused files', async () => {
  for (const index of [0, 1, 2, 3]) {
    const z = zipFixture([...entries(), { name: 'README.md', data: text('unused') }].map(e => ({ ...e, method: 0 })));
    z.bytes[z.entries[index].start] ^= 1; await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /CRC32/);
  }
});
test('ZIP checks exact ratio rather than floored division', async () => {
  const z = zipFixture([...entries(), { name: 'bomb.bin', data: text('small') }]), e = z.entries[3];
  const n = view(z.bytes).getUint32(e.central + 20, true) * 200 + 1;
  u32(z.bytes, e.local + 22, n); u32(z.bytes, e.central + 24, n);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /compression ratio/);
});
test('ZIP caps expanded PCK to32MiB before allocating it', async () => {
  // Random-ish 256 KiB compressed stream with a forged central size above32MiB,
  // ratio still <200. Must hit conservative cap, not attempt decompression.
  const es = entries(), payload = new Uint8Array(256 * 1024); let x = 123456789;
  for (let i = 0; i < payload.length; i++) { x ^= x << 13; x ^= x >>> 17; x ^= x << 5; payload[i] = x; }
  es[2].data = payload; const z = zipFixture(es), e = z.entries[2];
  u32(z.bytes, e.local + 22, MAX_BUFFERED_PACKAGE + 1); u32(z.bytes, e.central + 24, MAX_BUFFERED_PACKAGE + 1);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /conservatively 32 MiB/);
});

type Zip = ReturnType<typeof zipFixture>;
const zipCorruptions: Record<string, (z: Zip) => void> = {
  'multidisk EOCD': z => u16(z.bytes, z.end + 4, 1), 'entry count mismatch': z => u16(z.bytes, z.end + 8, 2),
  'entry count512 cap': z => { u16(z.bytes, z.end + 8, 513); u16(z.bytes, z.end + 10, 513); },
  'ZIP64 sentinel': z => u32(z.bytes, z.entries[0].central + 24, 0xffffffff),
  'ZIP64 version': z => { u16(z.bytes, z.entries[0].central + 6, 45); u16(z.bytes, z.entries[0].local + 4, 45); },
  'central offset outside': z => u32(z.bytes, z.end + 16, 0xfffffff0), 'central size mismatch': z => u32(z.bytes, z.end + 12, 4),
  'central signature': z => u32(z.bytes, z.central, 0), 'local signature': z => u32(z.bytes, 0, 0),
  'local version mismatch': z => u16(z.bytes, z.entries[0].local + 4, 10),
  'local encrypted mismatch': z => u16(z.bytes, z.entries[0].local + 6, 1),
  'local path mismatch': z => { z.bytes[z.entries[0].local + 30] = 120; },
  'invalid UTF8 path': z => { z.bytes[z.entries[0].local + 30] = 0xff; z.bytes[z.entries[0].central + 46] = 0xff; },
  'local CRC mismatch': z => u32(z.bytes, z.entries[0].local + 14, 99),
  'local length mismatch': z => u32(z.bytes, z.entries[0].local + 22, 99),
  'compressed payload into central directory': z => { u32(z.bytes, z.entries[0].local + 18, z.bytes.length); u32(z.bytes, z.entries[0].central + 20, z.bytes.length); },
  'aliased local record': z => u32(z.bytes, z.entries[1].central + 42, 0),
  'local header inside central': z => u32(z.bytes, z.entries[1].central + 42, z.central),
};
for (const [name, mutate] of Object.entries(zipCorruptions)) test(`ZIP rejects ${name}`, async () => {
  const z = zipFixture(entries()); mutate(z); await rejectZIP(z.bytes);
});
test('ZIP rejects overlapping valid local records hidden within stored payload', async () => {
  const inner = zipFixture([{ name: 'evil.txt', data: text('data'), method: 0 }]);
  const es = entries(); es.push({ name: 'outer.bin', data: inner.bytes.subarray(0, inner.central), method: 0 }); es.push({ name: 'evil.txt', data: text('data'), method: 0 });
  const z = zipFixture(es); u32(z.bytes, z.entries[4].central + 42, z.entries[3].start); await rejectZIP(z.bytes);
});
test('ZIP rejects invalid data descriptor', async () => {
  const z = zipFixture(entries().map(e => ({ ...e, descriptor: true }))); u32(z.bytes, z.entries[0].end + 4, 123); await rejectZIP(z.bytes);
});
test('ZIP rejects all truncated prefixes', async () => {
  const z = zipFixture(entries()); for (let n = 0; n < z.bytes.length; n++) await rejectZIP(z.bytes.subarray(0, n));
});
for (const patch of [
  { id: 'other' }, { id: 'Bad_ID' }, { engine: { name: 'godot', version: '4.6' } },
  { source: { repo: 'https://user:pass@example.com/repo' } }, { source: { repo: 'https://@example.com/repo' } },
  { source: { repo: 'file:///etc/passwd' } }, { source: { repo: 'https://example.com\\@evil.test' } }, { source: { commit: 'short' } },
  { entry: '../missing.pck' }, { entry: 'content/missing.pck' }, { entry: 'content/game.zip' },
  { assets: {} }, { assets: { cover: 'assets/cover.png', screenshots: Array(9).fill('assets/cover.png') } },
]) test(`bundle rejects metadata ${JSON.stringify(patch).slice(0, 70)}`, async () => rejectZIP(zipFixture(entries(patch)).bytes));
test('bundle rejects invalid PCK even with valid ZIP CRC', async () => {
  const es = entries(); es[2].data[es[2].data.length - 1] ^= 1; await rejectZIP(zipFixture(es).bytes);
});
test('bundle rejects oversized metadata without inflating', async () => {
  const z = zipFixture(entries()); u32(z.bytes, z.entries[0].local + 22, 65537); u32(z.bytes, z.entries[0].central + 24, 65537); await rejectZIP(z.bytes);
});
test('bundle rejects JSON invalid UTF8 and nonimage cover', async () => {
  for (const n of [0, 1]) { const es = entries(); es[n].data = Uint8Array.of(255); await rejectZIP(zipFixture(es).bytes); }
});

test('PCK supports absolute offsets with base0 and clear relative flag', async () => {
  const f = pckFixture(4); u32(f.bytes, 20, 0); u64(f.bytes, 24, 0); u64(f.bytes, f.records[0], f.base);
  assert.equal((await inspectPCK(f.bytes)).engineVersion, '4.7');
});
test('ZIP enforces768MiB theoretical total independently of per-entry ratio', async () => {
  const es = entries();
  // Stored bytes do not need to be inflated: every forged entry passes ratio200,
  // but their aggregate declared size exceeds768MiB before metadata decoding.
  for (let i = 0; i < 25; i++) es.push({ name: `extra-${i}`, data: new Uint8Array(256 * 1024), method: 0 });
  const z = zipFixture(es);
  for (const e of z.entries.slice(3)) {
    u16(z.bytes, e.local + 8, 8); u16(z.bytes, e.central + 10, 8);
    u32(z.bytes, e.local + 22, MAX_BUFFERED_PACKAGE); u32(z.bytes, e.central + 24, MAX_BUFFERED_PACKAGE);
  }
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /ZIP expanded total/);
});
test('ZIP enforces combined retained expansion cap before payload allocation', async () => {
  const es = entries({ assets: { cover: 'assets/cover.png', screenshots: ['assets/shot.png'] } });
  es[2] = { ...es[2], data: new Uint8Array(256 * 1024), method: 0 };
  es.push({ name: 'assets/shot.png', data: new Uint8Array(64 * 1024), method: 0 });
  const z = zipFixture(es);
  for (const [i, n] of [[2, 30 * 1024 * 1024], [3, 4 * 1024 * 1024]]) {
    const e = z.entries[i]; u16(z.bytes, e.local + 8, 8); u16(z.bytes, e.central + 10, 8);
    u32(z.bytes, e.local + 22, n); u32(z.bytes, e.central + 24, n);
  }
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /conservatively 32 MiB/);
});
for (const isCover of [true, false]) test(`ZIP enforces${isCover ? '4MiB cover' : '8MiB screenshot'} cap before inflate`, async () => {
  const es = entries(isCover ? {} : { assets: { cover: 'assets/cover.png', screenshots: ['assets/shot.png'] } });
  if (!isCover) es.push({ name: 'assets/shot.png', data: png });
  const index = isCover ? 1 : 3;
  es[index] = { ...es[index], data: new Uint8Array(64 * 1024), method: 0 };
  const z = zipFixture(es), e = z.entries[index], n = (isCover ? 4 : 8) * 1024 * 1024 + 1;
  u16(z.bytes, e.local + 8, 8); u16(z.bytes, e.central + 10, 8);
  u32(z.bytes, e.local + 22, n); u32(z.bytes, e.central + 24, n);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /bundle entry size/);
});
test('ZIP rejects unrecognized compression and malformed extras', async () => {
  await rejectZIP(zipFixture(entries().map(e => ({ ...e, method: 12 }))).bytes);
  for (const extra of [Uint8Array.of(0x55), Uint8Array.of(0x55, 0x54, 99, 0)]) {
    await rejectZIP(zipFixture(entries().map(e => ({ ...e, extra }))).bytes);
  }
});
test('ZIP rejects directories with payloads', async () => {
  await rejectZIP(zipFixture([...entries(), { name: 'assets/', data: text('hidden'), method: 0 }]).bytes);
});
test('ZIP does not skip selected CRC checks when unselected declared expansion is large', async () => {
  const es = entries(); es.push({ name: 'unused.bin', data: new Uint8Array(256 * 1024), method: 0 });
  const z = zipFixture(es), extra = z.entries[3];
  u16(z.bytes, extra.local + 8, 8); u16(z.bytes, extra.central + 10, 8);
  u32(z.bytes, extra.local + 22, MAX_BUFFERED_PACKAGE + 1); u32(z.bytes, extra.central + 24, MAX_BUFFERED_PACKAGE + 1);
  // The unused expansion is intentionally not decoded, but selected CRCs always are.
  u32(z.bytes, z.entries[1].central + 16, 123); u32(z.bytes, z.entries[1].local + 14, 123);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /CRC32/);
});
test('ZIP validates all structural entries before attempting any decompression', async () => {
  const z = zipFixture([...entries(), { name: '../bad', data: text('unsafe') }]);
  z.bytes.fill(255, z.entries[0].start, z.entries[0].end);
  await assert.rejects(inspectBundle(z.bytes, 'demo-game'), /ZIP path/);
});
test('ZIP never accepts a truncated deflate stream with correct prefix CRC', async () => {
  const es = entries(); es.push({ name: 'broken.txt', data: text('hello'), method: 0 });
  const z = zipFixture(es), e = z.entries[3];
  // Incomplete raw-DEFLATE stored block: BFINAL=1, LEN5, NLEN=~5, but no data.
  z.bytes.set(Uint8Array.of(1, 5, 0, 250, 255), e.start);
  u16(z.bytes, e.local + 8, 8); u16(z.bytes, e.central + 10, 8);
  await rejectZIP(z.bytes);
});
