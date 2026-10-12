# Third-party notices and acknowledgments

Thank you to all upstream maintainers and contributors. Original Game Stop server, website, CLI and game-integration SDK code is Apache-2.0; **dependencies keep their own licenses**. Native player clients and engine binaries are not included. Hosted games, artwork and user data are not licensed by this repository.

This inventory describes the versions declared in `server/go.mod` and verified with `go list -deps` for the server/CLI. Version changes require rechecking the graph and notices. The full module license files (including discovered nested license files) are preserved under `third_party/licenses/<module>@<version>/`; source-file notices in upstream source also remain applicable.

## Server and CLI runtime modules

The CLI uses the shared server library, so it is **not** a standard-library-only executable. Both binary graphs include:

| Module | Version | License | Upstream |
| --- | --- | --- | --- |
| `gorm.io/gorm` | v1.25.12 | MIT | <https://github.com/go-gorm/gorm> |
| `gorm.io/driver/mysql` | v1.5.7 | MIT | <https://github.com/go-gorm/mysql> |
| `github.com/go-sql-driver/mysql` | v1.7.0 | **MPL-2.0** | <https://github.com/go-sql-driver/mysql> |
| `github.com/jinzhu/inflection` | v1.0.0 | MIT | <https://github.com/jinzhu/inflection> |
| `github.com/jinzhu/now` | v1.1.5 | MIT | <https://github.com/jinzhu/now> |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause | <https://github.com/golang/text> |

### MPL source availability

The MySQL Go driver is used **unmodified** at `v1.7.0`. Its source files remain under MPL-2.0; Game Stop is a larger work containing that component, not a relicensing of it. Corresponding upstream source:

- <https://github.com/go-sql-driver/mysql/tree/v1.7.0>
- <https://proxy.golang.org/github.com/go-sql-driver/mysql/@v/v1.7.0.zip>

Our CLI release archives include that exact Go-module source ZIP in `third_party/source/`, the full MPL text, and this source-availability notice. If you modify those MPL-covered files or redistribute binaries, supply the corresponding modified source under MPL-2.0 and preserve applicable notices. Merely thanking the project is not a substitute for these obligations.

## Test-only Go modules

SQLite is imported by the project's tests, not the normal server/CLI runtime graph. Direct requirement does not necessarily mean runtime linkage.

| Module | Version | Top-level license | Upstream |
| --- | --- | --- | --- |
| `github.com/glebarez/sqlite` | v1.11.0 | MIT | <https://github.com/glebarez/sqlite> |
| `github.com/glebarez/go-sqlite` | v1.21.2 | BSD-3-Clause | <https://github.com/glebarez/go-sqlite> |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT | <https://github.com/dustin/go-humanize> |
| `github.com/google/uuid` | v1.3.0 | BSD-3-Clause | <https://github.com/google/uuid> |
| `github.com/mattn/go-isatty` | v0.0.17 | MIT | <https://github.com/mattn/go-isatty> |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | <https://github.com/remyoudompheng/bigfft> |
| `golang.org/x/sys` | v0.7.0 | BSD-3-Clause | <https://github.com/golang/sys> |
| `modernc.org/libc` | v1.22.5 | BSD-3-Clause + nested notices | <https://pkg.go.dev/modernc.org/libc@v1.22.5?tab=licenses> |
| `modernc.org/mathutil` | v1.5.0 | BSD-3-Clause + nested notices | <https://pkg.go.dev/modernc.org/mathutil@v1.5.0?tab=licenses> |
| `modernc.org/memory` | v1.5.0 | BSD-3-Clause + nested notices | <https://pkg.go.dev/modernc.org/memory@v1.5.0?tab=licenses> |
| `modernc.org/sqlite` | v1.23.1 | BSD-3-Clause; SQLite notices separately preserved | <https://pkg.go.dev/modernc.org/sqlite@v1.23.1?tab=licenses> |

SQLite itself is public domain; its Go wrappers/translations must **not** therefore all be called public domain. The table gives top-level licenses, not a claim that every embedded file uses the same license. Nested license texts are retained. If you redistribute upstream test/source trees, retain their embedded notices too (for example, the Jim Kalafut MIT benchmark notice in `glebarez/go-sqlite` tests).

The broader module metadata/checksum graph may include upstream tests and build tools not compiled into these binaries or the project's tests. For example, `github.com/google/pprof` at `v0.0.0-20221118152302-e6195bd50e26` is an Apache-2.0 upstream-test dependency, not a normal application runtime component. Historical `go.sum` entries are not a list of shipped binary dependencies.

## Go runtime and browser/SDK code

Go is BSD-3-Clause; the module minimum is Go 1.24. Release bundles record the actual build toolchain and include its LICENSE/PATENTS and available standard-library/vendor license notices. The checked-in `third_party/licenses/go/` text is the Go 1.24.0 reference license; release packaging collects the actual toolchain texts separately.

The website and JavaScript SDK use first-party HTML/CSS/JavaScript and browser APIs; there is no React, CSS framework, downloaded font, icon package or CDN runtime dependency in the reviewed source. Font-family names are local system fallbacks, not bundled fonts. The Godot SDK is first-party GDScript using Godot built-in APIs. No engine binary is bundled. OSS support uses the Go standard library rather than an Alibaba SDK package.

## Ecosystem and infrastructure thanks (not linked application modules)

- **Godot Engine** and the **godot-ohos** community: game-authoring and HarmonyOS adaptation work. Official Godot is MIT; each port's own artifacts and included components require their own license review. No port/client binaries are distributed here.
- **MySQL 8.4**: separately running database, GPLv2 with upstream additional terms and third-party notices. Referencing a container recipe does not bundle or relicense MySQL; redistributing that image is a separate compliance task.
- **Nginx** (BSD-2-Clause), **Certbot** (Apache-2.0 plus component-specific notices), **Docker Compose** (Apache-2.0), **systemd**, **OpenSSH** and the Linux ecosystem: operator-managed deployment tools, not embedded in these Go binaries. Verify licenses for the exact versions you deploy/distribute.
- **Node.js**, **Python**, **GitHub Actions**, **Wing** and **Gitleaks**: development, test and release tooling. Node's built-in test runner is used; no npm test framework is bundled.
- The pinned `actions/checkout`, `actions/setup-go`, `actions/setup-node`, `actions/upload-artifact` and `actions/download-artifact` actions are MIT-licensed upstream tools; their code is fetched by GitHub Actions, not vendored into Game Stop.

These acknowledgments imply no endorsement, affiliation or partnership.

## Redistribution checklist

1. Include Game Stop's LICENSE and NOTICE, this document, and applicable third-party copyright/license texts with source and binaries.
2. Preserve MIT/BSD permission, conditions and disclaimers; SPDX identifiers or links alone are insufficient.
3. Preserve MPL notices and provide corresponding driver source, including your modifications if any.
4. Record the real Go compiler and binary dependency versions (`go version -m`), and include runtime/vendor notices from that toolchain.
5. Do not assume client software, hosted games, covers, trademarks or user data are covered by Game Stop's license.
6. The restricted deployment receiver accepts only two ELF files; deliver legal/source materials separately through the operator's trusted distribution path rather than changing that transport implicitly.
