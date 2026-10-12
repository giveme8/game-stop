---
title: 游戏库服务与 CLI API
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - server/**
---

# 游戏库 API

本文描述公开服务端与发布 CLI 的 HTTP 契约。公开仓库不包含原生客户端、原生运行时/帧、引擎二进制或其源码历史；客户端一期面向 **HarmonyOS NEXT**，Android / iOS 为未来计划，不代表已发布能力。范围见 [ADR-0001](../adr/0001-public-server-client-boundary.md)。

托管实例 origin：`https://game-stop.baobaobao.online`；自建实例使用自己的 HTTPS origin。注册开放但需管理员审核（白名单），审核命令在服务器本机执行，**不提供 HTTP 管理接口**。业务接口凭 `Authorization: Bearer <token>` 鉴权。省略 Token 或提供格式错误、过期、已撤销等不可用凭据时，公开端点按匿名请求处理，只返回公开数据；需要有效凭据的端点仍返回 401，私有内容不因此可见。认证存储故障返回 503，不视为匿名成功。

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | /healthz | 数据库就绪检查 |
| POST | /api/v1/auth/register | 自助注册，账号进入待审核（`pending`）；不签发任何凭据 |
| POST | /api/v1/auth/login | 用户名+密码换 12 小时登录会话（`ls_`） |
| POST | /api/v1/auth/logout | 注销当前登录会话 |
| GET | /api/v1/me | 当前账号信息（名称、状态、审核时间） |
| DELETE | /api/v1/me | 注销自己的账号（销毁全部凭据，不可恢复；需密码 + 账号名确认） |
| POST | /api/v1/me/tokens | 签发自己的 API Token（明文只返回一次） |
| GET | /api/v1/me/tokens | 列出自己的 Token（不含摘要与明文） |
| DELETE | /api/v1/me/tokens/{id} | 撤销自己的 Token |
| GET | /gamesource/v1/index.json | 仅公开 gamesource/1 清单 |
| GET | /api/v1/games | 公开 + 当前用户自己的游戏 |
| GET | /api/v1/games/{id} | 单个可见游戏 |
| PUT | /api/v1/games/{id} | 所有者上传/替换，`.game` 或裸 pck 请求体 |
| PATCH | /api/v1/games/{id} | 所有者局部修改 JSON 元数据、visibility |
| DELETE | /api/v1/games/{id} | 所有者删除 |
| GET/HEAD | /api/v1/games/{id}/package.pck 或 package.game | 平台下载入口，每次检查权限，支持 Range；以响应元数据的 `package.format` 为准 |
| GET/HEAD | /api/v1/games/{id}/assets/{name} | 封面/截图，遵循游戏可见性规则 |

## 账号与凭据

注册（`POST /api/v1/auth/register`，JSON `{name,password}`）只创建账号，状态为 `pending`：
**注册本身不授予任何权限**。账号名 `^[a-zA-Z0-9_-]{3,32}$` 且全局唯一；密码 10–128 字符，
只保存 PBKDF2-HMAC-SHA256 派生值。重名返回 409，参数不合法 400，请求过频 429。

登录（`POST /api/v1/auth/login`）返回 `{name,status,sessionToken,expiresAt}`。用户名不存在与
密码错误返回**完全相同**的 401，不确认账号是否存在；被拒绝的账号返回 403 以便玩家知道结果。
待审核账号可以登录查看状态，但签发 Token 会返回 403。

**登录会话（`ls_`）与 API Token（`gs_`）权限不互串**：会话只能管理本账号自己的 Token，
不能调用需要 API Token 的接口（例如换取游玩凭据）；API Token 不能签发更多 Token。
会话有效期 12 小时，登出即失效。

签发 Token（`POST /api/v1/me/tokens`，JSON `{ttlHours}`，1–8760，默认 720）返回
`{id,token,expiresAt}`，**明文只返回这一次**，服务端只存 SHA256 摘要，请立即保存；
不要写进代码、工单或 Git。

注销账号（`DELETE /api/v1/me`，JSON `{password,confirmName}`）只接受**登录会话**——用 API Token
调用返回 401，免得一个泄露的 `gs_` 就能删掉账号。两项确认都必须成立：`confirmName` 与账号名
不一致返回 400（大小写不敏感），密码错误返回 403，同一来源或同一账号尝试过频返回 429，
成功返回 204 且**无响应体**。成功后该账号的全部 Token、登录会话、游玩会话与设备会话被销毁，
密码派生值被替换为不可用值，账号进入终态 `deleted`：不能登录（与用户名不存在返回完全相同的
401）、不能签发 Token，**不提供任何恢复路径**；`/account` 页面的「注销账号」区是唯一的自助入口。
名下的游戏与排行榜成绩保持不变（它们不是凭据），账号名继续被占用（同名重新注册返回 409）。

网页入口：`/register`、`/login`、`/account`（配套 `/auth.js`）。网页把会话存在
`sessionStorage`，不使用 Cookie，也不把任何凭据放进 URL。注销表单要求重新输入当前密码并原样
输入账号名，密码字段在提交前就被清空，且不随页面离开而保留。

PUT 的 `Content-Type` 为 `application/octet-stream`，请求体按**魔数**判别两种形态：

| 请求体 | 元数据来源 | 响应 `package.format` |
|---|---|---|
| `.game` 容器（zip，`PK\x03\x04`），上传时配置 OSS | **包内 `game.json`**（唯一真源）；非空 `X-Game-Metadata` 会返回 400 | `pck`：分发解出的 pck |
| `.game` 容器，上传时未配置 OSS | 同上 | `game/1`：保留并分发容器 |
| 裸 pck（`GDPC`） | `X-Game-Metadata` 头（UTF-8 JSON 的标准 Base64） | `pck`（是否配置 OSS 均如此） |

容器契约见 [游戏包格式](game-bundle-format.md)：服务端校验容器的路径、扩展名、加密、符号链接与
解压炸弹限制，校验包内 pck，并与 `game.json.engine.version` 交叉核对。封面与截图单独提供，
使用服务端生成的名称，遵循与游戏相同的可见性规则。

**上传格式不等于分发格式。** `package.size` / `package.sha256` 始终描述 `package.url` 返回的
分发字节：OSS 分支为解出的 pck，未配置 OSS 的容器分支为完整 `.game`。可选的
`received: {size, sha256}` 是服务端对上传原始字节的回执，不能替代下载校验信息。
既有记录可能保持原分发格式，客户端必须逐条读取 `package.format`（缺省为 `pck`）；
仅 `game/1` 需要安全解包后挂载 pck，任何情况下都不得把容器当 Godot 资源包。

`package.url` 可指向平台下载入口，也可为 OSS 的限时签名 HTTPS URL。跨源下载不得携带账号
Bearer；签名 URL 也应视为临时访问凭据，不写入公开日志。过期后重新获取元数据，不能硬编码
下载路径或假定所有记录都由同一存储后端提供。

元数据示例（裸 pck 形态的 `X-Game-Metadata`）：

```json
{"title":"演示游戏","engine":{"name":"godot","version":"4.7"},"visibility":"private","tags":["益智"]}
```

支持 subtitle、description、accent 等可选展示字段。ID 为 `[a-z0-9][a-z0-9-]{0,63}`，全局唯一，冲突不得抢占；删除保留所有者墓碑，原所有者可重新上传，其他用户不可占用。新游戏未指定 visibility 为 private；替换保留原可见性。服务端独立计算 `package.size/sha256`，不信任上传方声明的校验信息。元数据响应兼容 gamesource 游戏字段，附 visibility、ownerId、时间。私有游戏对非所有者不披露。

PATCH 仅接受 title、subtitle、description、tags、accent、visibility；engine 随 PUT 完整包校验后更新。删除和替换不会立即物理删除旧不可变包，以保证已获准的在途下载；需监控磁盘并在运维停机备份/保留窗口后回收未引用包，本期不提供自动 GC。

## CLI

从 [server](../../server/) 执行 `go build -o gamestop ./cmd/gamestop`；Token 仅通过 `GAMESTOP_TOKEN` 提供，不存配置、不放命令参数。生产不允许明文 HTTP，不允许关闭 TLS 校验，不跟随重定向。

```sh
gamestop list
gamestop show --id demo
gamestop verify --id demo --file ./demo.game      # 本机自检，不需要 Token
gamestop upload --id demo --file ./demo.game      # .game：元数据在包里
gamestop upload --id demo --file ./demo.pck --metadata ./metadata.json   # 裸 pck（旧形态）
gamestop download --id demo --file ./downloaded-demo.package
gamestop visibility --id demo --visibility public
gamestop delete --id demo
```

`verify` 在**本机**跑一遍服务端会跑的那套校验（容器安全、包内 pck、引擎版本交叉核对），
不需要 Token，也不产生任何服务端副作用——发布前先跑它，别等上传被拒。
`.game` 上传时若同时给了 `--metadata`，本地就会拒绝（那是两个真源）。

所有命令支持 `--server https://other-host`。下载先读取游戏元数据，再请求其 `package.url`；只接受没有 userinfo 的 HTTPS 地址，不跟随重定向。仅当目标 `host[:port]` 与配置的 HTTPS 服务端相同才发送账号 Bearer，跨源（例如 OSS 签名 URL）绝不发送。下载先写同目录临时文件，核对 `package.size` 与 `package.sha256`，再以硬链接原子发布完整结果，目标已存在则失败。要求目标文件系统支持硬链接；不支持时不降级为不安全覆盖。输出文件是服务器声明的分发字节，不会因 `--file` 的扩展名而转换格式；请根据 `package.format` 识别 pck 或容器。

## 管理员（审核与签发）

审核仍是**服务端命令**，不暴露 HTTP 管理端点；账号生命周期与凭据权限见上文「账号与凭据」。

```sh
server users pending                          # 查看待审核注册
server users approve --name alice             # 通过（白名单审核）
server users reject  --name bob               # 拒绝
server users create  --name carol [--password PW] [--status active|pending]
server tokens create --user alice --ttl 720h
server tokens revoke --id TOKEN_ID
```

不带 `--password` 创建的账号不能登录自助签发 Token（既有 Token 仍有效），供脚本化账号使用。

公开切换为私有会阻止新的匿名请求，但无法召回已下载副本。上传包含 GDScript/Godot 数据不等于允许原生可执行文件，包解析器拒绝禁止扩展、穿越路径及无法检查的加密包。

## 管理员

服务端使用 `MYSQL_DSN`、`DATA_DIR`、`LISTEN_ADDR`、`PUBLIC_BASE_URL`；通用部署配置与 OSS 说明见 [部署指南](../../deploy/README.md)。生产配置与凭据不属于公开仓库。

```sh
server users create --name alice
server tokens create --user alice --ttl 720h
server tokens revoke --id TOKEN_ID
```

签发响应包含一次性明文 Token，勿写进日志、工单或 Git。数据库仅存摘要。失窃时撤销再签发；管理员入口应限定 SSH 权限。
