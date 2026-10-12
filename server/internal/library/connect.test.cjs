const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/connect.js', 'utf8');

function setup(protocol = 'https:', responseOK = true) {
  const nodes = Object.fromEntries(['connect', 'authorize', 'account-token', 'consent', 'submit', 'status'].map(id => [id, { value: '', disabled: true, addEventListener(event, fn) { this[event] = fn; } }]));
  nodes.connect.dataset = { gameId: 'example', origin: 'http://192.168.1.10:8080', state: 'a'.repeat(32) };
  nodes.consent.checked = true;
  const calls = [], messages = [];
  const window = { opener: { closed: false, postMessage(...args) { messages.push(args); } }, addEventListener() {} };
  window.self = window; window.top = window;
  vm.runInNewContext(source, { document: { getElementById(id) { return nodes[id]; } }, window, location: { protocol }, AbortController, setTimeout, clearTimeout, fetch: async (url, init) => {
    calls.push({ url, init });
    return { ok: responseOK, json: async () => ({ gameId: 'example', token: 'ps_' + 'b'.repeat(43), expiresAt: new Date(Date.now() + 3600000).toISOString() }) };
  } });
  return { nodes, calls, messages };
}

test('connect forwards only scoped session after explicit consent', async () => {
  const { nodes, calls, messages } = setup();
  const secret = 'gs_' + 'a'.repeat(43);
  nodes['account-token'].value = secret;
  await nodes.authorize.submit({ preventDefault() {} });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, '/api/v1/games/example/play-sessions');
  assert.equal(calls[0].init.headers.Authorization, 'Bearer ' + secret);
  assert.equal(calls[0].init.redirect, 'error');
  assert.equal(calls[0].init.credentials, 'omit');
  assert.equal(nodes['account-token'].value, '');
  assert.equal(messages.length, 1);
  assert.equal(messages[0][1], nodes.connect.dataset.origin);
  assert.deepEqual(Object.keys(messages[0][0]).sort(), ['expiresAt', 'gameId', 'state', 'token', 'type']);
  assert.equal(messages[0][0].type, 'gamestop:session');
  assert.ok(!JSON.stringify(messages).includes(secret));
});

test('connect blocks HTTP and requires consent', async () => {
  const http = setup('http:');
  assert.equal(http.nodes.submit.disabled, true);
  assert.equal(http.nodes.authorize.submit, undefined);
  const { nodes, calls } = setup();
  nodes.consent.checked = false;
  nodes['account-token'].value = 'gs_' + 'a'.repeat(43);
  await nodes.authorize.submit({ preventDefault() {} });
  assert.equal(calls.length, 0);
});

test('connect failures never post credentials', async () => {
  const { nodes, messages } = setup('https:', false);
  nodes['account-token'].value = 'gs_' + 'a'.repeat(43);
  await nodes.authorize.submit({ preventDefault() {} });
  assert.equal(messages.length, 0);
  assert.equal(nodes['account-token'].value, '');
  assert.equal(nodes.submit.disabled, false);
});
