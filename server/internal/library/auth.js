"use strict";
/*
 * Registration, sign-in and account pages share this script.
 *
 * It only talks to the platform JSON API on the same origin, keeps the login
 * session in sessionStorage (a per-tab store that dies with the tab, never a
 * persistent browser store, a cookie or a URL) and shows an issued Token
 * plaintext exactly once. Nothing here ever writes a credential to the console,
 * the address bar, the page title or a form target.
 */
(() => {
  const SESSION_KEY = "gamestop.sessionToken";
  const TIMEOUT_MS = 15000;
  const NAME_PATTERN = /^[a-zA-Z0-9_-]{3,32}$/;
  const PASSWORD_MIN = 10;
  const PASSWORD_MAX = 128;
  const TOKEN_TTL_MIN = 1;
  const TOKEN_TTL_MAX = 8760;
  const TOKEN_TTL_DEFAULT = 720;
  const TOKEN_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;
  const LOCAL_HOSTS = ["localhost", "127.0.0.1", "[::1]", "::1"];

  const el = id => document.getElementById(id);
  const setText = (node, text) => { if (node) node.textContent = text === undefined ? "" : text; };
  const setHidden = (node, hidden) => { if (node) node.hidden = !!hidden; };
  const build = (tag, text, className) => {
    const node = document.createElement(tag);
    setText(node, text);
    if (className) node.className = className;
    return node;
  };

  // Storage can be unavailable (private mode, blocked storage); a failure must
  // degrade to "not signed in" instead of breaking the page.
  const readSession = () => { try { return sessionStorage.getItem(SESSION_KEY) || ""; } catch (_) { return ""; } };
  const writeSession = value => { try { sessionStorage.setItem(SESSION_KEY, value); } catch (_) {} };
  const clearSession = () => { try { sessionStorage.removeItem(SESSION_KEY); } catch (_) {} };

  // Passwords and session bearers are only entered on a trustworthy connection.
  // Plain HTTP stays usable for local development hosts only.
  const trustedConnection = () => location.protocol === "https:" || LOCAL_HOSTS.indexOf(location.hostname) !== -1;

  function formatTime(value) {
    const parsed = Date.parse(typeof value === "string" ? value : "");
    if (!Number.isFinite(parsed)) return "时间未知";
    const date = new Date(parsed);
    const pad = number => String(number).padStart(2, "0");
    return date.getUTCFullYear() + "-" + pad(date.getUTCMonth() + 1) + "-" + pad(date.getUTCDate()) + " " +
      pad(date.getUTCHours()) + ":" + pad(date.getUTCMinutes()) + " UTC";
  }

  // The platform measures a password in UTF-8 bytes. Counting characters as
  // well keeps "too short only because of multi-byte text" and "too long in
  // bytes" out of the request, so the page never answers a valid-looking form
  // with a generic 400.
  function utf8Length(value) {
    if (typeof TextEncoder === "function") return new TextEncoder().encode(value).length;
    let bytes = 0;
    for (const character of value) {
      const code = character.codePointAt(0);
      bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
    }
    return bytes;
  }

  // Every request is same-origin, uncached, unredirected and hard-timed out, so
  // an unresponsive platform is reported as a timeout instead of hanging.
  async function api(path, options) {
    const request = options || {};
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
    const headers = { "Content-Type": "application/json" };
    if (request.token) headers.Authorization = "Bearer " + request.token;
    try {
      const response = await fetch(path, {
        method: request.method || "GET",
        headers: headers,
        body: request.body === undefined ? undefined : JSON.stringify(request.body),
        mode: "same-origin",
        credentials: "omit",
        cache: "no-store",
        redirect: "error",
        referrerPolicy: "no-referrer",
        signal: controller.signal
      });
      let payload = null;
      if (response.status !== 204) {
        try { payload = await response.json(); } catch (_) { payload = null; }
      }
      return { status: response.status, ok: response.ok, payload: payload, timeout: false };
    } catch (error) {
      return { status: 0, ok: false, payload: null, timeout: !!error && error.name === "AbortError" };
    } finally {
      clearTimeout(timer);
    }
  }

  // Distinguishes a dead network, a timeout and a platform failure from an
  // application-level rejection, so users always see which one happened.
  function transportFailure(result, fallback) {
    if (result.timeout) return "网络超时：平台在限定时间内没有响应，请稍后重试。";
    if (result.status === 0) return "网络不可用：无法连接平台服务，请检查网络后重试。";
    if (result.status === 503) return "加载失败：平台暂时不可用，请稍后重试。";
    return fallback;
  }

  /* ------------------------------ /register ------------------------------ */
  function initRegister() {
    const form = el("register-form");
    if (!form) return false;
    const name = el("register-name");
    const password = el("register-password");
    const confirmation = el("register-confirm");
    const status = el("register-status");
    const submit = el("register-submit");
    const fail = text => { setText(status, text); if (submit) submit.disabled = false; };
    if (!trustedConnection()) {
      setHidden(form, true);
      fail("不安全连接：请改用 HTTPS 打开注册页，不要在明文连接上输入密码。");
      return true;
    }
    window.addEventListener("pagehide", () => {
      if (password) password.value = "";
      if (confirmation) confirmation.value = "";
    });
    form.addEventListener("submit", async event => {
      event.preventDefault();
      if (submit && submit.disabled) return;
      const accountName = name ? name.value.trim() : "";
      const secret = password ? password.value : "";
      const repeated = confirmation ? confirmation.value : "";
      if (password) password.value = "";
      if (confirmation) confirmation.value = "";
      if (!NAME_PATTERN.test(accountName)) { fail("用户名不合法：需为 3–32 位字母、数字、下划线或连字符。"); return; }
      const characters = Array.from(secret).length;
      if (characters < PASSWORD_MIN || characters > PASSWORD_MAX || utf8Length(secret) > PASSWORD_MAX) {
        fail("密码长度不合法：需为 10–128 个字符，且不超过 128 字节。");
        return;
      }
      if (secret !== repeated) { fail("两次输入的密码不一致，请重新输入。"); return; }
      if (submit) submit.disabled = true;
      setText(status, "正在提交注册…");
      const result = await api("/api/v1/auth/register", { method: "POST", body: { name: accountName, password: secret } });
      if (result.status === 201) {
        setHidden(form, true);
        setText(status, "注册已提交，需管理员审核白名单后才能签发 Token。审核通过后请返回登录页。");
        return;
      }
      if (result.status === 400) { fail("注册信息不合法：请检查用户名与密码后重试。"); return; }
      if (result.status === 409) { fail("该用户名已被占用，请更换用户名后重试。"); return; }
      if (result.status === 429) { fail("提交过于频繁：请稍后再试。"); return; }
      fail(transportFailure(result, "注册失败：平台暂时无法处理，请稍后重试。"));
    });
    return true;
  }

  /* -------------------------------- /login ------------------------------- */
  function initLogin() {
    const form = el("login-form");
    if (!form) return false;
    const name = el("login-name");
    const password = el("login-password");
    const status = el("login-status");
    const submit = el("login-submit");
    const fail = text => { setText(status, text); if (submit) submit.disabled = false; };
    if (!trustedConnection()) {
      setHidden(form, true);
      fail("不安全连接：请改用 HTTPS 打开登录页，不要在明文连接上输入密码。");
      return true;
    }
    window.addEventListener("pagehide", () => { if (password) password.value = ""; });
    form.addEventListener("submit", async event => {
      event.preventDefault();
      if (submit && submit.disabled) return;
      const accountName = name ? name.value.trim() : "";
      const secret = password ? password.value : "";
      if (password) password.value = "";
      if (!NAME_PATTERN.test(accountName) || secret.length === 0) { fail("请输入用户名与密码。"); return; }
      if (submit) submit.disabled = true;
      setText(status, "正在登录…");
      const result = await api("/api/v1/auth/login", { method: "POST", body: { name: accountName, password: secret } });
      if (result.status === 200) {
        const session = result.payload;
        const usable = session && typeof session.sessionToken === "string" && session.sessionToken.length > 0 &&
          session.sessionToken.length <= 512 && typeof session.expiresAt === "string" && Number.isFinite(Date.parse(session.expiresAt));
        if (!usable) { fail("登录失败：平台返回的会话信息无效，请稍后重试。"); return; }
        writeSession(session.sessionToken);
        setText(status, "登录成功，正在进入账号页…");
        location.replace("/account");
        return;
      }
      if (result.status === 401) { fail("凭据错误：用户名或密码不正确（平台不区分两者）。"); return; }
      if (result.status === 403) { fail("账号已被拒绝：无法登录，请联系管理员。"); return; }
      if (result.status === 429) { fail("尝试过于频繁：请稍后再试。"); return; }
      fail(transportFailure(result, "登录失败：平台暂时无法处理，请稍后重试。"));
    });
    return true;
  }

  /* ------------------------------- /account ------------------------------ */
  function initAccount() {
    const view = el("account-view");
    if (!view) return false;
    const signedOut = el("account-signed-out");
    const status = el("account-status");
    const name = el("account-name");
    const stateText = el("account-status-text");
    const created = el("account-created");
    const approved = el("account-approved");
    const pendingNote = el("account-pending-note");
    const rejectedNote = el("account-rejected-note");
    const logout = el("logout");
    const form = el("token-form");
    const ttl = el("token-ttl");
    const issue = el("token-submit");
    const issueStatus = el("token-status");
    const secret = el("token-secret");
    const secretValue = el("token-value");
    const copy = el("token-copy");
    const copyStatus = el("token-copy-status");
    const dismiss = el("token-dismiss");
    const list = el("token-list");
    const listStatus = el("token-list-status");
    const closeReveal = el("close-reveal");
    const closeForm = el("close-form");
    const closePassword = el("close-password");
    const closeName = el("close-name");
    const closeSubmit = el("close-submit");
    const closeCancel = el("close-cancel");
    const closeStatus = el("close-status");

    const hideSecret = () => {
      setText(secretValue, "");
      setHidden(secret, true);
      setText(copyStatus, "");
    };
    // 注销表单里的密码只活在内存里：收起表单或离开页面就立刻抹掉，绝不留下。
    const hideCloseForm = () => {
      if (closePassword) closePassword.value = "";
      if (closeName) closeName.value = "";
      setHidden(closeForm, true);
    };
    const showSignedOut = message => {
      if (list) list.replaceChildren();
      hideSecret();
      setText(listStatus, "");
      setHidden(view, true);
      setHidden(signedOut, false);
      setText(status, message);
    };
    window.addEventListener("pagehide", () => { hideSecret(); hideCloseForm(); });

    if (!trustedConnection()) {
      setHidden(view, true);
      setHidden(signedOut, true);
      setText(status, "不安全连接：请改用 HTTPS 打开账号页，会话凭据只允许在可信连接上使用。");
      return true;
    }

    function renderTokens(tokens) {
      if (!list) return;
      list.replaceChildren();
      if (!Array.isArray(tokens) || tokens.length === 0) {
        list.appendChild(build("li", "还没有签发过 Token。", "state"));
        return;
      }
      tokens.forEach(token => {
        const id = token && typeof token.id === "string" ? token.id : "";
        const item = build("li", undefined, "token-item");
        item.appendChild(build("p", "id：" + (id || "未知"), "token-id"));
        item.appendChild(build("p", "创建时间：" + formatTime(token && token.createdAt) + " · 过期时间：" + formatTime(token && token.expiresAt)));
        const revoked = token && typeof token.revokedAt === "string" && token.revokedAt !== "";
        item.appendChild(build("p", revoked ? "已撤销 " + formatTime(token.revokedAt) : "有效", revoked ? "state revoked" : "state active"));
        if (!revoked && TOKEN_ID_PATTERN.test(id)) {
          const revoke = build("button", "撤销", "ghost");
          revoke.type = "button";
          revoke.dataset.tokenId = id;
          item.appendChild(revoke);
        }
        list.appendChild(item);
      });
    }

    async function loadTokens() {
      setText(listStatus, "正在载入已签发的 Token…");
      const result = await api("/api/v1/me/tokens", { token: readSession() });
      if (result.status === 200 && result.payload && Array.isArray(result.payload.tokens)) {
        renderTokens(result.payload.tokens);
        setText(listStatus, result.payload.tokens.length === 0 ? "还没有签发过 Token。" : "共 " + result.payload.tokens.length + " 个 Token。");
        return true;
      }
      if (result.status === 401) {
        clearSession();
        showSignedOut("未登录：登录会话已过期或已失效，请重新登录。");
        return false;
      }
      setText(listStatus, transportFailure(result, "加载失败：无法获取已签发的 Token 列表，请稍后重试。"));
      return false;
    }

    async function loadAccount() {
      const session = readSession();
      if (!session) { showSignedOut("未登录：当前浏览器没有登录会话，请先登录。"); return; }
      // A session exists: never show the signed-out prompt while it loads.
      setHidden(signedOut, true);
      setHidden(view, true);
      setText(status, "正在加载账号信息…");
      const result = await api("/api/v1/me", { token: session });
      if (result.status === 401) {
        clearSession();
        showSignedOut("未登录：登录会话已过期或已失效，请重新登录。");
        return;
      }
      if (result.status !== 200 || !result.payload) {
        setText(status, transportFailure(result, "加载失败：无法获取账号信息，请稍后重试。"));
        return;
      }
      const account = result.payload;
      const accountState = typeof account.status === "string" ? account.status : "";
      const accountName = typeof account.name === "string" ? account.name : "";
      const canIssue = accountState === "active";
      setHidden(signedOut, true);
      setHidden(view, false);
      // A freshly loaded account always starts with the close form collapsed and
      // empty: a re-login must not inherit half-typed confirmation state.
      setHidden(closeReveal, false);
      hideCloseForm();
      setText(closeStatus, "");
      setText(status, "已登录：" + (accountName || "未知账号"));
      setText(name, accountName);
      setText(created, account.createdAt ? "创建时间：" + formatTime(account.createdAt) : "");
      setText(approved, account.approvedAt ? "审核通过时间：" + formatTime(account.approvedAt) : "");
      if (accountState === "pending") setText(stateText, "等待管理员审核：审核通过前不能签发 Token。");
      else if (accountState === "rejected") setText(stateText, "账号已被拒绝：不能签发 Token，请联系管理员。");
      else if (accountState === "active") setText(stateText, "已通过审核：可以签发 Token。");
      else setText(stateText, "账号状态未知：请联系管理员确认。");
      setHidden(pendingNote, accountState !== "pending");
      setHidden(rejectedNote, accountState !== "rejected");
      if (issue) issue.disabled = !canIssue;
      if (ttl) ttl.disabled = !canIssue;
      setText(issueStatus, canIssue ? "" : accountState === "rejected" ? "账号已被拒绝，无法签发 Token。" : "等待管理员审核，暂时不能签发 Token。");
      await loadTokens();
    }

    if (form) form.addEventListener("submit", async event => {
      event.preventDefault();
      if (issue && issue.disabled) return;
      const hours = Number(ttl ? ttl.value : TOKEN_TTL_DEFAULT);
      if (!Number.isInteger(hours) || hours < TOKEN_TTL_MIN || hours > TOKEN_TTL_MAX) {
        setText(issueStatus, "有效期不合法：请输入 1–8760 小时。");
        return;
      }
      if (issue) issue.disabled = true;
      setText(issueStatus, "正在签发 Token…");
      const result = await api("/api/v1/me/tokens", { method: "POST", token: readSession(), body: { ttlHours: hours } });
      const issued = result.payload;
      const usable = result.status === 201 && issued && typeof issued.token === "string" &&
        issued.token.length > 0 && issued.token.length <= 512;
      if (usable) {
        hideSecret();
        setText(secretValue, issued.token);
        setHidden(secret, false);
        setText(issueStatus, "已签发新 Token：明文只显示一次，请立即保存。");
        if (issue) issue.disabled = false;
        await loadTokens();
        return;
      }
      if (result.status === 401) {
        clearSession();
        showSignedOut("未登录：登录会话已过期或已失效，请重新登录。");
        return;
      }
      setText(issueStatus, transportFailure(result, result.status === 403 ? "账号当前状态不允许签发 Token。" : "签发失败：平台暂时无法处理，请稍后重试。"));
      if (issue) issue.disabled = false;
    });

    if (copy) copy.addEventListener("click", async () => {
      const value = secretValue ? secretValue.textContent : "";
      if (!value) { setText(copyStatus, "没有可复制的 Token：请先签发。"); return; }
      try {
        await navigator.clipboard.writeText(value);
        setText(copyStatus, "已复制到剪贴板。请粘贴到安全位置保存，不要分享。");
      } catch (_) {
        setText(copyStatus, "复制失败：请手动选中上面的 Token 并复制。");
      }
    });
    if (dismiss) dismiss.addEventListener("click", () => {
      hideSecret();
      setText(issueStatus, "已隐藏明文 Token。如果还没有保存，请撤销后重新签发。");
    });

    if (list) list.addEventListener("click", async event => {
      const target = event.target;
      const button = target && target.closest ? target.closest("button[data-token-id]") : null;
      if (!button || button.disabled) return;
      const id = button.dataset.tokenId || "";
      if (!TOKEN_ID_PATTERN.test(id)) return;
      button.disabled = true;
      setText(listStatus, "正在撤销…");
      const result = await api("/api/v1/me/tokens/" + encodeURIComponent(id), { method: "DELETE", token: readSession() });
      if (result.status === 204) {
        await loadTokens();
        setText(listStatus, "已撤销该 Token。");
        return;
      }
      if (result.status === 401) {
        clearSession();
        showSignedOut("未登录：登录会话已过期或已失效，请重新登录。");
        return;
      }
      setText(listStatus, transportFailure(result, "撤销失败：平台暂时无法处理，请稍后重试。"));
      button.disabled = false;
    });

    if (logout) logout.addEventListener("click", async () => {
      const session = readSession();
      logout.disabled = true;
      if (session) await api("/api/v1/auth/logout", { method: "POST", token: session });
      clearSession();
      showSignedOut("未登录：已退出登录。");
      logout.disabled = false;
    });

    if (closeReveal) closeReveal.addEventListener("click", () => {
      setHidden(closeForm, false);
      setHidden(closeReveal, true);
      setText(closeStatus, "");
      if (closePassword) closePassword.focus();
    });
    if (closeCancel) closeCancel.addEventListener("click", () => {
      hideCloseForm();
      setHidden(closeReveal, false);
      setText(closeStatus, "");
    });
    if (closeForm) closeForm.addEventListener("submit", async event => {
      event.preventDefault();
      if (closeSubmit && closeSubmit.disabled) return;
      const typed = closeName ? closeName.value.trim() : "";
      const secret = closePassword ? closePassword.value : "";
      if (closePassword) closePassword.value = "";
      const current = name ? (name.textContent || "").trim() : "";
      if (secret.length === 0 || secret.length > PASSWORD_MAX || utf8Length(secret) > PASSWORD_MAX) {
        setText(closeStatus, "请输入当前密码（10–128 个字符，且不超过 128 字节）。");
        return;
      }
      // Typing the account name is what separates "I meant this" from a stray
      // click; the password is the second, independent confirmation.
      if (!current || typed.toLowerCase() !== current.toLowerCase()) {
        setText(closeStatus, "确认失败：请完整输入你的账号名。");
        return;
      }
      if (closeSubmit) closeSubmit.disabled = true;
      setText(closeStatus, "正在注销账号…");
      const result = await api("/api/v1/me", { method: "DELETE", token: readSession(), body: { password: secret, confirmName: typed } });
      if (result.status === 204) {
        clearSession();
        hideCloseForm();
        showSignedOut("账号已注销：凭据已全部销毁，无法再登录。你名下的游戏与排行榜成绩仍然保留。");
        return;
      }
      if (closeSubmit) closeSubmit.disabled = false;
      if (result.status === 401) {
        clearSession();
        showSignedOut("未登录：登录会话已过期或已失效，请重新登录。");
        return;
      }
      if (result.status === 403) { setText(closeStatus, "密码不正确：注销未执行，账号仍然有效。"); return; }
      if (result.status === 400) { setText(closeStatus, "确认失败：账号名与当前账号不一致。"); return; }
      if (result.status === 429) { setText(closeStatus, "尝试过于频繁：请稍后再试，账号仍然有效。"); return; }
      setText(closeStatus, transportFailure(result, "注销失败：平台暂时无法处理，请稍后重试。"));
    });

    loadAccount();
    return true;
  }

  function boot() {
    if (initRegister()) return;
    if (initLogin()) return;
    initAccount();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();
