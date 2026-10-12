"use strict";
(() => {
  const form = document.getElementById("device-form");
  const input = document.getElementById("account-token");
  const button = document.getElementById("authorize-button");
  const status = document.getElementById("device-status");
  const code = document.getElementById("device-code").textContent;
  if (location.protocol !== "https:") {
    input.disabled = button.disabled = true;
    status.textContent = "仅允许在 HTTPS 平台页面输入账号 Token。";
    return;
  }
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!document.getElementById("device-confirm").checked || button.disabled) return;
    let token = input.value.trim();
    input.value = "";
    if (!/^gs_[A-Za-z0-9_-]{43}$/.test(token)) {
      status.textContent = "请输入管理员为您提供的有效账号 Token。";
      token = "";
      return;
    }
    button.disabled = true;
    status.textContent = "正在授权…";
    try {
      const response = await fetch("/api/v1/device-sessions/authorize", {
        method: "POST", mode: "same-origin", credentials: "omit", redirect: "error",
        cache: "no-store", referrerPolicy: "no-referrer",
        headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
        body: JSON.stringify({ userCode: code })
      });
      token = "";
      if (response.ok) {
        form.hidden = true;
        status.textContent = "授权成功，请返回游戏设备。";
      } else {
        status.textContent = response.status === 409 ? "此设备码已被授权。请返回设备或重新配对。" :
          response.status === 404 ? "设备码已失效或游戏不可用，请重新配对。" :
          response.status === 401 ? "账号 Token 无效或已过期，请联系管理员。" : "暂时无法授权，请稍后重试。";
        button.disabled = false;
      }
    } catch (_) {
      status.textContent = "无法确认授权结果。请查看游戏设备；若未连接，请重新配对。";
      button.disabled = false;
    } finally { token = ""; }
  });
})();
