---
title: ADR-0002：Cloudflare native SaaS with isolated D1 and R2 storage
status: accepted
date: 2026-10-12
owners:
  - giveme8
supersedes: []
superseded_by: []
tags:
  - deployment
  - storage
---

# ADR-0002：Cloudflare native SaaS with isolated D1 and R2 storage

## 背景

公开仓库里的 Go + MySQL 服务是给自建实例用的，它要求一台常驻 Linux 主机、一个 MySQL 实例和一块持久磁盘。把 SaaS 入口换到 `https://game-stop.giveme8.com` 时，用户明确要求**不采用反向代理**，而是做成 Cloudflare 原生形态，并通过 CLI 部署。

同时有三条现实约束：

1. Workerd 的 isolate 内存预算是 128 MiB，Go 服务的内存模型与 512 MiB 单请求上传契约都不能照搬。
2. Cloudflare 的单请求体上限由**区域套餐**决定（Free/Pro 100 MB、Business 200 MB），Workers 付费计划、容器和流式读取都绕不过它。
3. 原站数据库与私有对象存储没有可用的管理员访问凭据，因此无法导出账号、私有游戏或成绩。

## 决策

- 在 `cloudflare/` 下新增独立实现：TypeScript Worker（`src/index.ts` 路由）、D1 存业务数据、R2（私有桶 `game-stop-saas-packages`）存游戏包与素材。Go 服务**保留**为自建部署路径，两条实现共享同一套 HTTP 契约。
- 数据**不迁移账号**：新站是全新实例，用户重新注册；只按公开清单迁入**已公开**的游戏与素材。原站账号、私有游戏、成绩、排行榜全部留在原站，原站继续运行。
- 上传按形态分两条路，都由服务端独立校验：
  - `.game` 容器：整体读入内存后校验（上限 32 MiB），解出的 pck 与素材分别落 R2，**对外只分发 pck**，`received` 记录容器字节。
  - 裸 pck：流式写入 R2（分片上传），再用 range 读取做 `GDPC` 目录、路径、MD5、引擎版本校验；上限 512 MiB，**不受** 100 MB 单请求限制，因为分片上传的每个请求都很小。CLI 需要改用分片上传协议才能超过单请求上限。
- 所有资源名、绑定与账号隔离：不触碰账号下其它项目的桶/库；不复用 `giveme8.com` 上任何已有 Worker 路由。
- 公开仓库继续不包含生产密钥：D1/R2 通过 Wrangler 的 OAuth 凭据部署，配置里只有资源 ID 与公开 origin。

## 不采用的方案

### 方案 A：Worker 反向代理到现有 Go 源站

最小改动，但用户明确要求原生形态；而且边缘代理会引入三处必须另行修好的问题：`Server.BaseURL` 生成的包/素材/设备 URL 仍是旧 origin、Go 只在回环时信任 `X-Real-IP` 导致按 IP 限流被压到边缘出口、以及 512 MiB 上传仍受 100 MB 限制。它只换入口，不改变归属，也不是多租户 SaaS。

### 方案 B：Cloudflare Containers 承载 Go 二进制

可以保留 Go 代码，但容器磁盘默认是临时的，需要另配可访问的持久 MySQL、R2/FUSE 挂载、快照与成本模型；启动即跑 `Migrate` 的多实例语义、进程内限流和 4 个上传槽位也要重做。在没有原站数据授权的前提下无法完成数据迁移。

### 方案 C：D1 直接替换 MySQL（Go 服务改驱动）

会破坏自建实例的既有部署契约，也让 SQLite 与 MySQL 的并发/迁移语义在同一份代码里分叉。

## 后果

### 正向影响

- SaaS 版可以完全跑在 Cloudflare 边缘上，无需维护主机、MySQL 与证书；冷启动与容量由平台承担。
- D1 提供事务与原子条件写，足以实现幂等提交、一次性设备配对与账号注销的原子性。
- 新站数据与旧站物理隔离，迁移风险被限制在“公开游戏副本”这一范围内。

### 代价与限制

- 两套后端实现要同步维护同一份 HTTP 契约；协议变更必须同时改 Go 与 Worker 并各自测试。
- D1 是单写者模型，PBKDF2 600,000 次的密码校验会消耗可观的 Worker CPU 时间；需要付费计划的 CPU 预算，且高并发登录是已知瓶颈。
- R2 私有桶的对象由 Worker 代理读取，下载流量计入 Workers 请求与出口；`/connect`、`/leaderboards` 等页面与包下载都走 Worker。
- `.game` 容器上传上限 32 MiB；更大的游戏必须发裸 pck。
- 新站没有原站的历史排行榜与账号；原站用户的 Token 在新站无效。

### 后续约束

- 契约变更必须同时更新两套实现；Worker 侧的校验不允许弱于 Go 侧（PK 路径、可执行扩展、解压炸弹、引擎版本交叉核对）。
- 不得把 D1/R2 的 ID、密钥或生产 origin 之外的凭据写进仓库；部署只走 Wrangler OAuth 或 CI 秘密。
- 迁移公开游戏必须逐条核对 `size` 与 `sha256`，并匿名回下载验证；不得用上传响应代替线上核验。
- 变更依赖时必须重新核对许可证与 `THIRD_PARTY_NOTICES.md`。

## 关联内容

- [开源边界 ADR](0001-public-server-client-boundary.md)
- [工程约束](../engineering/constraints.md)
- [游戏包格式](../api/game-bundle-format.md)
- [游戏库 API](../api/game-library-api.md)
