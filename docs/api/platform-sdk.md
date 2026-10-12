---
title: 游戏内 SDK 与排行榜接入
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - sdk/**
  - server/internal/library/platform*
  - server/internal/library/game-stop.js
---

# Game Stop SDK v1

## 能力与边界

公开 SDK 是游戏接入代码，不是原生播放器客户端。公开仓库不包含原生客户端、运行时/帧、
引擎二进制或其源码历史；客户端一期面向 **HarmonyOS NEXT**，Android / iOS 为未来计划，
不代表已发布能力。范围见 [ADR-0001](../adr/0001-public-server-client-boundary.md)。

### 平台地址与独立宿主契约

集成方必须明确选择提供账号与成绩服务的**可信 HTTPS origin**，将它与游戏 ID、短期游玩凭据
一起传给 SDK；不得把包的 CDN / OSS 下载地址当作平台 API 地址。静态游戏源不自动拥有账号服务。
同一平台发行的游戏通常使用该平台的 origin，自建实例必须显式配置自己的地址。

| SDK | 平台地址配置 |
|---|---|
| 浏览器 | `new GameStopSDK({baseURL, gameId, playToken})`；省略 `baseURL` 时当前默认值为 `https://game-stop.baobaobao.online`，不是自动读取页面 origin |
| Godot | `configure(base_url, game_id, play_token)`；由集成方显式传入 HTTPS origin |

页面确实托管在平台同源时可以显式传 `location.origin`；跨源游戏应使用经用户/宿主信任的
平台地址。账号 Token 不得交给游戏，也绝不经 HTTP 发送。只有下文的授权流程允许开发期
LAN HTTP 游戏接收短期游玩凭据，并明确警告其被窃取风险。

不同平台的账号、凭据与成绩相互独立。切换平台或游戏时必须清除旧凭据并重新配置，不得把
一个平台的 Token 带到另一个平台。具体启动配置、进程内注入和客户端存储方式由独立宿主
决定，不属于公开 SDK 的自动行为。

- Godot 4 与浏览器：保存最佳成绩、读取排行榜、设备文字朗读。
- `score`：非负整数，越高越好；`time-ms`：正整数毫秒，越低越好。最大值 9007199254740991。
- 每账号/游戏/模式/榜保留最佳值，同分同名次（1、1、3），先达成先展示。显示账号名称；不要用敏感个人信息作为账号名称。
- 成绩是客户端自报，不是防作弊或奖金结算依据。
- 玩家在平台网页自助注册（需管理员审核）并自助签发 Token，详见 [账号与凭据](game-library-api.md#账号与凭据)。受信任集成方换取短期游玩凭据后交给游戏；SDK 不自动绑定账号或注入身份，已有游戏也不会自动上报。
- 朗读不走云服务：浏览器由 SpeechSynthesis 提供，Godot 由 DisplayServer 提供；设备可能缺少语音能力/语言包。浏览器首次调用放在用户点击事件中。

## 受信任的平台端创建游玩会话

`POST /api/v1/games/{id}/play-sessions`，使用现有 `Authorization: Bearer <account-token>`，返回 `{gameId,token,expiresAt}`。

账号需可读取该游戏。会话有效期一小时，绑定该账号、游戏和来源 API Token（`gs_`）；登录会话（`ls_`）不能用于此接口。来源 Token 撤销/过期后不可继续提交。会话过期时重新授权，并用有效的新会话替换 SDK 内存里的凭据。

**不要把账号 Token 写入游戏包、网页脚本、URL、日志或 localStorage。** 会话创建不开放跨源管理 CORS；应由受信任宿主/平台后端代理执行，随后把短期 `ps_...` 凭据交给当前游戏。SDK 不提供持有管理员凭据的捷径。

游玩凭据不是强沙箱；不受信任游戏可能窃取同进程内存。它只能提交绑定玩家在绑定游戏里的成绩，无上传/删除游戏权限。退出或换游戏时清除 SDK 引用/凭据并停止朗读。

## HTTP 契约

| 方法 | 路径 | 鉴权与响应 |
|---|---|---|
| POST | `/api/v1/games/{id}/play-sessions` | 账号 Bearer；返回短期凭据 |
| POST | `/api/v1/games/{id}/scores` | `X-Play-Token`；JSON `{board,mode,value,submissionId}`，返回当前最佳 `{gameId,board,mode,value,achievedAt}` |
| GET | `/api/v1/games/{id}/leaderboard?board=score&mode=default&limit=20` | 公开匿名；私有仅现有所有者 Bearer；返回 `{gameId,board,mode,entries:[{rank,playerName,value,achievedAt}]}` |
| GET | `/leaderboards/{id}?board=score&mode=default` | 仅公开网页榜，最多 100 条，只列 default 与合法已存模式；不同模式绝不混排 |
| GET | `/sdk/v1/game-stop.js` | 无依赖浏览器 SDK |

`board` 为 `score` 或 `time-ms`，limit 1..100。submissionId 为 16..64 位字母/数字/下划线/短横线，SDK 默认生成 32 位安全随机十六进制串。**一次逻辑成绩只生成一个 ID，网络超时重试必须复用它。** 同 ID 不同内容 409；同内容重试返回原提交时的最佳值快照，最新最佳值请重新读榜。400 输入非法、401 凭据缺失/过期、404 不可见游戏、503 服务不可用；错误为 `{error:...}`。公开读榜与 scoped-token 提交允许 CORS，不使用 Cookie、不开放跨源管理 Bearer。

HTTP 的 `mode` 省略或空字符串时为 `default`，其他值必须匹配 `^[a-z0-9][a-z0-9-]{0,63}$`。旧成绩保持 default。幂等键仍为 game+player+submissionId（不是每个 mode 独立），相同 ID 改 mode/board/value 返回 409。PlaySession 不绑定 mode，仍只绑定玩家/游戏。

**SDK 支持范围不同：** 浏览器 SDK 的 `submitScore` / `getLeaderboard` 接受 `mode` 选项（缺省 `default`，显式空串会被 SDK 拒绝）；当前公开 Godot SDK 的 `submit_score(value, board, submission_id)` 与 `get_leaderboard(board, limit)` **没有 mode 参数**，请求省略 mode，只操作 `default`。需要 Godot 多模式榜时，应另行实现符合上述 HTTP 契约的请求层，不要假定现有方法已支持。

## HTTPS 平台弹窗授权

游戏在用户点击后打开 `https://game-stop.baobaobao.online/connect?gameId=...&mode=...&origin=...&state=...`。origin 使用 `location.origin` 精确序列化并 URL 编码；state 使用密码学随机 16 字节、32 位小写十六进制。禁止把账号 Token 或游玩凭据放在 URL。页面拒绝未知/重复参数、非法 ID/mode/state/origin；state 必填。origin 只允许 HTTPS，或 localhost、私有 IPv4、127/8 回环的 HTTP；拒绝 userinfo/path/query/fragment、非标准 IP 拼写、显式默认端口等非精确来源。

平台页显示游戏、模式、精确来源，用户手动输入自己的现有账号 Token 并勾选授权。LAN HTTP 明确警告：游戏页面明文可被局域网攻击者篡改，短期游玩凭据可被窃取，绝不能把账号 Token 交给该游戏。账号 Token 只用于同源 fetch POST play-sessions 的 Authorization Bearer，不写日志、URL、localStorage、Cookie或游戏；输入框发送后清空。页面只在 HTTPS 顶层且有 opener 时启用。

成功只向精确 origin 调用 `window.opener.postMessage({type:'gamestop:session',state,token,gameId,expiresAt}, origin)`，token 为 ps_ 短期凭据，不含账号 Token。游戏必须同时检查 `event.origin` 为可信平台 HTTPS origin、`event.source===popup`、state 和 gameId 匹配当前请求。授权页 CSP 为 self script/connect、frame-ancestors none；独立本地 connect.js，no-store/no-referrer、X-Frame-Options DENY。COOP unsafe-none 保留 opener；源站游戏应配置 same-origin-allow-popups（nothreads），不能用 same-origin 隔断 opener。中途关闭或失败不授予匿名身份；超时重新点击授权。

## mode 数据迁移与回滚

启动 Migrate 前停止所有旧/新服务写入并备份数据库。迁移先在两个成绩表新增非空 mode default 'default'，回填旧空值，再用单条 MySQL `ALTER TABLE player_scores DROP PRIMARY KEY, ADD PRIMARY KEY (game_id,user_id,board,mode)` 原子替换主键；不分别删除主键，不删除记录。MySQL DDL 隐式提交，步骤可重复执行以恢复中断。ScoreSubmission 主键不变，仅增加 mode。SQLite 测试走事务建表复制，不能代替 MySQL 实机验证。完成后核对旧行数和 default 成绩及多模式同玩家写入。回滚不得直接删除 mode（会使多模式键冲突）；先停止写入，保留完整备份，再恢复迁移前数据库与服务，新增成绩需另行归档。

## 浏览器游戏

```html
<script src="https://game-stop.baobaobao.online/sdk/v1/game-stop.js"></script>
```

```js
// playToken 来自受信任宿主，不是账号 Token。
const sdk = new GameStopSDK({
  baseURL: 'https://game-stop.baobaobao.online', // 自建实例改为自己的可信 HTTPS origin
  gameId: 'demo',
  playToken
});
const submissionId = GameStopSDK.submissionId();
try {
  await sdk.submitScore(120, { submissionId });
  const leaderboard = await sdk.getLeaderboard({ board: 'score', limit: 20 });
} catch (error) {
  // error.code / error.status；超时后复用 submissionId 重试，切勿无限自动重试。
}
// 应在用户点击后调用；不支持、取消、未授权都会 reject。
await sdk.speak('恭喜，你得到了120分！', { language: 'zh-CN' });
sdk.stopSpeaking();
sdk.dispose();
```

浏览器 SDK 的 getLeaderboard 仅走公开榜，不携带管理凭据。`setPlayToken()` 可更新过期会话；`dispose()` 清除凭据、停止朗读。SpeechSynthesis 是页面全局设备，stop/interrupt 也可能影响其他组件的朗读，应由平台统一管理。

## Godot 游戏

把 [Godot SDK](../../sdk/godot/game_stop.gd) 复制进游戏工程，参照 [接入示例](../../sdk/godot/example.gd)。从游戏场景代码通过 `preload(...).new()` 实例化，添加子节点后调用 `configure()`；网络请求要求节点已经进入场景树。独立宿主应保证该初始化代码被执行，不依赖未约定的自动加载或身份注入。

开启 `audio/general/text_to_speech` 并确保当前运行的工程、引擎与设备有对应语言的 voice。无可用 voice 时 SDK 明确返回 unsupported；公开 SDK 不承诺任何特定移动平台已提供语音能力，必须在实际目标环境验证。

Godot 示例不包含凭据，不读取包内硬编码秘密；集成方显式传入 HTTPS origin、gameId 和当前游玩 token。`configure()` 仅在内存保存配置，重新配置时可替换过期 token；无凭据时仍可读公开榜，但不能提交成绩。网络方法返回 `{ok, status, data, error}`，`status=0` 表示本地或传输失败，调用者负责复用同一 submission ID 重试。

`speak()` 返回 true 仅表示已交给可用 voice，不能证明实际发声或完成；`tts_failed(reason)` 只报告参数/能力检查的同步失败。为避免覆盖进程全局 DisplayServer callbacks，首版不捕获异步朗读错误或完成事件。`stop_speaking()` 也是进程全局停止。退出或换游戏时停止朗读，清除/释放包含配置的 SDK 实例。

## 验证与上线

浏览器/服务端检查：从仓库根目录运行 `node --test server/internal/library/game-stop.test.cjs`，从 [server](../../server/) 目录运行 `go test ./...`。已安装 Godot 4 时，可从仓库根目录运行可选的 `godot --headless --path sdk/godot --script ../../tests/platform_sdk_test.gd`，执行公开的 [SDK 测试](../../tests/platform_sdk_test.gd)。本文档整理未在本地执行该可选检查。

SQLite 隔离测试不能代替 MySQL 并发/迁移验收；Godot headless 与浏览器测试也不证明原生设备行为。服务端升级需按授权的部署流程执行迁移、构建与验收；公开 SDK 不自动发布生产、不自动更新游戏或原生客户端。包上传与下载格式见 [游戏包格式](game-bundle-format.md)，不能把 SDK 集成误认为原生客户端源码公开。
