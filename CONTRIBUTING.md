# Contributing

The full contributor guide lives in the docs:
<https://tarathestar.github.io/enso/contributing/>
(source: [`docs/content/contributing.md`](docs/content/contributing.md)).

The very short version:

- Read [`AGENTS.md`](AGENTS.md) — it is the architectural source of
  truth (package layout, non-goals, dependency policy, soak-test
  risks).
- Run `make check` before sending a patch (`gofmt + vet + test +
  build`). CI runs the same target.
- No CGO, no new dependencies without discussion.
- For anything architectural (new internal package, public-API
  change, new abstraction), open an issue to discuss the design
  before writing the implementation.
- Report security issues privately — see [`SECURITY.md`](SECURITY.md).

By contributing, you agree your contribution is licensed under the
project's license (`AGPL-3.0-or-later`; see [`LICENSE`](LICENSE)).
