# 0001 — Adopt AGPL-3.0 for the open-source core

- **Status:** Accepted
- **Date:** 2026-06-02
- **Supersedes:** the "Apache 2.0" entry in the plan.md tech-stack table and README.

## Context

mere is open source and self-hostable: a self-hoster gets the whole product,
with no cloud-only tier (README; [plan.md](../plan.md) "Self-hosters get the
full product"). We also intend to offer a **paid hosted version** of the same
software — see [ADR-0002](0002-open-core-hosted-wrapper.md).

The repo currently declares **Apache 2.0**. Apache is maximally permissive:
anyone — including a well-funded cloud vendor — can take mere, run it as a
competing managed service, add their own closed improvements, and never
contribute anything back. For a small project whose business model *is* "we
host it for you," that is the central commercial risk. The license is the only
lever that addresses it without making the code non-free.

The two licenses that the comparable projects in this space converged on:

- **AGPL-3.0** — Plausible, PostHog, Grafana, Mastodon. A strong copyleft with a
  *network-use* clause (§13): if you run a **modified** version and let users
  interact with it over a network, you must offer those users the corresponding
  source of your modifications. Still OSI-approved, still genuinely free/open —
  a self-hoster's rights are completely intact.
- **BSL / source-available** — Sentry, HashiCorp, CockroachDB. Blocks competing
  hosting for ~4 years, then converts to an OSS license. Effective, but it is
  **not** OSI-approved open source, which conflicts directly with mere's stated
  "you own the data, whole product, real open source" positioning.

## Decision

License the open-source core under **GNU AGPL-3.0-or-later**.

The canonical license text lives in [`/LICENSE`](../../LICENSE). README and the
plan.md tech-stack table are updated to match.

AGPL is chosen over BSL because keeping mere *actually* open source (OSI-approved,
self-hosters fully unrestricted) is a core part of the product's identity, and
over Apache because the network-copyleft is what discourages a competitor from
running a proprietary fork of our own software as a rival service.

## Consequences

**Good**

- A competitor who hosts a *modified* mere must release their modifications.
  This neutralizes the "take it, improve it secretly, out-compete the authors"
  scenario that Apache permits.
- Self-hosters are unaffected. Running an unmodified mere for your own org has no
  new obligation; AGPL's source-offer duty attaches to *distributing or
  network-serving a modified version to others*, not to internal use.
- It aligns mere with the de-facto standard for "open source + we sell hosting"
  (Plausible, PostHog), which lowers the explaining-it cost with prospective
  users and contributors.

**The obligation we take on — dual-licensing requires we own the copyright.**

This is the consequence that matters for the business and must not be lost:

- Our own paid hosted build will link proprietary code (billing, quota
  enforcement) against the core. AGPL's copyleft would normally force *that*
  combined work to be AGPL too. We are exempt from this **only because we are the
  sole copyright holder** — a copyright holder is not bound by the license they
  grant to others, so we can license our own core into a closed wrapper under
  separate terms (classic dual-licensing). See
  [ADR-0002](0002-open-core-hosted-wrapper.md) for how the wrapper is structured
  to keep the proprietary surface as small (and as cleanly separated) as
  possible.
- **Therefore: every external contribution must come with a CLA or a DCO + broad
  license grant** that lets us continue to license the contributed code under
  both AGPL *and* our proprietary hosted terms. Without this, the moment we merge
  an outside contributor's patch, that patch is AGPL-only and our closed wrapper
  can no longer legally link the core. This is a hard prerequisite, not a
  nice-to-have. Action: add `CONTRIBUTING.md` + a CLA/DCO gate **before** opening
  the repo to outside PRs. (Tracked as a follow-up; the repo is single-author
  today, so there is no immediate breach — but the gate must exist before the
  first external merge.)

**Costs / risks**

- A minority of enterprises have blanket "no AGPL" procurement policies. This can
  cost some self-host adoption. Mitigation: the hosted offering is the path for
  anyone who can't take AGPL on-prem; and dual-licensing leaves the door open to
  sell a commercially-licensed build later if real demand appears.
- AGPL §13 only bites on *modified* versions. A competitor running mere
  **unmodified** as a service owes nothing. AGPL is a deterrent against
  proprietary forks, not a prohibition on managed hosting per se. We accept this;
  the operational moat (we run it well) carries the rest, consistent with the
  Plausible model.

## Alternatives

- **Keep Apache 2.0.** Rejected: gives away the one structural defense against a
  proprietary competing fork, which is precisely the threat to a hosting
  business.
- **BSL / source-available.** Rejected: not OSI open source; contradicts the
  "real open source, you own it" positioning. Reconsider only if AGPL proves
  insufficient against a specific competitive threat.
- **MIT core + a separately-licensed `ee/` directory in the same repo** (the
  PostHog/Cal.com shape). Rejected for now: it puts proprietary code *in* the
  open repo and leans on per-directory license headers, which is more bookkeeping
  than a clean two-repo split. AGPL core + a private wrapper repo
  ([ADR-0002](0002-open-core-hosted-wrapper.md)) keeps the open repo 100% open.
