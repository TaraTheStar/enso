---
# Conditional ship-vs-escalate router. `review` runs once and emits a
# structured verdict; the two `if`-gated edges out of it route to exactly
# one of `ship` / `escalate`. The branch not taken is skipped (it never
# runs and reports `(skipped)`).
#
# This is the canonical "structured outputs in action" example: the
# reviewer's `{"verdict": ...}` field is what the edge predicates read.
#
# Note the eq/ne pairing: a *missing* verdict makes the eq-gate NOT fire
# (ship stays put) and the ne-gate fire (escalate runs) — i.e. an
# unparseable review escalates, which is the safe direction.
#
# Run with: /workflow gated-ship <feature description>
roles:
  build:
    tools: [read, write, edit, bash, grep, glob]
  review:
    tools: [read, grep, glob]
  ship:
    tools: [bash]
  escalate:
    tools: [bash]
edges:
  - build  -> review
  - review -> ship      if '{{ eq .review.verdict "LGTM" }}'
  - review -> escalate  if '{{ ne .review.verdict "LGTM" }}'
---

## build

You are the builder. Implement the following request using the
read/write/edit/bash tools:

{{ .Args }}

When done, summarise what you changed, file by file. Do not inline the
diff — the reviewer will inspect the files directly.

## review

You are the reviewer. Inspect the changes summarised below against the
original request. Use read/grep/glob to check the actual file contents —
do not trust the builder's summary alone.

# Original request

{{ .Args }}

# Builder's summary

{{ .build.output }}

# Output

Write your assessment as prose, then end your message with a fenced JSON
block carrying the machine-readable verdict the router reads:

```json
{"verdict": "LGTM", "reason": "one-line justification"}
```

Use `"LGTM"` only if the changes are correct and complete; otherwise use
`"changes-requested"` (any non-LGTM value routes to escalate).

## ship

The review passed. Tag the work as shippable: run whatever release or
merge step this project uses (e.g. `git tag`, open a PR with `gh`), then
report what you did.

# What was built

{{ .build.output }}

## escalate

The review did **not** pass. Do not ship. Summarise the outstanding
problems for a human, using the reviewer's reasoning below, and open or
update a tracking issue if the project uses one.

# Reviewer's findings

{{ .review.output }}
