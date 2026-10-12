#!/usr/bin/env node
/**
 * Copy the public catalog of one Game Stop instance into another.
 *
 * Only games the source lists as public are read, and only content the source
 * already hands to anonymous clients is transferred. Accounts, private games,
 * scores and leaderboards are never touched. Each package is verified against
 * the source catalog's size and SHA-256 before it is rebuilt as a `.game`
 * container and uploaded with the `gamestop` CLI.
 *
 * Usage:
 *   GAMESTOP_TOKEN=... node scripts/migrate-public-games.mjs \
 *     --source https://game-stop.baobaobao.online --target https://game-stop.giveme8.com
 *
 * The token is read from the environment only: it is never printed, logged or
 * written to disk by this script.
 */
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { zipSync } from 'fflate';

const BUFFERED_LIMIT = 32 * 1024 * 1024;

function parseArgs(argv) {
  const args = { source: 'https://game-stop.baobaobao.online', target: '', only: [], limit: 0, keep: false, timeout: 120000, records: '', verify: true };
  for (let index = 0; index < argv.length; index++) {
    const flag = argv[index];
    const next = () => argv[++index];
    if (flag === '--source') args.source = next().replace(/\/$/, '');
    else if (flag === '--target') args.target = next().replace(/\/$/, '');
    else if (flag === '--only') args.only = next().split(',').map(s => s.trim()).filter(Boolean);
    else if (flag === '--limit') args.limit = Number(next());
    else if (flag === '--timeout') args.timeout = Number(next());
    else if (flag === '--records') args.records = next();
    else if (flag === '--no-verify') args.verify = false;
    else if (flag === '--keep') args.keep = true;
    else throw new Error(`unknown flag ${flag}`);
  }
  for (const [name, value] of [['--source', args.source], ['--target', args.target]]) {
    if (!value.startsWith('https://')) throw new Error(`${name} must be an HTTPS origin`);
    new URL(value);
  }
  if (!args.target) throw new Error('--target is required');
  return args;
}

async function fetchBytes(url, timeout) {
  const response = await fetch(url, { redirect: 'error', signal: AbortSignal.timeout(timeout) });
  if (!response.ok) throw new Error(`GET ${url} -> ${response.status}`);
  return new Uint8Array(await response.arrayBuffer());
}
async function fetchJSON(url, timeout) {
  return JSON.parse(new TextDecoder().decode(await fetchBytes(url, timeout)));
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

/** Minimal GDPC header read: the pck must declare 4.x and stay within limits. */
function pckEngineVersion(bytes) {
  if (bytes.length < 100 || new TextDecoder().decode(bytes.subarray(0, 4)) !== 'GDPC') throw new Error('not a Godot PCK');
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const major = view.getUint32(8, true);
  const minor = view.getUint32(12, true);
  if (major !== 4) throw new Error(`unsupported engine major ${major}`);
  return `${major}.${minor}`;
}

function imageExtension(bytes) {
  if (bytes.length >= 8 && [137, 80, 78, 71, 13, 10, 26, 10].every((n, i) => bytes[i] === n)) return '.png';
  if (bytes.length >= 3 && bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) return '.jpg';
  if (bytes.length >= 12 && bytes[8] === 87 && bytes[9] === 69 && bytes[10] === 66 && bytes[11] === 80) return '.webp';
  return '';
}

const TRANSIENT = /TLS handshake timeout|connection reset|PROTOCOL_ERROR|cannot retry|stream error|GOAWAY|connection refused|EOF|timeout|temporarily unavailable|502|503|504|broken pipe/i;

/** Retry only on transport failures: an application-level rejection is final. */
function runCLIRetry(args, env, attempts = 5) {
  let last;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try { return runCLI(args, env); }
    catch (error) {
      last = error;
      if (!TRANSIENT.test(error.message)) throw error;
      const wait = Math.min(30000, 2000 * attempt);
      process.stderr.write(`  retry ${attempt}/${attempts} ${args[0]} ${args[2] ?? ''}: ${error.message.slice(0, 120)}\n`);
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, wait);
    }
  }
  throw last;
}

function runCLI(args, env) {
  const result = spawnSync('gamestop', args, { encoding: 'utf8', env, timeout: 900000 });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`gamestop ${args[0]} failed (${result.status}): ${(result.stderr || result.stdout || '').trim()}`);
  return result.stdout;
}

/** Read the target instance's own records, including the caller's private ones. */
function targetGames(env, target, anonymous) {
  const scoped = anonymous ? { ...env, GAMESTOP_TOKEN: '' } : env;
  const parsed = JSON.parse(runCLIRetry(['list', '--server', target], scoped));
  return new Map((parsed.games ?? []).map(game => [game.id, game]));
}
function verifyAnonymousDownload(env, target, record, workDir) {
  const file = join(workDir, `verify-${record.id}.pck`);
  runCLIRetry(['download', '--server', target, '--id', record.id, '--file', file], { ...env, GAMESTOP_TOKEN: '' });
  const bytes = readFileSync(file);
  rmSync(file, { force: true });
  return { ok: bytes.length === record.pckBytes && sha256(bytes) === record.pckSha256, anonymousBytes: bytes.length };
}

function gameJSON(entry, cover) {
  const doc = { id: entry.id, title: entry.title, engine: { name: 'godot', version: entry.engine.version }, entry: 'content/game.pck' };
  for (const field of ['subtitle', 'description', 'accent']) if (entry[field]) doc[field] = entry[field];
  if (Array.isArray(entry.tags) && entry.tags.length) doc.tags = entry.tags;
  if (cover) doc.assets = { cover: `assets/cover${cover.ext}` };
  return doc;
}

/**
 * Per-game digests of what this tool published, so a later run can still verify
 * games whose upload was skipped (already present, or retried after a network
 * failure). Only digests the source catalog already publishes are stored.
 */
function mergeRecords(copied, recordsPath) {
  let existing = [];
  if (recordsPath) {
    try { existing = JSON.parse(readFileSync(recordsPath, 'utf8')); } catch { existing = []; }
    if (!Array.isArray(existing)) existing = [];
  }
  const byID = new Map(existing.map(record => [record.id, record]));
  for (const record of copied) {
    byID.set(record.id, { id: record.id, pckBytes: record.pckBytes, pckSha256: record.pckSha256 });
  }
  const merged = [...byID.values()].sort((a, b) => (a.id < b.id ? -1 : 1));
  if (recordsPath) {
    mkdirSync(dirname(recordsPath), { recursive: true });
    writeFileSync(recordsPath, JSON.stringify(merged, null, 2));
  }
  return merged;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const token = process.env.GAMESTOP_TOKEN;
  if (!token) throw new Error('GAMESTOP_TOKEN is required');
  const env = { ...process.env, GAMESTOP_TOKEN: token };

  const sourceCatalog = await fetchJSON(`${args.source}/gamesource/v1/index.json`, args.timeout);
  if (sourceCatalog.protocol !== 'gamesource/1') throw new Error(`unsupported source protocol ${sourceCatalog.protocol}`);
  let targetIDs = new Set();
  try { targetIDs = new Set(targetGames(env, args.target, false).keys()); }
  catch { /* a first deployment has an empty or not-yet-created catalog */ }

  let selected = (sourceCatalog.games ?? []).filter(game => game.status !== 'hidden');
  if (args.only.length) selected = selected.filter(game => args.only.includes(game.id));
  if (args.limit) selected = selected.slice(0, args.limit);

  const work = mkdtempSync(join(tmpdir(), 'gamestop-migrate-'));
  const recordsPath = args.records || join(work, 'migrated-records.json');
  const report = { copied: [], skipped: [], failed: [], barePCK: [] };
  try {
    for (const game of selected) {
      const pkg = game.package ?? {};
      try {
        if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(game.id)) throw new Error('unsafe game id');
        if (pkg.format && pkg.format !== 'pck') throw new Error(`unsupported source format ${pkg.format}`);
        const pck = await fetchBytes(pkg.url, args.timeout);
        if (pck.length !== pkg.size || sha256(pck) !== pkg.sha256) throw new Error('package does not match the source catalog');
        const engineVersion = pckEngineVersion(pck);
        if (engineVersion !== game.engine.version) throw new Error(`PCK header ${engineVersion} != catalog ${game.engine.version}`);

        let cover = null;
        if (game.cover) {
          try {
            const bytes = await fetchBytes(game.cover, args.timeout);
            const ext = imageExtension(bytes);
            if (!ext) throw new Error('cover is not a supported image');
            cover = { bytes, ext };
          } catch (error) {
            process.stderr.write(`  cover unavailable for ${game.id}: ${error.message}\n`);
          }
        }

        const files = { 'content/game.pck': pck };
        if (cover) files[`assets/cover${cover.ext}`] = cover.bytes;
        files['game.json'] = new TextEncoder().encode(JSON.stringify(gameJSON(game, cover)));
        const container = zipSync(files, { level: 0 });
        const retained = pck.length + (cover?.bytes.length ?? 0);

        let artifact = join(work, `${game.id}.game`);
        let kind = 'game/1';
        if (container.length > BUFFERED_LIMIT || retained > BUFFERED_LIMIT) {
          // The native SaaS validates .game containers in one buffered pass, so an
          // oversized bundle is published as a bare PCK instead of being skipped.
          artifact = join(work, `${game.id}.pck`);
          kind = 'pck';
          writeFileSync(artifact, pck);
        } else {
          writeFileSync(artifact, container);
        }

        // Re-read the target each iteration: a re-run must be idempotent and must
        // not re-upload bytes the instance already serves under the same digest.
        const existing = targetGames(env, args.target, false).get(game.id);
        let record;
        if (existing && existing.package?.sha256 === pkg.sha256) {
          record = { id: game.id, kind: existing.package.format ?? 'pck', containerBytes: 0, pckBytes: pck.length, pckSha256: pkg.sha256, upload: 'already present' };
          process.stdout.write(`already present ${game.id}\n`);
        } else {
          runCLIRetry(['verify', '--id', game.id, '--file', artifact], env);
          const metadata = join(work, `${game.id}.json`);
          writeFileSync(metadata, JSON.stringify({
            title: game.title, subtitle: game.subtitle, description: game.description,
            tags: game.tags, accent: game.accent, engine: game.engine, visibility: 'private',
          }));
          const uploadArgs = ['upload', '--server', args.target, '--id', game.id, '--file', artifact];
          if (kind === 'pck') uploadArgs.push('--metadata', metadata);
          const uploaded = runCLIRetry(uploadArgs, env);
          record = { id: game.id, kind, containerBytes: container.length, pckBytes: pck.length, pckSha256: pkg.sha256, upload: uploaded.trim().slice(0, 400) };
          process.stdout.write(`uploaded ${game.id} (${kind}, ${statSync(artifact).size} bytes)\n`);
        }
        if ((existing?.visibility ?? 'private') !== 'public') {
          runCLIRetry(['visibility', '--server', args.target, '--id', game.id, '--visibility', 'public'], env);
        }
        if (kind === 'pck' && cover) report.barePCK.push(game.id);
        report.copied.push(record);
      } catch (error) {
        report.failed.push({ id: game.id, error: error.message });
        process.stderr.write(`FAILED ${game.id}: ${error.message}\n`);
      }
    }

    // Verification happens before the work directory is removed: the anonymous
    // download writes into it, so cleanup must not run first.
    const verified = [];
    const records = mergeRecords(report.copied, recordsPath);
    if (args.verify) {
      const catalog = targetGames(env, args.target, true);
      for (const record of records) {
        const online = catalog.get(record.id);
        if (!online) { verified.push({ id: record.id, ok: false, reason: 'missing from the anonymous target catalog' }); continue; }
        const inCatalog = online.package?.sha256 === record.pckSha256 && online.package?.size === record.pckBytes;
        if (!inCatalog) { verified.push({ id: record.id, ok: false, reason: 'catalog size/sha256 mismatch' }); continue; }
        // Independent check: download the published bytes without any credential
        // and re-hash them, instead of trusting the upload response.
        const downloaded = verifyAnonymousDownload(env, args.target, record, work);
        verified.push({ id: record.id, ...downloaded });
      }
    } else {
      process.stdout.write('verification skipped (--no-verify); records saved for a later run\n');
    }

    console.log(JSON.stringify({
      source: args.source, target: args.target,
      copied: report.copied.length, failed: report.failed, barePCK: report.barePCK,
      verified, verifiedOK: verified.filter(v => v.ok).length, alreadyPresent: [...targetIDs].length,
    }, null, 2));
    if (report.failed.length || verified.some(v => !v.ok)) process.exitCode = 1;
  } finally {
    if (!args.keep) rmSync(work, { recursive: true, force: true });
    else process.stdout.write(`artifacts kept in ${work}\n`);
  }
}

await main();
