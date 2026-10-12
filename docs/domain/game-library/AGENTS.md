# Game library backend Module Rules

## Scope

This directory contains the Game library backend domain.

## Required Reading

- `README.md`
- `rules.md`


## Rules for Agents

- Do not change domain behavior without updating the relevant domain documentation.
- Do not introduce new states without updating `state-machine.md`.
- Preserve existing audit and idempotency behavior.
- Keep business rules out of controllers and UI components.
- Add tests for normal, boundary, and retry scenarios.

## Review Checklist

- Are permissions affected?
- Are money, inventory, or externally triggered callbacks affected?
- Is retry or duplicate handling required?
- Does this change require an ADR?
