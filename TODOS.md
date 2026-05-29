# TODOs

Items that don't belong to any single step's plan in `docs/plan.md`. Each step's "Decisions for this step" subsection in plan.md is authoritative for that step.

- **Migrations must be backward-compatible** (convention applying to every step with a schema change). Add-column: OK. Drop-column: requires expand-contract over two deploys (deploy code that no longer reads the column → deploy migration that drops it). Rename: same shape. Documented in `docs/self-host.md` (step 12) and referenced from every migration file's header comment. Effort: N/A (convention). Priority: P1 (immediate, applies starting step 3).

- **MV upgrade choreography runbook.** When evolving a materialized view (e.g. `events_v2 → events_v3`) while data is live: spec the parallel-MV + backfill + read-swap + drop procedure. Write the runbook the first time we need it, not preemptively. Effort: M when needed. Priority: P3.
