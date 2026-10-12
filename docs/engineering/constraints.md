---
title: Public server engineering constraints
status: active
owner: giveme8
last_reviewed: 2026-10-12
source_of_truth: true
applies_to:
  - server/**
  - sdk/**
  - deploy/**
  - cloudflare/**
---

# Engineering constraints

## 自建实现（server/）

- Production uses Go >= 1.24, GORM and MySQL. SQLite is isolated test infrastructure, not a production storage option.
- Public source includes website/server, publisher CLI, game integration SDK and generic deployment examples. Native player applications, native frames, engine/signing binaries and their Git history are excluded. See [ADR-0001](../adr/0001-public-server-client-boundary.md).

## Cloudflare 原生 SaaS（cloudflare/）

- The SaaS edition is a separate implementation of the same HTTP contract: TypeScript Worker + D1 + R2. It is **not** a reverse proxy to the Go service and does not share its database. See [ADR-0002](../adr/0002-cloudflare-native-saas.md).
- The workerd isolate budget is 128 MiB, far below the Go service's memory model. Buffered validation of a `.game` container is capped at 32 MiB for the upload **and** for the combined expanded metadata + PCK + images; bare PCKs are streamed into R2 and validated by bounded range reads instead.
- Cloudflare caps one request body by **zone plan** (Free/Pro 100 MB, Business 200 MB, Enterprise configurable). Workers Paid, Containers and streaming do not bypass it; `MAX_PACKAGE_BYTES` must stay under the plan's cap. Raising it for larger games requires a plan change or a different upload protocol, not a config edit.
- Object storage stays in a private R2 bucket. Downloads are proxied through the Worker with per-request visibility checks; never expose the bucket or add a public bucket domain.
- D1 is a single-writer database. Keep authorization, idempotency and one-time-consumption guarantees as atomic conditional SQL inside one batch; do not replace them with read-then-write sequences.
- The Worker's validation must never be weaker than the Go validator: same PCK header/path/MD5 policy, same forbidden executable extensions, same container limits, same engine-version cross-check, fail-closed on unknown structures.
- The password KDF is PBKDF2-HMAC-SHA256 with 600,000 iterations; do not lower it to fit a CPU budget. Expect a paid Workers plan and treat login as a known CPU bottleneck.

## 共同约束

- Credentials must never enter source, logs, URLs or published build artifacts. CLI user tokens use `GAMESTOP_TOKEN`; database, OSS and Cloudflare credentials come from protected environment files, Wrangler OAuth or secret stores. Never disable TLS verification.
- The service may store untrusted uploaded game content; package checks do not create a sandbox. Keep permission checks, path restrictions, byte limits and immutable/atomic storage behavior intact.
- Test MySQL only against a disposable database. Migrations may modify schema; production migration requires backup and explicit operator approval. D1 migrations follow the same rule and must be reviewed before `--remote`.
- Public workflows must not use production credentials or deploy to a live instance. Pull-request code must never run with privileged tokens (`pull_request_target` is not appropriate for untrusted test execution).
- Preserve third-party license texts. Apache-2.0 applies to original project code, not separately licensed dependencies, clients, hosted games or artwork.
- Run the Go, Worker, browser and deployment tests, the boundary check and `wing check --strict` before submitting. Report skipped MySQL/OSS/Godot/workerd tests honestly; host tests do not demonstrate mobile device behavior.
