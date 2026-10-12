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
---

# Engineering constraints

- Production uses Go >= 1.24, GORM and MySQL. SQLite is isolated test infrastructure, not a production storage option.
- Public source includes website/server, publisher CLI, game integration SDK and generic deployment examples. Native player applications, native frames, engine/signing binaries and their Git history are excluded. See [ADR-0001](../adr/0001-public-server-client-boundary.md).
- Credentials must never enter source, logs, URLs or published build artifacts. CLI user tokens use `GAMESTOP_TOKEN`; operator database/OSS credentials use protected environment files or secret stores. Never disable TLS verification.
- The service may store untrusted uploaded game content; package checks do not create a sandbox. Keep permission checks, path restrictions, byte limits and immutable/atomic storage behavior intact.
- Test MySQL only against a disposable database. Migrations may modify schema; production migration requires backup and explicit operator approval.
- Public workflows must not use production credentials or deploy to the live site. Pull-request code must never run with privileged production tokens (`pull_request_target` is not appropriate for untrusted test execution).
- Preserve third-party license texts. Apache-2.0 applies to original project code, not separately licensed dependencies, clients, hosted games or artwork.
- Run Go/browser/deployment tests, boundary checks and `wing check --strict` before submitting. Report skipped MySQL/OSS/Godot tests honestly; host tests do not demonstrate mobile device behavior.
