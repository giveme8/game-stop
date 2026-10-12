import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { renderTemplate } from '../src/template';

// The template engine is hand-written, so its output is pinned here: the home
// page is the only place where a silent rendering regression reaches users.
const home = readFileSync(new URL('../pages/home.html', import.meta.url), 'utf8');

const game = (id: string, extra: Record<string, unknown> = {}) => ({
  ID: id, Title: `游戏 ${id}`, Subtitle: '副标题', Description: '描述',
  Tags: ['标签一', '标签二'], Engine: '4.7', Size: '1.0 MiB',
  DownloadURL: `/api/v1/games/${id}/package.pck`, HasCover: false, Cover: '', ...extra,
});

test('cards with a cover render a lazy image and no fallback decoration', () => {
  const html = renderTemplate(home, {
    Games: [game('covered', { HasCover: true, Cover: 'https://acct.r2.cloudflarestorage.com/b/assets/covered/cover.webp?X-Amz-Signature=ab%2Bcd' })],
    Unavailable: false,
  });
  assert.match(html, /<div class="card-art">/);
  assert.match(html, /loading="lazy"/);
  assert.match(html, /decoding="async"/);
  assert.match(html, /width="768" height="512"/);
  assert.match(html, /alt="游戏 covered 的游戏封面"/);
  // Signed URLs contain & and %, which must survive HTML escaping intact.
  assert.match(html, /src="https:\/\/acct\.r2\.cloudflarestorage\.com\/b\/assets\/covered\/cover\.webp\?X-Amz-Signature=ab%2Bcd"/);
  // The fallback class exists in the stylesheet, so assert on the markup.
  assert.doesNotMatch(html, /class="card-art no-cover"/);
});

test('cards without a cover keep the decorative fallback and no img tag', () => {
  const html = renderTemplate(home, { Games: [game('plain')], Unavailable: false });
  assert.match(html, /<div class="card-art no-cover">/);
  assert.doesNotMatch(html, /<img /);
  assert.match(html, /GODOT \/ PCK/);
});

test('titles and descriptions are escaped, and the count reflects the list', () => {
  const html = renderTemplate(home, {
    Games: [game('xss', { Title: '<script>alert(1)</script>', Description: 'a & b' })],
    Unavailable: false,
  });
  assert.doesNotMatch(html, /<script>alert\(1\)<\/script>/);
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(html, /a &amp; b/);
  assert.match(html, /1 款游戏/);
});

test('an unavailable catalog shows the notice instead of an empty grid', () => {
  const html = renderTemplate(home, { Games: [], Unavailable: true });
  assert.match(html, /游戏库暂时无法加载/);
  assert.doesNotMatch(html, /款游戏/);
});

test('the SaaS connect and CLI upload guides stay on the page', () => {
  const html = renderTemplate(home, { Games: [], Unavailable: false });
  assert.match(html, /id="app"/);
  assert.match(html, /https:\/\/game-stop\.giveme8\.com\//);
  assert.match(html, /设置 → 游戏源/);
  assert.match(html, /id="cli"/);
  assert.match(html, /gamestop upload --server https:\/\/game-stop\.giveme8\.com/);
  assert.match(html, /GAMESTOP_TOKEN/);
  // The documented limits must match the implementation.
  assert.match(html, /32 MiB/);
  assert.match(html, /95 MiB/);
});
