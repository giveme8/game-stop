---
title: 游戏包格式 .game（bundle v1）
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - server/**
---

# 游戏包格式 `.game`（bundle v1）

`.game` 是标准 ZIP 容器，把游戏元数据、封面/截图和 Godot pck 放在同一个发布产物中。
服务端同时接受 `.game` 与裸 pck；上传接口见 [游戏库 API](game-library-api.md)。
**容器是发布/分发格式，不是运行期资源包；交给引擎的必须是经过校验的 pck。**

本文是公开服务端与独立客户端的互操作契约，不公开任何原生客户端、运行时/帧、引擎二进制或其历史。
客户端一期面向 **HarmonyOS NEXT**，Android / iOS 为未来计划，不代表已发布能力。
公开范围见 [ADR-0001](../adr/0001-public-server-client-boundary.md)。

## 1. 上传与分发是两回事

`PUT /api/v1/games/{id}` 的请求体按魔数识别：`PK\x03\x04` 为 `.game`，`GDPC` 为裸 pck。
容器的元数据唯一真源是包内 `game.json`；同时提供非空 `X-Game-Metadata` 会返回 400。
裸 pck 仍通过该头提供 UTF-8 JSON 的标准 Base64 元数据。

| 上传形态 | 上传时的存储配置 | 对外分发字节 | `package.format` | `package.size` / `package.sha256` |
|---|---|---|---|---|
| `.game` | 配置 OSS | 解出的 pck；图片单独提供 | `pck` | 解出 pck 的大小 / SHA256 |
| `.game` | 未配置 OSS | 原始 `.game` 容器；图片仍单独提供 | `game/1` | 整个容器的大小 / SHA256 |
| 裸 pck | 任一配置 | 裸 pck | `pck` | pck 的大小 / SHA256 |

`game/1` **不是仅在迁移期出现的遗留格式**：未配置 OSS 的部署仍会产生它。既有记录也可能
保持原分发格式，不能根据当前部署配置、上传文件后缀或其他游戏的格式推断本条记录。
客户端必须遵守每条元数据的 `package.format`（省略时为 `pck`）。

可选响应 `received: {size, sha256}` 描述服务端收到的原始上传字节，供发布方核对回执。
它与描述下载字节的 `package.*` 不同；尤其不能用容器的上传 SHA256 校验 OSS 分发的 pck。

## 2. 容器与路径约束

| 项 | 规定 |
|---|---|
| 类型 | 标准 ZIP，扩展名 `.game` |
| 压缩 | deflate 或 store；目录路径使用 `/`，禁止反斜杠 |
| 体积 | 容器 ≤ **512 MiB**；解压后总量 ≤ **768 MiB** |
| 条目 | 最多 **512** 个条目；单条压缩率上限 **200:1** |
| 加密 | 禁止加密条目与无法检查的加密 pck |
| 链接 | 不得使用符号链接或硬链接绕过内容/路径检查 |
| 嵌套容器 | v1 发布包不应嵌套 `.zip` / `.game`；不得递归执行或盲目解压未知内容 |
| 路径 | 只允许安全相对路径；禁止空路径段、`.`、`..`、绝对路径、盘符、`%`、控制字符、尾部空格或点 |
| 可执行内容 | 禁止 `.so`、`.abc`、`.hap`、`.har`、`.hsp`、`.dex`、`.apk`、`.exe`、`.dll`、`.dylib`、`.com`、`.bat`、`.cmd`、`.ps1`、`.msi`、`.wasm`、`.gdextension`，以及文件名中的 `.so.` 变体 |

这些限制同样适用于包内 pck 的路径与内容。允许 GDScript/Godot 数据不等于允许原生可执行分发，
也不表示包内容可信：**包检查不是执行沙箱**，运行不受信任游戏仍须独立设计隔离与权限边界。

## 3. 发布布局

```text
my-game.game
├── game.json              # 必需：元数据
├── assets/
│   ├── cover.png          # 必需：封面
│   └── shot-1.png         # 可选：截图
├── content/
│   └── game.pck           # 可玩内容；入口可由 entry 指定
├── LICENSE               # 可选：游戏本体的许可证
└── README.md             # 可选：说明
```

根目录必须能直接找到 `game.json`，不要再套一层游戏目录。v1 用一个 `entry` 指向可玩 pck；
多产物需求应另行扩展协议，不得改变已有字段含义。游戏本体及其素材的许可证由发布者声明，
不因平台原创代码采用 Apache-2.0 而自动变更。

## 4. 元数据

```json
{
  "id": "demo",
  "title": "演示游戏",
  "subtitle": "专注力挑战",
  "description": "在规定时间内完成挑战。",
  "tags": ["专注力", "益智"],
  "accent": "#e0243a",
  "engine": { "name": "godot", "version": "4.7" },
  "entry": "content/game.pck",
  "source": {
    "repo": "https://example.com/games/demo.git",
    "ref": "v1.0.0",
    "commit": "0123456789abcdef0123456789abcdef01234567",
    "license": "MIT"
  },
  "assets": {
    "cover": "assets/cover.png",
    "screenshots": ["assets/shot-1.png"]
  },
  "order": 1
}
```

| 字段 | 必填 | 约束 / 说明 |
|---|---|---|
| `id` | 是 | `[a-z0-9][a-z0-9-]{0,63}`，必须等于上传 URL 的 `{id}` |
| `title` | 是 | ≤ 512 字节 |
| `subtitle` / `description` | 否 | ≤ 1024 / ≤ 8192 字节 |
| `tags[]` | 否 | ≤ 16 个，每个 ≤ 128 字节 |
| `accent` | 否 | `#RRGGBB` |
| `engine.name` | 是 | `godot` |
| `engine.version` | 是 | `4.x`，必须与包内 pck 头的主/次版本一致 |
| `entry` | 否 | 默认 `content/game.pck`；必须是包内存在的安全路径，扩展名为 `.pck` |
| `source.repo` | 否 | HTTP(S) Git 仓库地址，≤ 512 字节，禁止 userinfo |
| `source.ref` / `source.commit` | 否 | ref ≤ 128 字节；commit 为 40 位十六进制；仅作溯源信息 |
| `source.license` | 否 | SPDX 标识或自由文本，≤ 64 字节 |
| `assets.cover` | 是 | 封面路径，见 §5 |
| `assets.screenshots[]` | 否 | 截图路径，≤ 8 张 |
| `order` | 否 | 排序权重 |

元数据文件最大 64 KiB，未知字段可被忽略。`visibility` 是平台属性，由发布者通过
`gamestop visibility` 或 PATCH 管理；`ownerId`、时间戳、包大小与摘要由平台决定，
不信任包内的同名声明。新游戏默认私有，替换包保留原可见性。

## 5. 素材

- 封面与截图只接受 PNG / JPEG / WebP；按文件头魔数判断，不信任扩展名。
- 封面必需，≤ **4 MiB**，建议 **3:2 横图**。
- 截图可选，≤ **8** 张，每张 ≤ **8 MiB**，建议 16:9 或与游戏同比例。
- 标题和标签由展示界面渲染，不要把它们烧进图片；截图不得泄露个人信息。
- 素材单独提供，查看封面无需下载完整包。响应中的 `cover`、`coverSha256`、`screenshots`
  供客户端使用；服务端生成素材存储名称，不把包内路径当作服务器文件系统路径。

## 6. 服务端上传处理契约

1. 流式接收至临时存储，计算上传大小与 SHA256，超过 512 MiB 拒绝。
2. 解压前检查中央目录、条目数、解压体积、压缩率、路径、加密与符号链接等限制。
3. 解析元数据，核对游戏 ID、字段格式与长度，确定 pck 入口及素材。
4. 校验入口 pck：`GDPC` 头、支持的版本/结构、内容路径、加密与 sparse 等标志、逐项 MD5；
   与声明的引擎版本交叉核对，不一致拒绝。
5. 校验并分离素材；按 §1 的 OSS / 非 OSS 分支选择分发字节，独立计算分发大小与 SHA256。
6. 完成存储后才发布游戏记录，不暴露未完成的下载。服务端使用生成的路径和原子发布机制，
   替换/删除不会立即回收旧不可变包，以免破坏在途下载。

`source.repo` **只用于记录与展示**；服务器不 clone 仓库、不执行构建脚本，不提供任意仓库构建服务。
完整权限与故障行为见 [游戏库 API](game-library-api.md)，安全基线见
[工程约束](../engineering/constraints.md) 与 [游戏库规则](../domain/game-library/rules.md)。

## 7. 独立客户端的下载与安装

1. 从元数据取得 `package.url`、`size`、`sha256`、`format`。只支持 `pck`（默认）与 `game/1`；
   未识别格式必须拒绝并提示兼容性问题，不得猜测安装。
2. 下载到隔离临时文件，限量读取，校验完整分发字节的大小与 SHA256。
3. `pck`：检查资源包结构与 `GDPC` 头；不进行 ZIP 解压。
4. `game/1`：应用 §2 的路径/解压限制，读取元数据与入口，解出 pck 并再次校验内容及引擎版本。
   **绝不能把容器直接交给 Godot 挂载，也不能把“引擎接受了文件”当作包校验。**
5. 全部校验成功后，原子发布到客户端自行管理的安装位置；失败不得替换已有有效安装。
6. 本地安装记录保存分发格式、分发大小、分发 SHA256 及必要的安装状态，独立于发布元数据。
   成功或失败都清理临时文件；不要回写或污染包内元数据。

**更新判别使用分发 SHA256。** `game/1` 的 `package.size` 是容器大小，不是解出的 pck 大小；
二者不能直接比较，更不能因此反复判定有更新。本地记录的分发摘要与远端一致时才可复用。
安装目录布局、状态文件名、进程启动及引擎管理由独立客户端决定，不属于本协议。

下载 URL 可为临时签名 HTTPS 地址。客户端不得向跨源对象存储转发账号 Bearer，也不应记录签名 URL；
失效时重新获取元数据。更多传输规则见 [游戏源协议](game-source-protocol.md)。

## 8. 清单与兼容性

```json
{
  "package": {
    "url": "https://example.com/api/v1/games/demo/package.game",
    "size": 386840,
    "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "format": "game/1"
  },
  "cover": "https://example.com/api/v1/games/demo/assets/cover.png",
  "screenshots": ["https://example.com/api/v1/games/demo/assets/shot-1.png"]
}
```

此例是未配置 OSS 的容器分发形态；OSS 上传分支返回 `format: "pck"`、pck 的大小/摘要及其下载地址。
两种形态都使用 `gamesource/1`，未知可选字段必须忽略。引入新分发格式前必须确认目标客户端支持；
不支持容器的客户端只能安装 `pck` 记录，不能把 ZIP 当作 pck 兜底。

## 9. 发布打包与复现

先按 §3 准备目录，再从该目录压缩，避免额外目录前缀：

```sh
# Info-ZIP：-X 不写 extra field，-D 不写目录条目。
cd staging && zip -X -D -q -r ../dist/demo.game .
gamestop verify --id demo --file ../dist/demo.game
gamestop upload --id demo --file ../dist/demo.game
```

上面的压缩命令只演示布局，不自动保证可复现。可复现构建还应固定条目顺序、时间戳与输入文件字节，
避免 OS 扩展字段；字体构建时间等上游元数据也必须稳定。溯源 commit 应指向本游戏实际使用的源码版本，
避免仅因无关提交而修改包内容。归档工具必须确实产出 ZIP 与 POSIX 路径，不能只依赖 `.game` 后缀猜格式。
发布前用 `gamestop verify` 做本机校验，不需要 Token，也没有服务端副作用。

## 10. 互操作安全验收

独立实现应覆盖以下用例；此清单不代表已进行任何原生设备验证：

- 路径穿越、绝对路径、盘符、百分号编码、反斜杠、控制字符、尾部空格/点。
- 禁止的可执行扩展及 `.so.` 变体、链接条目、未知或加密结构。
- 条目数、解压总量、单条压缩率及下载字节数超限。
- 元数据缺失、非 JSON、超长、ID 与路由不一致、入口指向包外或非 pck。
- pck 内容/版本校验失败，以及素材魔数或体积不合要求。
- OSS 下 `.game` 转为 pck、无 OSS 下保留容器，以及每种分发字节的大小/摘要校验。
- 断流、摘要不符、临时 URL 失效和安装失败时保留已有有效安装、不泄露凭据。

本格式不引入 DRM、签名密钥体系、平台侧构建或多产物运行协议；新增能力须明确扩展契约。
