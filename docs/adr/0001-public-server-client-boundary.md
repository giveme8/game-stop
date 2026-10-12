---
title: ADR-0001：Publish server separately from native clients
status: accepted
date: 2026-10-12
owners:
  - giveme8
supersedes: []
superseded_by: []
tags:
  - distribution
---

# ADR-0001：Publish server separately from native clients

## 背景

Game Stop 原为同时维护网站/服务端与原生播放器的私有混合工程。开源平台代码并不意味着开源客户端；仅从最新提交删除客户端仍会通过历史、分支和标签泄露源码。

## 决策

- 公开仓库使用 `giveme8/game-stop`，从经审核的白名单文件快照开始，不继承私有 Git 对象与历史。
- 保留完整原始工程及生产部署配置于独立私有仓库，不将其分支、标签、旧 release 或 CI 产物复制到公开仓库。
- 公开 Go 服务、嵌入式网站、发布 CLI、游戏接入 SDK、协议文档和通用部署示例，原创代码采用 Apache-2.0。
- 不包含客户端、原生宿主/帧、引擎二进制、签名材料、账号/业务数据或托管游戏。
- 客户端一期支持 HarmonyOS NEXT，未来计划支持 Android / iOS。路线图不是已发布能力；客户端许可证单独管理。
- 公共 CI 仅测试和发布公开 CLI，不持有生产部署凭据。开源不自动部署现有站点。

## 不采用的方案

直接把混合仓库设为 public 不满足客户端保密边界；只删除工作树中的客户端目录也不能清理历史。重写所有历史并强推还会影响协作者和既有部署，并不能保证远端缓存不再访问旧对象。

## 后果

### 正向影响

原生客户端保持私有；平台代码可审计、自建与贡献；依赖许可证和公开范围明确。

### 代价与限制

公开仓库不保留混合工程提交历史。两个仓库的后续同步必须经过源文件范围审查，不能整仓合并。既有生产部署仍需私有运维流程。

### 后续约束

维护文件范围门禁、秘密扫描和第三方声明；发布二进制时随附许可证/NOTICE，以及 MPL 依赖对应源码。新增公开目录或依赖须经过审查。

## 关联内容

- [工程约束](../engineering/constraints.md)
- [业务规则](../domain/game-library/rules.md)
- [SDK 契约](../api/platform-sdk.md)
