# Changelog — otel-sampler

All notable changes to the `otel-sampler` module. This module follows the
repo-wide pre-1.0 policy in [VERSIONING.md](../VERSIONING.md): a breaking change
warrants at least a minor bump while the module is on `0.x`.

## 0.1.1

First version released under the repo's versioning policy (version constant,
CHANGELOG, and release-guard coverage).

The behavioral changes below would normally warrant a minor bump under the
pre-1.0 rule in [VERSIONING.md](../VERSIONING.md). A patch bump is used instead
because `0.1.0` never pointed at content matching this repository (see the
`0.1.0` entry), so there is no released behavior for a caller to have depended
on — the "breaking" changes are relative to an unusable tag.

### Added

- `Threshold(probability float64) uint64` and `Sampled(probability float64, rv uint64) bool`
  export the sampler's exact threshold arithmetic, including its sub-threshold
  precision rounding. Test harnesses and assertions can now predict a decision
  for **any** randomness value instead of re-deriving `(1-p)·2^56`, which
  diverges from the real decision on boundary values.

### Changed

- **BREAKING (behavioral)** — `WithSingleLinkSeed` no longer copies the link's
  entire tracestate onto the linked root. Only the link's `ot=rv:` randomness is
  carried over; foreign vendor members (`congo=…`, `rojo=…`) and the upstream
  `th:` are dropped. Previously a brand-new, unrelated trace inherited both,
  which fed third-party vendor entries to every downstream hop of that trace and
  — on the Drop path — made downstream exporters compute the adjusted count from
  the *upstream* probability.
- `WithSingleLinkSeed` now preserves the caller's `ParentContext` values when it
  synthesizes the remote parent, instead of replacing the context with
  `context.Background()`. A delegate that reads baggage or other context values
  now sees them on the single-link path, matching the parent-child path.
- `WithSingleLinkSeed`'s godoc documents that the wrapper also writes
  `ot=rv:` into `SamplingResult.Tracestate` on root paths (it does not only
  adjust sampler input), and warns against composing it with
  `sdktrace.ParentBased` — the synthesized seed is a valid remote parent, so
  ParentBased would decide from the link's sampled flag and ignore the
  probability entirely.
- Dependencies raised to OpenTelemetry `v1.44.0`, matching the instrumentation
  modules' floor. The module previously resolved `v1.39.0` when tested alone.

### Fixed

- Removed an unreachable `Delete("ot")` branch in `ProbabilitySampler`
  (`thkv` is never empty, so the branch could not be taken) and merged the
  duplicated `th:`/`rv:` tracestate sub-key writers into one helper, so a fix on
  one no longer has to be mirrored by hand onto the other.

## 0.1.0

Initial tag. Cut before the module stabilized and **superseded** — the published
`otel-sampler/v0.1.0` tag points at a pre-rebase commit that is not an ancestor
of `main`. Because the Go module proxy caches versions immutably, that version
number cannot be reused; `0.1.1` is the first version whose content matches the
repository. Use `0.1.1` or later.
