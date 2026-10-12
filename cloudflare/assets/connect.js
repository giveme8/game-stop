(() => {
  'use strict';
  const root = document.getElementById('connect');
  const form = document.getElementById('authorize');
  const tokenInput = document.getElementById('account-token');
  const consent = document.getElementById('consent');
  const button = document.getElementById('submit');
  const status = document.getElementById('status');
  const opener = window.opener;
  if (location.protocol !== 'https:' || window.top !== window.self || !opener) {
    status.textContent = '请从游戏打开受信任 HTTPS 平台授权窗口；此页面不能嵌入其他页面。';
    tokenInput.disabled = true;
    return;
  }
  button.disabled = false;
  status.textContent = '核对来源后输入账号 Token 并确认授权。';
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!consent.checked || button.disabled) return;
    let accountToken = tokenInput.value;
    tokenInput.value = '';
    if (!/^gs_[A-Za-z0-9_-]{43}$/.test(accountToken)) {
      accountToken = '';
      status.textContent = '账号 Token 格式无效。';
      return;
    }
    button.disabled = true;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const responsePromise = fetch('/api/v1/games/' + encodeURIComponent(root.dataset.gameId) + '/play-sessions', {
        method: 'POST', headers: { Authorization: 'Bearer ' + accountToken },
        credentials: 'omit', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer', signal: controller.signal
      });
      accountToken = '';
      const response = await responsePromise;
      if (!response.ok) throw new Error('authorization failed');
      const session = await response.json();
      if (session.gameId !== root.dataset.gameId || !/^ps_[A-Za-z0-9_-]{43}$/.test(session.token) || typeof session.expiresAt !== 'string' || !Number.isFinite(Date.parse(session.expiresAt)) || Date.parse(session.expiresAt) <= Date.now()) throw new Error('invalid response');
      if (opener.closed) throw new Error('game closed');
      opener.postMessage({ type: 'gamestop:session', state: root.dataset.state, token: session.token, gameId: session.gameId, expiresAt: session.expiresAt }, root.dataset.origin);
      status.textContent = '已发送短期游玩凭据，可关闭窗口返回游戏。';
      form.hidden = true;
    } catch {
      status.textContent = '授权失败、账号无权访问、窗口已关闭或网络不可用。请核对后重试。';
      button.disabled = false;
    } finally {
      accountToken = '';
      tokenInput.value = '';
      clearTimeout(timeout);
    }
  });
  window.addEventListener('pagehide', () => { tokenInput.value = ''; });
})();
