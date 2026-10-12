# Contributing to Game Stop

欢迎改进网站、服务端、CLI、游戏接入 SDK、文档与测试。

## 范围

原生 HarmonyOS / Android / iOS 播放器不在本仓库。请勿提交客户端源码、引擎二进制、签名资料、游戏素材或私有仓库的历史。首次贡献前请阅读 [AGENTS.md](AGENTS.md) 与 [工程约束](docs/engineering/constraints.md)。

## 本地验证

```sh
(cd server && go vet ./... && go test -race -count=1 ./...)
node --test server/internal/library/*.test.cjs
python3 -m unittest discover -s deploy -p 'test_*.py' -v
python3 scripts/check-public-boundary.py
wing check --strict
```

Go >= 1.24；Node >= 22。MySQL 专属测试需设置 `TEST_MYSQL_DSN` 为专用可销毁数据库。请在 PR 中区分已运行和跳过的测试。不要使用生产凭据、数据或真实游戏作为公开测试样本。

可选 Godot SDK 测试（仓库根目录）：

```sh
godot --headless --path sdk/godot --script ../../tests/platform_sdk_test.gd
```

## 提交

- 说明动机、范围、验证结果和兼容性；修改 API 时同步文档。
- 每个 PR 聚焦一件事，保留安全、权限与幂等语义。
- 新依赖必须记录来源、版本与许可证，更新 `THIRD_PARTY_NOTICES.md` 和许可证文本；MPL 文件修改必须保留对应源码可获取性。
- 仅提交你有权贡献的内容。除明确标注的第三方内容外，贡献以本仓库 Apache-2.0 许可提交。
- 安全报告见 [SECURITY.md](SECURITY.md)，不要公开密钥或利用细节。
