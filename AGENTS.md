# AGENTS.md

## Project

Game Stop is the open-source game-library server, website, publisher CLI and game-integration SDK maintained by giveme8. Go module: `server/`; embedded website: `server/internal/library/`; Godot and browser examples: `sdk/`. The Cloudflare-native SaaS edition lives in `cloudflare/` (TypeScript Worker + D1 + R2) and implements the same HTTP contract; see `docs/adr/0002-cloudflare-native-saas.md`.

## Public/private boundary

- This repository does NOT contain the HarmonyOS, Android or iOS player applications, native runtime/frame, engine binaries, signing materials or their history.
- Client roadmap: phase one HarmonyOS NEXT; Android and iOS are future plans, not released implementations.
- Never merge private-repository history, branches or tags into this repository. Import only reviewed, allowlisted public files.
- Never commit credentials, production configuration, user data, game packages or generated binaries. Public CI has no production deploy credentials.
- Game developer SDKs are public integration code, not the native player client.

## Required reading

- `docs/engineering/constraints.md`
- `docs/domain/game-library/rules.md`
- Relevant API documents under `docs/api/`
- `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md` before changing dependencies or redistributing binaries.

## Checks

```sh
(cd server && go vet ./... && go test -race -count=1 ./...)
node --test server/internal/library/*.test.cjs
python3 -m unittest discover -s deploy -p 'test_*.py' -v
python3 scripts/check-public-boundary.py
(cd cloudflare && npm ci && npm run check && npm test)
wing check --strict
```

Go >= 1.24 and Node.js >= 22 are required for the full server/browser test suite. `TEST_MYSQL_DSN` must name a disposable database; SQLite-only tests are not evidence of MySQL integration. Optional Godot SDK checks require Godot 4. The Worker suite's `platform.test.ts` uses local workerd through Miniflare; it is not evidence about a deployed instance.

## Rules

- Use Wing to scaffold domain documents and ADRs; run `wing sync --indexes` and `wing check --strict` before committing.
- Read nearest instructions before editing; preserve existing security, permission and idempotency behavior.
- Update tests and protocol docs for externally observable changes.
- Do not claim device validation, production deployment or integration coverage without evidence.
- Keep third-party copyrights and license texts intact. Update the dependency notices when the module graph changes.
- Do not run production verification, migrations, provisioning or deployment without explicit operator authorization. Deploying the SaaS (`wrangler deploy`) and applying remote D1 migrations are operator actions; public CI never holds those credentials.
