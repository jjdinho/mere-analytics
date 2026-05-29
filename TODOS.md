# TODOs

Items that don't belong to any single step's plan in `docs/plan.md`. Each step's "Decisions for this step" subsection in plan.md is authoritative for that step.

- **Migrations must be backward-compatible** (convention applying to every step with a schema change). Add-column: OK. Drop-column: requires expand-contract over two deploys (deploy code that no longer reads the column → deploy migration that drops it). Rename: same shape. Documented in `docs/self-host.md` (step 12) and referenced from every migration file's header comment. Effort: N/A (convention). Priority: P1 (immediate, applies starting step 3).

- **MV upgrade choreography runbook.** When evolving a materialized view (e.g. `events_v2 → events_v3`) while data is live: spec the parallel-MV + backfill + read-swap + drop procedure. Write the runbook the first time we need it, not preemptively. Effort: M when needed. Priority: P3.

- **Forced password-change UX after `must_change_password=true`.** Step 3 added the column and the operator script sets it, and `auth.Session.MustChangePassword` is plumbed end-to-end, but login still drops the user on the normal home page. Add a `/account/change-password` interstitial that authenticated users with `must_change_password=true` are forced through before any other authenticated route renders. Easiest to ship alongside step 4 (when there's other account/team UI to anchor it on). Effort: S. Priority: P2.
