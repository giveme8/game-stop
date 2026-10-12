# Self-hosting Game Stop

These are **reviewable examples**, not an installer for an existing server. The public repository has no production host pins, credentials, signing keys or deploy-enabled workflow. The hosted service's deployment remains separately managed.

## Requirements and isolation

- Linux with systemd, a Go-built Linux server binary, MySQL 8.4, HTTPS reverse proxy, and optional Docker Compose for MySQL.
- A dedicated unprivileged `game-stop` system user, dedicated database account and persistent data directory.
- Keep MySQL and the HTTP listener on loopback/private networks. Example ports: `127.0.0.1:13306` (MySQL) and `127.0.0.1:18080` (API). Check conflicts first; do not stop unrelated services.
- Set DNS and obtain HTTPS certificates for **your own domain**. Nginx and Certbot manage TLS outside Go. Never disable certificate verification.

## Service configuration

Provision `/etc/game-stop/server.env` securely (root-owned mode 0600). The example `game-stop.service` uses systemd's `EnvironmentFile`; do not put secrets into repository files, CLI arguments or logs.

| Variable | Purpose |
| --- | --- |
| `MYSQL_DSN` | Dedicated MySQL account/DB; include `parseTime=true&charset=utf8mb4` |
| `DATA_DIR` | Persistent package/assets directory, e.g. `/var/lib/game-stop` |
| `LISTEN_ADDR` | Internal bind address, e.g. `127.0.0.1:18080` |
| `PUBLIC_BASE_URL` | Your HTTPS origin, e.g. `https://games.example.com` |
| `OSS_BUCKET`, `OSS_ENDPOINT`, `OSS_ACCESS_KEY_ID`, `OSS_ACCESS_KEY_SECRET` | Optional OSS storage; set all required values together or leave all unset |
| `OSS_PREFIX`, `OSS_URL_TTL` | Optional storage prefix and signed-URL lifetime |

With no OSS configuration, packages/assets live on local disk; `.game` containers retain `game/1` representation. With OSS, containers are split into assets and validated PCK, distributed as `pck`. Back up the database **and the configured storage**. Use least-privilege bucket-scoped credentials and rotate them through your secret manager; public CI needs no OSS secrets.

The server checks and migrates schema on startup. Test upgrades against a disposable copy and back up before running a new binary. SQLite is only a testing dependency, not a production backend.

## MySQL Compose example

`compose.yaml` expects operator-created secret files `/etc/game-stop/mysql_password` and `/etc/game-stop/mysql_root_password`. Generate strong unique secrets without echoing them; protect both files. The app account password must match `MYSQL_DSN`.

```sh
# Inspect configuration before starting anything.
docker compose -f deploy/compose.yaml config --quiet
# Run only after provisioning and reviewing host paths/ports/secrets.
docker compose -f deploy/compose.yaml up -d --wait --wait-timeout 180
```

The persistent volume is `game-stop_mysql-data`. **Never run `down -v` against a database you intend to preserve.** The image uses MySQL's own license; it is not re-licensed by this project.

## Binary, systemd and HTTPS

Build Linux binaries on a trusted machine, install to an operator-selected release directory, and point `/opt/game-stop/current` to the chosen release. Ensure the application user can read the binary and write only its data directory. Review and install `game-stop.service`; it references that layout and deliberately restricts write access.

`nginx.conf.example` is an HTTP bootstrap example. Replace the example domain, install into an include path confirmed by `nginx -T`, check with `nginx -t`, then obtain/configure HTTPS with Certbot. Do not install a TLS block referencing certificates that do not exist. Review renewal and test it. Proxy all data routes through Go authentication; never publicly alias the data directory.

Verify both local `/healthz` and your public HTTPS endpoint, then test with disposable accounts/game data. A green health response proves database readiness, not which source commit is running; retain binary hashes and release identifiers.

## Optional restricted deployment receiver

`receive.py` implements a narrow SSH forced command: `deploy <40-character-commit>` with a tar stream containing exactly two regular Linux ELF files, `server` and `gamestop`. It checks archive shape/size, serializes activation and rolls back the symlink if local health fails. Its defaults use the systemd layout above.

If you adopt it, review the code first. Install it root-owned; use a separate deploy identity with a forced command, pinned independently verified SSH host identity, disabled forwarding, and only the minimum restart permission. The deploy identity can replace app code and thereby access application data; it is not a sandbox from that data. Never attach this access to untrusted pull-request workflows. No key provisioning or hosted-service deployment runs from this repository.

The receiver transport intentionally accepts only two binaries. Deliver LICENSE, NOTICE, THIRD_PARTY_NOTICES and dependency licenses/source alongside distributions through a separate trusted operator path; do not add unrecognized archive members to the receiver stream.

## Operations

- Operators may approve users and issue/revoke tokens with `server users ...` and `server tokens ...` using the same protected environment. Token issuance prints a secret once; capture it securely, never into CI logs.
- Monitor disk, database and certificate expiry; keep consistent off-host database/storage backups and test restoration.
- Retain several known-good release binaries. Binary rollback cannot reverse destructive schema changes; never restore a DB automatically just because a health check fails.
- Package deletion/replacement does not immediately reclaim immutable files. Plan offline garbage collection only after backup and retention review.

## Checks

```sh
python3 -m unittest discover -s deploy -p 'test_*.py' -v
docker compose -f deploy/compose.yaml config --quiet
```

Receiver tests mock service restart and HTTP health; they do not prove a real production deployment or TLS setup.
