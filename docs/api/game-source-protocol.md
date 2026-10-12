---
title: 游戏源 HTTP 协议 v1（game-source）
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - server/**
---

# 游戏源 HTTP 协议 v1（game-source）

客户端通过一个 base URL 获取清单，再按清单获取封面与游戏包。游戏源可以是公开静态站点，
也可以是支持鉴权与动态清单的 Game Stop 服务端。平台账户、上传与私有内容接口见
[游戏库 API](game-library-api.md)。

本文支持独立客户端实现，不包含原生客户端、运行时/帧、引擎二进制或其源码历史。
客户端一期面向 **HarmonyOS NEXT**，Android / iOS 为未来计划，不代表已发布能力。
范围见 [ADR-0001](../adr/0001-public-server-client-boundary.md)。

## 1. 范围与角色

| 角色 | 职责 |
|---|---|
| 游戏源 | 提供清单、封面和可校验的游戏包；静态源无需实现账号服务 |
| 独立客户端 | 解析清单、展示游戏、下载并校验包、检查运行时兼容性，然后安装/启动 |

清单是唯一入口，客户端不猜包路径。`size` 与 `sha256` 描述实际分发字节，校验失败的下载
不得进入有效安装状态。本协议不规定客户端目录布局、配置存储、界面或原生启动机制。

## 2. 端点与源选择

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `{base}/gamesource/v1/index.json` | 固定清单入口 |
| GET / HEAD | 清单中的 `package.url` | 裸 pck 或 `.game` 容器，由 `package.format` 区分；支持 Range 便于分段下载 |
| GET | 清单中的 `cover` / `screenshots[]` | PNG / JPEG / WebP 素材 |
| GET | `{base}/gamesource/v1/health` | 可选探活；Game Stop 服务端检查数据库就绪状态 |

`{base}` 由集成方显式配置。多个源的选择和持久化属于客户端设计，但源之间的账号凭据、
缓存与安装记录必须隔离；不得在失败时把凭据自动转发到另一个源。切换源后，旧请求不得覆盖
新源的数据。同一个游戏 ID 在不同源中不应被默认视为同一可信内容。

托管实例示例为 `https://game-stop.baobaobao.online`。静态 LAN 源可以用于不带凭据的开发，
不因此自动拥有账号或成绩能力。平台 SDK 地址与信任边界见 [SDK 契约](platform-sdk.md)。

## 3. 清单 schema

```json
{
  "protocol": "gamesource/1",
  "generatedAt": "2026-10-12T13:00:00+08:00",
  "ttlSeconds": 300,
  "sourceName": "示例游戏源",
  "games": [
    {
      "id": "demo",
      "title": "演示游戏",
      "subtitle": "专注力挑战",
      "description": "在规定时间内完成挑战。",
      "tags": ["专注力", "益智"],
      "accent": "#e0243a",
      "cover": "covers/demo.png",
      "engine": { "name": "godot", "version": "4.7" },
      "screenshots": ["screenshots/demo-1.png"],
      "package": {
        "url": "packages/demo.game",
        "size": 213416,
        "sha256": "fdc5ec834a7e50839c21b48837023e76f613d21540c2a41b91b231d74a2933a4",
        "format": "game/1"
      },
      "status": "ready",
      "order": 1
    }
  ]
}
```

| 字段 | 必填 | 约束 / 说明 |
|---|---|---|
| `protocol` | 是 | 固定 `gamesource/1`；不支持的协议版本必须拒绝 |
| `generatedAt` | 否 | 带时区的 ISO8601，仅用于展示/排障 |
| `ttlSeconds` | 否 | 建议缓存时长，默认 300；不替代包校验或访问控制 |
| `sourceName` | 否 | 游戏源展示名 |
| `games[]` | 是 | 数组，可为空 |
| `games[].id` | 是 | `[a-z0-9][a-z0-9-]{0,63}`；稳定标识，内容变化必须更新分发摘要 |
| `games[].title` | 是 | 游戏展示名 |
| `games[].subtitle` / `description` | 否 | 展示说明；客户端可截断 |
| `games[].tags[]` | 否 | 标签；客户端可限制展示数量 |
| `games[].accent` | 否 | `#RRGGBB`；缺省使用客户端兜底色 |
| `games[].cover` | 否 | 封面 URL，按 §4 解析 |
| `games[].coverSha256` | 否 | 提供时校验对应封面内容 |
| `games[].engine.name` | 否 | 当前为 `godot` |
| `games[].engine.version` | 是 | 引擎主/次版本，例如 `4.7`；客户端自行确认兼容运行时 |
| `games[].package.url` | 是 | 分发地址，按 §4 解析；不要用文件扩展名代替格式字段 |
| `games[].package.size` | 是 | 分发字节数，完整下载后核对 |
| `games[].package.sha256` | 是 | 64 位小写十六进制 SHA256，不能留空 |
| `games[].package.format` | 否 | `pck`（缺省）或 `game/1`；未知格式必须拒绝安装 |
| `games[].screenshots[]` | 否 | 截图 URL，最多 8 张 |
| `games[].status` | 否 | `ready`（默认）、`hidden`（不展示）、`deprecated`（可展示但提示） |
| `games[].order` | 否 | 小值在前；同值按 ID 字典序 |

未知可选字段必须忽略，未知协议版本不得猜测解析。`status` 是展示提示，不是权限控制；
私有内容仍须由服务端在每个受保护入口检查权限。

### 上传容器后的实际分发格式

Game Stop 服务端在**上传时配置 OSS** 的情况下会拆分 `.game`，独立分发 pck 与图片，
返回 `package.format: "pck"`，大小和 SHA256 均对应解出的 pck。
**上传时没有配置 OSS** 则保留完整容器，返回 `package.format: "game/1"`，
大小和 SHA256 对应整个容器。裸 pck 上传始终为 `pck`。

`game/1` 并非仅限迁移期，且既有记录可能保留原格式。每条记录都必须按自己的格式处理；
若元数据带 `received`，它只是上传回执，不用于下载校验。完整说明见
[游戏包格式](game-bundle-format.md#1-上传与分发是两回事)。

## 4. URL 解析与凭据

1. `package.url`、`cover`、`screenshots[]` 为 HTTP(S) 绝对 URL 时按给定地址请求，
   但仍应用 §8 的传输策略；可跨域使用 CDN 或对象存储。
2. 相对地址以清单所在目录 `{base}/gamesource/v1/` 为基准解析。拒绝 `..`、反斜杠、
   控制字符等不安全路径；不得把 URL 路径未经检查映射为本地文件路径。
3. URL 不得含 userinfo。生产仅允许 HTTPS，证书校验始终开启。
4. 账号 Bearer 只发送给可信的平台同源端点，**不得转发给跨源包/图片地址或重定向目标**。
   OSS 签名 URL 的访问授权来自签名本身；它是限时凭据，不应记录到公开日志。
5. 签名 URL 过期时重新读取元数据。不要假定旧 URL 永久有效，也不要据此认为安装包已更新；
   内容版本以 `package.sha256` 为准。

## 5. 缓存与条件请求

- 清单响应若提供 `ETag` 或 `Last-Modified`，客户端可保存并使用条件请求；只有服务端返回
  `304` 且本地有对应缓存时才能沿用。未提供条件响应的服务端仍可正常实现本协议。
- `ttlSeconds` 是建议；响应的 `Cache-Control`、权限变化与临时 URL 有效期优先考虑。
  私有元数据与凭据不得进入共享公开缓存。
- 包内容以分发 SHA256 为版本，本地已经校验且摘要一致时可复用，不用仅为 URL 改变而重下。

## 6. 下载、校验与安装

1. 先验证 ID、格式、大小、摘要与引擎版本。独立客户端必须设置下载上限（与服务端一致时为
   **512 MiB**），不把元数据大小当作取消流量限制的理由。
2. 可使用 HEAD 或 Range 检查对象大小；Range 响应应核对 `Content-Range` 的总大小，
   不能误把分段 `Content-Length` 与整个包大小比较。完整下载仍须独立核对字节数与 SHA256。
3. 流式下载到隔离临时文件，计算 SHA256。失败、超限或校验不符时清理临时产物，不覆盖有效安装。
4. 按 `package.format` 检查内容：
   - `pck`：检查 `GDPC` 头、版本、路径与禁止内容，不只检查后缀。
   - `game/1`：先按 [容器契约](game-bundle-format.md) 检查路径、链接、加密、解压体积等，
     再解出声明的 pck，并校验其内容及引擎版本。**不得直接把容器交给 Godot 挂载。**
   - 未知格式：拒绝安装并提示兼容性问题，不尝试兜底执行。
5. 校验全部通过后原子发布安装，记录本次**分发字节**的格式、大小与 SHA256。
   容器大小不能与解出的 pck 大小直接比较来判定更新；本地状态与发布元数据分开管理。

必须拒绝路径穿越、禁止的原生可执行内容及无法检查的加密/未知结构，详细限制见
[游戏包格式](game-bundle-format.md#2-容器与路径约束)。校验不代表内容可信，也不提供运行隔离。

## 7. 版本与兼容

`gamesource/1` 中保持现有字段含义，只通过新增可选字段扩展。改变字段含义或增加必需字段
需要新的协议 major。独立客户端可以自行适配其他旧式清单，但本契约不要求实现私有客户端
的历史兼容逻辑；新源应产出本文格式。

## 8. 安全约束

| 项 | 要求 |
|---|---|
| 生产传输 | HTTPS，始终验证证书；不得静默降级 HTTP |
| 开发 HTTP | 只可由开发者显式允许指定本地主机，不携带账号凭据；不得成为生产默认 |
| 内容 | 包限制不是执行沙箱；不受信任游戏须独立隔离 |
| 路径与 ID | 安全字符集与相对路径检查，不允许包覆盖安装区外文件 |
| 大小与压缩 | 下载、解压体积、条目数与压缩率都要有上限 |
| 隐私与日志 | 协议不要求设备标识；日志不得记录 Token、签名 URL 或敏感用户数据 |
| 跨源隔离 | 不转发平台凭据，不混用不同源的登录状态与游戏记录 |

## 9. 素材约定

封面建议 **3:2 横图**，由展示界面裁切；截图可按游戏比例制作。标题与标签应由客户端以文字渲染，
便于清晰显示和本地化，不要烧进封面。图片应不含个人信息。

## 10. 最小静态源

```text
{base}/gamesource/v1/index.json
{base}/gamesource/v1/covers/demo.png
{base}/gamesource/v1/packages/demo.pck
```

清单中相应使用 `covers/demo.png`、`packages/demo.pck`，填入真实大小/SHA256，
并设置 `package.format: "pck"`。可用任意支持 GET / HEAD / Range 的静态服务器托管；
本地 HTTP 仅用于无凭据开发。需要注册、权限、上传或成绩服务时，部署公开服务端并参照
[游戏库 API](game-library-api.md) 与 [SDK 契约](platform-sdk.md)。
