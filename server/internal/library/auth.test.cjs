'use strict';
// 账号页（/account）的注销流程：真跑 auth.js，喂假 DOM 与假 API。
// 这里覆盖的是"页面到底发不发那个请求、发的是什么、失败时页面说什么"——
// 服务端的鉴权与数据一致性由 Go 测试负责。
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/auth.js', 'utf8');

const PASSWORD = 'a-long-enough-password';
const SESSION = 'ls_' + 'a'.repeat(43);
const SESSION_KEY = 'gamestop.sessionToken';
const ACCOUNT = 'closing-player';

function fakeNode(id) {
  return {
    id,
    value: '',
    textContent: '',
    hidden: false,
    disabled: false,
    className: '',
    dataset: {},
    children: [],
    listeners: {},
    addEventListener(event, fn) { this.listeners[event] = fn; },
    focus() { this.focused = true; },
    replaceChildren() { this.children = []; },
    appendChild(child) { this.children.push(child); },
    closest() { return null; }
  };
}

const IDS = [
  'account-view', 'account-signed-out', 'account-status', 'account-name', 'account-status-text',
  'account-created', 'account-approved', 'account-pending-note', 'account-rejected-note', 'logout',
  'token-form', 'token-ttl', 'token-submit', 'token-status', 'token-secret', 'token-value', 'token-copy',
  'token-copy-status', 'token-dismiss', 'token-list', 'token-list-status',
  'close-reveal', 'close-form', 'close-password', 'close-name', 'close-submit', 'close-cancel', 'close-status'
];

// replies 以 "METHOD /path" 为键；缺省是账号正常、Token 列表为空、注销成功。
function setup(replies = {}, startSignedIn = true) {
  const nodes = Object.fromEntries(IDS.map(id => [id, fakeNode(id)]));
  nodes['account-name'].textContent = ACCOUNT;
  const store = startSignedIn ? { [SESSION_KEY]: SESSION } : {};
  const calls = [];
  const defaults = {
    'GET /api/v1/me': { status: 200, body: { name: ACCOUNT, status: 'active', createdAt: '2026-01-02T03:04:05Z' } },
    'GET /api/v1/me/tokens': { status: 200, body: { tokens: [] } },
    'DELETE /api/v1/me': { status: 204, body: null }
  };
  const responses = Object.assign({}, defaults, replies);
  const context = {
    document: {
      readyState: 'complete',
      getElementById: id => nodes[id] || null,
      createElement: tag => fakeNode(tag)
    },
    window: { addEventListener() {} },
    location: { protocol: 'https:', hostname: 'games.example', replace() {} },
    navigator: {},
    sessionStorage: {
      getItem: key => (key in store ? store[key] : null),
      setItem(key, value) { store[key] = value; },
      removeItem(key) { delete store[key]; }
    },
    AbortController,
    TextEncoder,
    setTimeout: () => 0,
    clearTimeout: () => {},
    fetch: async (url, init) => {
      const method = (init && init.method) || 'GET';
      calls.push({ url, init, method });
      const reply = responses[method + ' ' + url] || { status: 503, body: {} };
      return {
        status: reply.status,
        ok: reply.status >= 200 && reply.status < 300,
        json: async () => reply.body
      };
    }
  };
  vm.runInNewContext(source, context);
  return { nodes, calls, store };
}

// boot() 里的 loadAccount() 是异步的：把微任务队列跑干净再看结果。
const flush = async () => { for (let i = 0; i < 50; i += 1) await Promise.resolve(); };

function submit(nodes, name, password) {
  nodes['close-name'].value = name;
  nodes['close-password'].value = password;
  return nodes['close-form'].listeners.submit({ preventDefault() {} });
}

test('注销要求密码与账号名同时正确，成功后清掉本地会话', async () => {
  const { nodes, calls, store } = setup();
  await flush();
  assert.equal(nodes['account-status'].textContent, '已登录：' + ACCOUNT);
  assert.equal(nodes['close-form'].hidden, true);

  nodes['close-reveal'].listeners.click();
  assert.equal(nodes['close-form'].hidden, false);
  assert.equal(nodes['close-password'].focused, true);

  // 账号名对不上：本地就拦下，一个请求都不发，密码字段立刻清空。
  await submit(nodes, 'someone-else', PASSWORD);
  assert.equal(calls.filter(call => call.method === 'DELETE').length, 0);
  assert.match(nodes['close-status'].textContent, /确认失败/);
  assert.equal(nodes['close-password'].value, '');

  // 两个都对：DELETE /api/v1/me，带登录会话和 JSON 里的密码 + 确认名。
  await submit(nodes, ACCOUNT, PASSWORD);
  await flush();
  const deletes = calls.filter(call => call.method === 'DELETE');
  assert.equal(deletes.length, 1);
  assert.equal(deletes[0].url, '/api/v1/me');
  assert.equal(deletes[0].init.headers.Authorization, 'Bearer ' + SESSION);
  assert.equal(deletes[0].init.credentials, 'omit');
  assert.equal(deletes[0].init.redirect, 'error');
  assert.deepEqual(JSON.parse(deletes[0].init.body), { password: PASSWORD, confirmName: ACCOUNT });

  assert.equal(store[SESSION_KEY], undefined);
  assert.equal(nodes['account-view'].hidden, true);
  assert.equal(nodes['account-signed-out'].hidden, false);
  assert.match(nodes['account-status'].textContent, /账号已注销/);
  assert.equal(nodes['close-form'].hidden, true);
  assert.equal(nodes['close-password'].value, '');
  assert.equal(nodes['close-name'].value, '');
});

test('密码错误时账号保持登录，密码字段不留在页面上', async () => {
  const { nodes, calls, store } = setup({ 'DELETE /api/v1/me': { status: 403, body: { error: 'password does not match' } } });
  await flush();
  nodes['close-reveal'].listeners.click();
  await submit(nodes, ACCOUNT, 'wrong-but-long-enough');
  await flush();
  assert.equal(calls.filter(call => call.method === 'DELETE').length, 1);
  assert.match(nodes['close-status'].textContent, /密码不正确/);
  assert.equal(nodes['close-password'].value, '');
  assert.equal(store[SESSION_KEY], SESSION);
  assert.equal(nodes['close-form'].hidden, false);
  assert.equal(nodes['close-submit'].disabled, false);
});

test('会话已失效时注销返回 401：清掉本地会话并回到未登录', async () => {
  const { nodes, store } = setup({ 'DELETE /api/v1/me': { status: 401, body: { error: 'sign-in required' } } });
  await flush();
  nodes['close-reveal'].listeners.click();
  await submit(nodes, ACCOUNT, PASSWORD);
  await flush();
  assert.equal(store[SESSION_KEY], undefined);
  assert.equal(nodes['account-signed-out'].hidden, false);
  assert.match(nodes['account-status'].textContent, /未登录/);
});

test('没有登录会话时不显示注销入口', async () => {
  const { nodes, calls } = setup({}, false);
  await flush();
  assert.equal(nodes['account-view'].hidden, true);
  assert.match(nodes['account-status'].textContent, /未登录/);
  assert.equal(calls.length, 0);
});
