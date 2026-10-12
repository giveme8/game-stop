'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { GameStopSDK } = require('./game-stop.js');
const token = 'ps_' + 'a'.repeat(43);
test('HTTPS and scoped credentials only', () => {
  for (const baseURL of ['http://example.com', 'https://user:pass@example.com', 'https://example.com/x', 'https://example.com/?x=1']) assert.throws(() => new GameStopSDK({ baseURL, gameId: 'demo' }));
  assert.throws(() => new GameStopSDK({ gameId: '../demo' }));
  assert.throws(() => new GameStopSDK({ gameId: 'demo', playToken: 'gs_' + 'a'.repeat(43) }));
});
test('score requests carry only scoped token and preserve retry id', async () => {
  const old = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return { ok: true, status: 200, text: async () => '{"value":42}' }; };
  try {
    const sdk = new GameStopSDK({ gameId: 'demo', playToken: token });
    const submissionId = GameStopSDK.submissionId();
    await sdk.submitScore(42, { submissionId }); await sdk.submitScore(42, { submissionId });
    assert.equal(calls[0].options.body, calls[1].options.body);
    assert.equal(calls[0].options.headers['X-Play-Token'], token);
    assert.equal(calls[0].options.headers.Authorization, undefined);
    assert.equal(calls[0].options.redirect, 'error'); assert.equal(calls[0].options.credentials, 'omit');
    await sdk.getLeaderboard({ board: 'time-ms' });
    assert.equal(calls[2].options.headers['X-Play-Token'], undefined);
    await assert.rejects(sdk.submitScore(NaN)); await assert.rejects(sdk.submitScore(0, { board: 'time-ms' }));
    sdk.dispose(); await assert.rejects(sdk.submitScore(1), { code: 'unauthenticated' });
  } finally { globalThis.fetch = old; }
});
test('HTTP failures, malformed responses and timeout remain actionable', async () => {
  const old = globalThis.fetch;
  try {
    const sdk = new GameStopSDK({ gameId: 'demo', timeoutMs: 10 });
    globalThis.fetch = async () => ({ ok: false, status: 404, text: async () => '{"error":"game not found"}' });
    await assert.rejects(sdk.getLeaderboard(), { status: 404 });
    globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => '<html>' });
    await assert.rejects(sdk.getLeaderboard(), { code: 'invalid_response' });
    globalThis.fetch = (_, options) => new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));
    await assert.rejects(sdk.getLeaderboard(), { code: 'timeout' });
  } finally { globalThis.fetch = old; }
});
test('speech unsupported, completion, cancellation and errors', async () => {
  const oldSpeech = globalThis.speechSynthesis, oldUtterance = globalThis.SpeechSynthesisUtterance;
  try {
    delete globalThis.speechSynthesis;
    const sdk = new GameStopSDK({ gameId: 'demo' });
    await assert.rejects(sdk.speak('你好'), { code: 'unsupported' });
    let spoken;
    globalThis.SpeechSynthesisUtterance = class { constructor(text) { this.text = text; } };
    globalThis.speechSynthesis = { speak: u => { spoken = u; }, cancel() {} };
    const done = sdk.speak('你好'); spoken.onend(); await done;
    const cancelled = sdk.speak('暂停'); sdk.stopSpeaking(); await assert.rejects(cancelled, { code: 'cancelled' });
    const failed = sdk.speak('失败'); spoken.onerror({ error: 'not-allowed' }); await assert.rejects(failed, { code: 'speech_error' });
  } finally { globalThis.speechSynthesis = oldSpeech; globalThis.SpeechSynthesisUtterance = oldUtterance; }
});
