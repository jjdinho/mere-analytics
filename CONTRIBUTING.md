# Contributing to mere

Thanks for your interest in improving mere. There is one non-negotiable
requirement — a **sign-off plus a license grant** on every contribution — and
then the usual practical bits. Please read the first two sections before you
open a pull request.

## Why this matters here

mere's core is licensed under **AGPL-3.0-or-later**
([ADR-0001](docs/adr/0001-adopt-agpl-3.0.md)). We also run a **paid hosted
build** of the same code, which links a small amount of proprietary glue
([ADR-0002](docs/adr/0002-open-core-hosted-wrapper.md)). That hosted build is
legal only because the maintainers hold the copyright to the whole core and can
license it to themselves under separate terms — classic dual-licensing.

A patch that lands without granting us that same flexibility becomes AGPL-only,
and from that moment the hosted build can no longer legally link the core. To
keep dual-licensing intact, **every contribution must carry both** of the
following: a Developer Certificate of Origin sign-off (§1) and the contribution
license grant (§2).

## 1. Sign off every commit (DCO)

We use the [Developer Certificate of Origin](DCO) (DCO 1.1) — a lightweight
statement that you wrote the patch, or otherwise have the right to submit it
under the project's license. You assert it by adding a `Signed-off-by` line to
each commit:

```
Signed-off-by: Your Name <you@example.com>
```

Git adds this for you with the `-s` flag:

```
git commit -s -m "your message"
```

The name and email must be your real ones and must match the commit author.
Every commit in a pull request needs the line; you can add it to existing
commits with `git rebase --signoff <base>`. The full text you are agreeing to is
in [`DCO`](DCO).

## 2. Contribution license (dual-licensing grant)

The DCO alone only certifies provenance — it does not let us license your code
under anything but AGPL. So, in addition to the DCO:

> By submitting a contribution (a pull request, patch, or any other change) with
> your sign-off, you grant the maintainers of mere and their successors and
> assigns a perpetual, worldwide, non-exclusive, royalty-free, irrevocable
> copyright and patent license to reproduce, modify, prepare derivative works
> of, publicly display and perform, sublicense, and distribute your contribution
> and such derivative works — **under AGPL-3.0-or-later _and_ under any other
> license terms, including proprietary or commercial terms.** You confirm you
> are legally entitled to grant this license (for example, if your employer has
> rights to work you do, that you have their permission).

You keep the copyright to your contribution. This is a license grant, **not** an
assignment — you remain free to use your own code however you like. It simply
lets us keep offering mere both as AGPL open source and as our hosted build, the
way [ADR-0001](docs/adr/0001-adopt-agpl-3.0.md) and
[ADR-0002](docs/adr/0002-open-core-hosted-wrapper.md) describe.

If you cannot make this grant for a particular contribution, please say so in
the pull request **before** we review it, rather than signing off.

## Working on a change

- For anything beyond a small fix, open an issue first so we can agree on the
  approach before you invest time.
- mere is test-first: add or update tests with your change and make sure the
  suite passes. See the [Quickstart](README.md#quickstart-local) for bringing up
  a local stack.
- Keep changes focused and match the surrounding style.

That's it — thanks for contributing.
