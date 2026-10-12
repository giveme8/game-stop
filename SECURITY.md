# Security policy

## Reporting

Please report vulnerabilities through this repository's GitHub **Security → Advisories → Report a vulnerability** private reporting channel, when enabled. Do not open a public issue with credentials, personal data, unpublished exploits or private-client source.

If the private reporting button is unavailable, open an issue asking maintainers for a private contact channel **without including vulnerability details**. No response-time guarantee is implied.

## Boundaries

- This repository covers the public server, website, CLI and game integration SDK, not the separately distributed native clients.
- Never test destructive actions against the hosted service or other users' accounts. Use your own disposable self-hosted environment.
- Server-side package validation is not a runtime sandbox. Scores are client-reported and are not anti-cheat guarantees.
- Tokens, passwords, production DSNs and private game data do not belong in issues, logs, pull requests or test fixtures.
- Public CI has no production deployment credentials; do not add them or run untrusted pull requests with privileged tokens.
- Review new dependencies, preserve their licenses and run secret/boundary checks before publishing.

Only the current default branch is actively maintained; releases do not imply a long-term support commitment.
