---
title: Game library backend
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - server/**
---

# Game library backend

## 职责

管理账号审核、会话与 Token、游戏所有权、上传校验、公开/私有分发及排行榜；网站和 CLI 共用同一协议。

## 非职责

不实现原生播放器、引擎宿主或反作弊系统；不授予游戏内容或第三方商标使用权。

## 核心对象

| 对象 | 说明 |
| --- | --- |
| User | pending / active / rejected / deleted 账号及审核状态 |
| Token / LoginSession / PlaySession | 分离管理登录、API 与短期游玩权限 |
| Game | 全局稳定 ID、所有者、可见性与不可变包元数据 |
| PlayerScore / ScoreSubmission | 最佳成绩与幂等提交 |

## 上游依赖

MySQL、经过校验的本地存储或可选 OSS，以及受信任的操作员配置。

## 下游影响

网站、CLI、游戏 SDK 和协议兼容的独立客户端。

## 关键规则入口

- [业务规则](rules.md)
- [API](../../api/game-library-api.md)
- [SDK](../../api/platform-sdk.md)
- [开源边界 ADR](../../adr/0001-public-server-client-boundary.md)
