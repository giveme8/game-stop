# Game Stop server and publisher CLI

See the [project README](../README.md) for scope, client roadmap, license and quick start. This directory contains the Go service, embedded website and `gamestop` CLI, not a mobile player.

## Build and test

From this directory, with Go >= 1.24 and Node.js >= 22 available:

```sh
go mod download
go vet ./...
go test -race -count=1 ./...
go build -trimpath -o ../out/server ./cmd/server
go build -trimpath -o ../out/gamestop ./cmd/gamestop
```

Create `../out` first. `MYSQL_DSN` is required to run the service. `DATA_DIR`, `LISTEN_ADDR` and `PUBLIC_BASE_URL` configure storage, listener and public HTTPS origin. Optional OSS settings are described in [deployment](../deploy/README.md).

Tests use isolated SQLite unless `TEST_MYSQL_DSN` names a disposable MySQL database; some MySQL-specific cases skip without it. Browser tests require Node rather than silently skipping. OSS live tests require separate opt-in test configuration, never production credentials in public CI.

## API and uploads

- Register/login/account pages are embedded in the service; registrations require operator approval.
- Upload `.game` with embedded `game.json`, or bare `.pck` with separate metadata. Both undergo server validation.
- API tokens come from a protected `GAMESTOP_TOKEN` environment variable for the CLI; never place them in arguments or logs.
- The CLI defaults to the hosted site; use `--server https://games.example.com` for your own service.
- Download representation follows `package.format`: a `.game` upload can be distributed as `pck` or `game/1` depending on storage configuration.

See [API reference](../docs/api/game-library-api.md), [package format](../docs/api/game-bundle-format.md), [SDK](../docs/api/platform-sdk.md) and [deployment](../deploy/README.md).

## Operator verification

`scripts/verify-production.py` is an operator-only integration tool. It can create accounts, upload/delete test games and issue/revoke tokens against an explicitly authorized instance. It reads protected service configuration and leaves audit tombstones. Do not run it against the hosted site or a production database without separate approval; it is not part of public CI.

Deleted/replaced immutable packages are not immediately garbage-collected. Operators must monitor storage and design backup/retention/offline cleanup. Public-to-private changes cannot recall already downloaded content.
