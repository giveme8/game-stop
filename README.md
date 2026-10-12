# Game Stop

**面向 Godot 游戏的开源游戏库平台：网站、服务端、发布 CLI 与游戏接入 SDK。**

[在线站点](https://game-stop.baobaobao.online/) · [API 文档](docs/api/game-library-api.md) · [游戏包格式](docs/api/game-bundle-format.md) · [SDK 接入](docs/api/platform-sdk.md) · [自建部署](deploy/README.md)

Game Stop 帮助游戏作者发布作品，让玩家浏览游戏库，并通过兼容客户端获取游戏。服务端使用 Go + GORM + MySQL，网站随 Go 二进制内嵌，无需单独安装前端依赖或构建前端工程。

## 开源范围与客户端规划

| 部分 | 本仓库是否包含 |
| --- | --- |
| 游戏库 API、账号与权限、游戏包校验和分发 | ✅ 开源 |
| 游戏库网站、注册登录、账号页、排行榜与授权页 | ✅ 开源 |
| `gamestop` 发布管理 CLI | ✅ 开源 |
| Godot / JavaScript 游戏接入 SDK 与示例 | ✅ 开源 |
| 通用自建部署示例、测试、协议文档 | ✅ 开源 |
| 鸿蒙 / Android / iOS 原生播放器客户端 | **❌ 不包含，不在本次开源范围** |
| 原生运行时宿主、引擎二进制、签名材料、线上数据与游戏内容 | **❌ 不包含** |

**客户端一期支持鸿蒙（HarmonyOS NEXT），未来计划支持 Android 和 iOS。** Android / iOS 是后续路线图，不代表已经发布，也不承诺具体时间。客户端的发布、源码和许可证独立管理；本仓库的 Apache-2.0 许可不覆盖客户端。

这里的 SDK 是给游戏作者调用排行榜、成绩提交和设备朗读能力的接入代码，**不是播放器客户端**。网站是游戏库与账号平台，不是 Godot Web 游戏运行站；游戏仍需兼容的运行环境。

本公开仓库从经过范围审核的干净快照开始，**不导入含客户端的私有 Git 历史、分支、标签或旧构建产物**。

## 主要能力

- 多用户游戏库：游戏默认私有，所有者可公开；匿名用户只能读取公开内容。
- `.game` 自描述容器与裸 `.pck` 上传：检查结构、路径、引擎版本、大小与 SHA256，拒绝不支持的可执行载荷。
- `gamesource/1` 清单、包下载、Range / ETag 与下载完整性校验。
- 注册审核、登录会话、限时且可撤销的 API Token；不同凭据类型隔离权限。
- 分游戏、模式与榜单记录最佳成绩，支持幂等重试。**成绩为客户端自报，不是防作弊或奖品结算系统。**
- Godot / 浏览器 SDK：短期游玩凭据、排行榜和设备侧文字朗读。
- 本地磁盘或可选阿里云 OSS 存储；生产数据库为 MySQL，SQLite 仅用于测试。

## 快速开始

### 环境

- Go **1.24 或更新版本**。
- MySQL **8.4**（运行服务和 MySQL 集成测试）。
- Node.js **22 或更新版本**（浏览器脚本测试；运行服务不需要 Node）。
- 可选：Godot 4（SDK 测试）、Python 3（部署接收器测试）、Wing CLI（项目治理校验）。

### 构建与测试

```sh
git clone https://github.com/giveme8/game-stop.git
cd game-stop
mkdir -p out
cd server
go mod download
go vet ./...
go test -race -count=1 ./...
go build -trimpath -o ../out/server ./cmd/server
go build -trimpath -o ../out/gamestop ./cmd/gamestop
cd ..
node --test server/internal/library/*.test.cjs
python3 -m unittest discover -s deploy -p 'test_*.py' -v
python3 scripts/check-public-boundary.py
```

未设置 `TEST_MYSQL_DSN` 时，测试使用隔离 SQLite，并跳过 MySQL 专属集成测试；公共 CI 提供一次性 MySQL 数据库。可选 OSS 实测需专用测试凭据，默认不运行，不要把生产凭据用于 PR。

### 自建服务

1. 为实例准备专用 MySQL 数据库和最小权限账号。
2. 在受保护的本机环境中设置 `MYSQL_DSN`，并配置 `DATA_DIR`、`LISTEN_ADDR`、`PUBLIC_BASE_URL`。**不要把数据库密码或 Token 写进 Git、命令参数、共享终端记录或日志。**
3. 执行 `./out/server serve`；启动会检查数据库并执行迁移，升级前应备份。
4. 在服务前使用 HTTPS 反向代理。访问 `/healthz` 检查数据库就绪；访问 `/` 查看游戏库。
5. 通过网页注册后，由实例管理员在服务主机上执行 `server users approve --name USER` 审核。

配置和运维边界见 [部署手册](deploy/README.md)。示例不是针对现有主机的一键安装器；请先审核再部署。

### 发布游戏

```sh
./out/gamestop list
./out/gamestop verify --id demo --file ./demo.game
# 在安全环境中提供 GAMESTOP_TOKEN 后：
./out/gamestop upload --id demo --file ./demo.game
./out/gamestop visibility --id demo --visibility public
./out/gamestop show --id demo
```

CLI 默认连接在线站点；自建实例使用 `--server https://games.example.com`。上传 `.game` 时元数据来自包内 `game.json`，不要再传 `--metadata`；裸 `.pck` 则配合元数据文件。上传格式与下载格式可能不同，以响应中的 `package.format` 为准。平台源码开源**不意味着线上账号自动获批、游戏素材开源或获得无限托管配额**。

## 目录

```text
server/                   Go API、嵌入式网站、gamestop CLI 与测试
sdk/                      游戏作者接入 SDK / 示例（不是播放器 App）
tests/                    Godot SDK 测试
deploy/                   通用 MySQL / systemd / Nginx / 接收器示例
docs/                     协议、领域规则、开源边界与架构决策
scripts/                  开源范围校验
third_party/licenses/     Go 依赖及运行时版权和许可证文本
.github/workflows/        无生产部署权限的公共 CI 与 CLI 发布
```

## 致谢

感谢以下项目及其维护者和贡献者，让 Game Stop 得以实现：

- [Go](https://go.dev/) 和标准库：服务端、CLI 与网络/加密基础。
- [GORM](https://gorm.io/)、[GORM MySQL Driver](https://github.com/go-gorm/mysql) 与 [Go MySQL Driver](https://github.com/go-sql-driver/mysql)：数据库访问。
- [glebarez/sqlite](https://github.com/glebarez/sqlite)、[modernc SQLite](https://pkg.go.dev/modernc.org/sqlite) 及 [SQLite](https://sqlite.org/)：隔离测试与纯 Go SQLite 支持。
- [Godot Engine](https://godotengine.org/)：游戏创作生态与 GDScript SDK；也感谢 [godot-ohos](https://github.com/godothub/godot-ohos) 社区在鸿蒙适配上的工作。**本仓库不分发这些引擎或原生客户端产物，也不替上游声明许可证。**
- [MySQL](https://www.mysql.com/)、[Nginx](https://nginx.org/)、[Certbot](https://certbot.eff.org/)、[systemd](https://systemd.io/)、[Docker Compose](https://github.com/docker/compose)：自建部署所依赖的生态工具。
- Node.js、GitHub Actions、Wing、Gitleaks 及所有直接/间接依赖的贡献者：测试、协作和发布安全。

准确的依赖版本、许可证与分发说明见 [第三方声明](THIRD_PARTY_NOTICES.md)。致谢不表示上述项目与 Game Stop 存在隶属、合作或背书关系。

## 许可证与贡献

本仓库原创代码采用 **[Apache License 2.0](LICENSE)**；版权归各贡献者所有，见 [NOTICE](NOTICE)。第三方代码与依赖保留其原有许可证；不因本项目开源而被重新授权。线上游戏、封面、商标、用户数据及仓库外的客户端不在此许可范围内。

欢迎提交 Issue 和 Pull Request。请先阅读 [贡献指南](CONTRIBUTING.md)；安全问题请遵循 [安全政策](SECURITY.md)，不要在公开 Issue 中披露密钥或可利用细节。
