# Architecture Decision Records

Each ADR captures one decision that shapes the codebase: the context, the
options weighed, what was chosen, and the consequences we accept. ADRs are
append-only — a decision that is later reversed gets a new ADR that supersedes
the old one (which stays on disk, marked `Superseded`), so the reasoning is
never lost.

ADRs are reserved for the larger, cross-cutting decisions that need a paragraph
of "why" and a record of the options not taken — not every day-to-day
implementation choice.

## Format

```
# NNNN — <short title>

- **Status:** Proposed | Accepted | Superseded by ADR-XXXX
- **Date:** YYYY-MM-DD

## Context        — the forces at play; why a decision is needed now
## Decision       — what we chose, stated plainly
## Consequences   — what follows: the good, the bad, the obligations we take on
## Alternatives   — what else we considered and why we passed
```

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-adopt-agpl-3.0.md) | Adopt AGPL-3.0 for the open-source core | Accepted |
| [0002](0002-open-core-hosted-wrapper.md) | Open-core: a hosted wrapper around an unmodified core, via extension seams | Accepted |
| [0003](0003-importable-app-composition-root.md) | Expose an importable app-composition root (keep everything else internal) | Accepted |
