# Cloudflare 原生 SaaS 实现

`cloudflare/` 是 Game Stop 的 **Cloudflare 原生** 后端：TypeScript Worker + D1（业务数据）+ R2（游戏包与素材）。它与 `server/` 的 Go 实现**共用同一套 HTTP 契约**（`docs/api/`），但**不是**反向代理，也不共享数据库。设计取舍见 [ADR-0002](../docs/adr/0002-cloudflare-native-saas.md)。

生产实例：**https://game-stop.giveme8.com**（Worker `game-stop-saas`，自定义域自动签发证书）。旧的 Go 实例 `https://game-stop.baobaobao.online` 未关停，账号与成绩独立。

## 结构

```text
src/index.ts       路由、中间件、错误与 CORS 边界
src/auth.ts        注册/登录/会话/Token/注销、D1 原子限流
src/platform.ts    游玩会话、成绩幂等提交、排行榜、设备配对
src/library.ts     游戏库 API：列表/详情/上传/下载/素材/公开清单
src/package.ts     `.game` 容器与 PCK 校验（32 MiB 缓冲上限）
src/large-pck.ts   裸 pck 的流式 + range 校验（R2 分片上传）
src/pages.ts       网页渲染（复用 pages/ 的 HTML 与 assets/ 的脚本）
src/template.ts    html/template 子集渲染器
migrations/        D1 schema（`wrangler d1 migrations apply`）
scripts/           公开目录迁移工具
```

## 资源

| 绑定 | 资源 | 说明 |
| --- | --- | --- |
| `DB` | D1 `game-stop-saas-db` | 用户、Token、会话、游戏元数据、成绩、设备配对 |
| `PACKAGES` | R2 `game-stop-saas-packages` | **私有**桶：游戏包与封面/截图 |
| `PUBLIC_BASE_URL` | 变量 | 生成离线清单 URL 用；必须是本实例的 HTTPS origin |
| `MAX_PACKAGE_BYTES` | 变量 | 裸 pck 单请求上限；当前 99,614,720（95 MiB） |
| `R2_ACCOUNT_ID` / `R2_BUCKET` / `R2_URL_TTL` | 变量 | 直传签名 URL 的端点与有效期（当前 300 秒） |
| `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY` | **Secret** | 只用于给对象签名，必须限制在该桶的 Object Read & Write |

## 大文件直传（为什么用签名 URL）

Workers 从 R2 流式回源在实测中**极慢且会中途停住**（同一份代码本地 `wrangler dev` 5 MiB 只要 6 ms，线上经边缘却只有 ~2.5 KB/s；同一对象的签名直传是 1.3 MB/s，差约 500 倍）。所以：

- 清单与 API 里的 `package.url`、`cover`、`screenshots[]` 是**时长 300 秒的 R2 签名 URL**，客户端直接从边缘取对象；这与 Go 实现在 OSS 上的做法一致（`docs/api/game-source-protocol.md` 明确允许绝对 URL 与限时签名）。
- `/api/v1/games/{id}/package.pck`、`/assets/{name}` 仍然保留，作用退化为 **302 跳到签名 URL**（浏览器链接、`curl -L` 仍可用）。`Range`/`HEAD`/条件请求由 R2 自己处理。
- 未配置签名密钥时自动回退成 Worker 自己流式回源（小文件可用，大文件会很慢）。
- 签名 URL 是**限时能力**：不写日志、不进公开缓存。私有游戏的可见性在签名前检查；TTL 到期后客户端重新读元数据。

## 上限（与 Go 实现的差异）

- **`.game` 容器 32 MiB**：容器在内存里整体校验，且解出的元数据 + PCK + 素材合计也必须 ≤ 32 MiB（isolate 128 MiB 预算）。更大的游戏必须发布**裸 pck**。
- **裸 pck 95 MiB**：受 Cloudflare **区域套餐**的单请求体上限约束（Free/Pro 100 MB、Business 200 MB）。Workers 付费计划、容器和流式读取都绕不过它。要再大必须换套餐或另做分片上传协议。
- **下载**：包由 Worker 从私有桶代理读出，支持 `Range`/`HEAD`/`ETag`；不对外暴露桶地址。
- **密码**：PBKDF2-HMAC-SHA256 600,000 次。登录是 CPU 敏感路径，没有付费 Workers 计划时体验会明显受限；不要为了省钱下调迭代次数。

## 常用命令

```sh
npm ci
npm run check        # tsc --noEmit
npm test             # 220+ 用例：auth / platform(D1) / package
npm run dev          # 本地 wrangler dev（本地 D1 + 本地 R2）
npm run dry-run      # 只打包，不部署
```

部署与运维（**需要操作员授权**；公开 CI 不持有这些凭据）：

```sh
npx wrangler d1 migrations apply game-stop-saas-db --remote --env production
npx wrangler secret put R2_ACCESS_KEY_ID --env production       # 只在该桶有 Object Read & Write
npx wrangler secret put R2_SECRET_ACCESS_KEY --env production
npx wrangler deploy --env production
```

`wrangler.jsonc` 里的 `env.production` 绑定上述 D1/R2 与自定义域。首次部署前先建资源：

```sh
npx wrangler d1 create game-stop-saas-db
npx wrangler r2 bucket create game-stop-saas-packages
```

Wrangler 的 OAuth 凭据会过期且无法在非交互环境刷新；`wrangler login` 必须在交互终端执行，或改用 `CLOUDFLARE_API_TOKEN`。

签名是否可用可以在本地只读验证（不需要部署；密钥只从环境变量读，不打印）：

```sh
R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... \
R2_BUCKET=game-stop-saas-packages KEY=games/<id>/<version>-<hash16>.pck \
node scripts/verify-presign.mjs
```

## 账号审核（没有 HTTP 管理入口）

注册一律进入 `pending`，与 Go 实现一致。SaaS 用 D1 命令审核，不需要新增管理接口：

```sh
npx wrangler d1 execute game-stop-saas-db --remote --env production \
  --command "UPDATE users SET status='active', approved_at=datetime('now') WHERE name='alice'"
npx wrangler d1 execute game-stop-saas-db --remote --env production \
  --command "SELECT name,status,created_at FROM users WHERE status='pending'"
```

签发 Token 仍走网页（登录后在 `/account` 自助签发），明文只显示一次。不要在命令行里回显 Token。

## 迁移公开游戏

`scripts/migrate-public-games.mjs` 把**源实例的公开目录**复制到目标实例：只读源站公开清单与匿名可下载的包与封面，逐条核对 `size`/`sha256`，重打成 `.game` 容器后用 CLI 上传并切公开，最后匿名回下载重新哈希验证。**不迁移账号、私有游戏、成绩或排行榜。**

```sh
GAMESTOP_TOKEN=... node scripts/migrate-public-games.mjs \
  --source https://game-stop.baobaobao.online \
  --target https://game-stop.giveme8.com
```

可加 `--only id1,id2`、`--limit N`、`--keep`（保留临时产物）。脚本是幂等的：目标已存在同摘要的包就跳过上传，只补齐可见性。Token 只从环境变量读，不打印、不落盘。
