## ADDED Requirements

### Requirement: Module layout follows Go community convention

Every instrumentation module SHALL follow the same directory layout, aligned with idiomatic Go module conventions and the widely-referenced `golang-standards/project-layout` guide. A new contributor SHALL be able to open any of the four modules and find the same categories in the same places.

Canonical layout per module:

```
<module>/                     # module root (one go.mod per module)
├── go.mod / go.sum
├── README.md / README.zh-TW.md
├── CHANGELOG.md              # release notes (if module is published)
├── LICENSE                   # if not symlinked from repo root
├── doc.go                    # package overview shown by `go doc` and pkg.go.dev
├── version.go                # `Version()` and `instrumentationVersion` const
├── <facade files>.go         # public types and constructors (e.g. conn.go, collection.go)
├── tracing.go                # public tracing helpers reused by callers
├── env_flags.go              # gate composition, calls into internal/flags
├── options.go                # functional `With*` options
├── internal/                 # compiler-enforced privacy — nothing exported
│   ├── flags/                # cross-module-shared gate helpers (byte-identical across modules)
│   ├── shared/               # interfaces + helpers reused by direct and traced
│   ├── direct/               # disabled-mode impls — zero otel/sdk imports
│   └── traced/               # enabled-mode impls — full instrumentation
├── examples/                 # runnable example programs (separate go.mod)
│   └── <demo>/main.go
└── tests/                    # integration tests (separate go.mod, testcontainers)
    └── integration/
```

#### Scenario: All four modules share the layout

- **WHEN** a reviewer compares the directory trees of `otel-mongo/otelmongo`, `otel-mongo/v2`, `otel-nats`, and `otel-gorilla-ws` after this change
- **THEN** each top-level category (`internal/{flags,shared,direct,traced}`, `examples/`, `tests/integration/`) SHALL appear in the same place with the same name in every module
- **AND** the only allowed differences SHALL be the module-specific facade file names (e.g. `collection.go` in mongo vs `conn.go` in nats/ws)

#### Scenario: Layout matches Go convention

- **WHEN** a Go developer familiar with `golang-standards/project-layout` opens any module
- **THEN** `internal/` SHALL hold all packages not meant for downstream import (Go's compiler enforces this)
- **AND** `examples/` SHALL be the directory name for runnable demos (plural, per Go community trend), NOT `example/`
- **AND** `tests/integration/` SHALL hold integration tests separated from package-local `_test.go` files
- **AND** there SHALL be no top-level `pkg/` subdirectory within a module (consumers import the module directly)

### Requirement: Facade files at module root, helpers grouped by concern

The module root SHALL contain only public facade files (constructors, the wrapper types) and helpers that are part of the public API surface. Internal helpers SHALL live under `internal/`. Files SHALL be grouped by concern, not by file type — e.g. `collection.go` holds the `Collection` facade + its options + its compile-time impl assertions in one place rather than scattering across `types.go` / `interfaces.go` / `methods.go`.

#### Scenario: Module root has no internal-only files

- **WHEN** a reviewer lists the module root (`ls <module>/*.go`)
- **THEN** every file SHALL define at least one exported identifier consumed by users of the module
- **AND** purely-internal helpers SHALL be moved under `internal/`

#### Scenario: Compile-time assertions co-located with facade

- **WHEN** the facade declares `collectionImpl` (or equivalent) interface
- **THEN** the assertions `var _ collectionImpl = (*direct.Collection)(nil)` and `var _ collectionImpl = (*traced.Collection)(nil)` SHALL live in the same file as the facade type
- **AND** the assertions SHALL NOT be split into a separate `asserts.go` or `bindings.go`

### Requirement: User-friendly README structure

Each module's `README.md` and `README.zh-TW.md` SHALL follow the same section order so a user can scan any module in the same way:

1. One-line description and import path
2. Status badge / version
3. Quick start (10-line example)
4. Feature flags table (env vars + defaults)
5. Public API surface
6. Disabled-mode behaviour and migration notes
7. Internals overview (one paragraph + tree diagram)
8. Versioning / release policy

#### Scenario: Consistent section order

- **WHEN** a user opens the README of any of the four modules
- **THEN** the section headings SHALL appear in the canonical order above
- **AND** the `## Feature Flags` table SHALL list every env var the module reads, its default (`disabled`), and the truthy values accepted

#### Scenario: Tree diagram present

- **WHEN** the reader scrolls to "Internals overview"
- **THEN** the section SHALL contain an ASCII tree showing `internal/{flags,shared,direct,traced}/` and a one-line description of each subpackage

### Requirement: `examples/` (plural) for runnable demos

Each module SHALL ship runnable example programs under `examples/<demo-name>/`. The current singular `example/` directory in `otel-nats`, `otel-mongo`, `otel-gorilla-ws` SHALL be renamed to `examples/`. Each example SHALL be its own Go module (separate `go.mod`) so module dependency closure is not polluted by demo dependencies (otelresty, gin, etc.).

#### Scenario: Singular `example/` renamed

- **WHEN** the change lands
- **THEN** no module SHALL contain a top-level `example/` directory
- **AND** runnable demos SHALL live under `examples/<demo-name>/`

#### Scenario: Each example is its own module

- **WHEN** a reviewer opens any `examples/<demo>/` directory
- **THEN** it SHALL contain a `go.mod` and a `main.go`
- **AND** the parent module's `go.mod` SHALL NOT depend on example-only third-party packages

### Requirement: `tests/integration/` for testcontainers-based tests

Each module SHALL keep its testcontainers-based integration tests under `tests/integration/` as a separate Go submodule. Unit tests SHALL stay co-located with the code as `*_test.go` files in the package they test.

#### Scenario: Integration tests isolated

- **WHEN** `go test ./...` is run from the module root
- **THEN** it SHALL run only unit tests (co-located `*_test.go`)
- **AND** integration tests under `tests/integration/` SHALL run only when `cd tests/integration && go test ./...` is invoked

#### Scenario: Testcontainers dependency isolated

- **WHEN** a reviewer inspects the module root `go.mod`
- **THEN** `github.com/testcontainers/testcontainers-go` SHALL NOT appear as a direct dependency of the module root
- **AND** it SHALL appear in `tests/integration/go.mod` only

### Requirement: `internal/` subpackage naming is consistent

The four canonical subpackages under `internal/` SHALL be named `flags`, `shared`, `direct`, `traced`. No module SHALL use synonyms (e.g. `gate`, `common`, `disabled`, `instrumented`).

#### Scenario: Consistent subpackage names

- **WHEN** a reviewer runs `find <repo>/pkg/instrumentation-go -type d -path '*/internal/*' -maxdepth 4`
- **THEN** every entry SHALL match one of `internal/flags`, `internal/shared`, `internal/direct`, `internal/traced` (or be a sub-path under one of them)
- **AND** no other top-level internal subpackage SHALL exist unless explicitly documented in `pkg/instrumentation-go/CLAUDE.md`

### Requirement: Layout migration is mechanical and auditable

The directory restructure SHALL be a separate commit (or commit series) from the feature-flag logic change so reviewers can audit the layout move via `git log --follow` and `git diff -M` without semantic noise.

#### Scenario: Layout move is a rename commit

- **WHEN** a reviewer runs `git log --follow --stat <module>/examples/<demo>/main.go`
- **THEN** the history SHALL trace back to the previous `example/<demo>/main.go` location via Git rename detection
- **AND** the rename commit message SHALL state `chore: rename example/ → examples/ for Go layout convention` (or equivalent)

### Requirement: Documentation lists the layout in one place

`pkg/instrumentation-go/CLAUDE.md` SHALL include a top-level "Module Layout" section that shows the canonical tree and names each category. Per-module READMEs MAY repeat the tree but SHALL NOT define a different shape.

#### Scenario: Single source of layout truth

- **WHEN** a contributor needs to verify whether a file belongs at the module root or under `internal/shared/`
- **THEN** the `pkg/instrumentation-go/CLAUDE.md` "Module Layout" section SHALL provide the definitive answer
- **AND** per-module READMEs SHALL reference this section rather than duplicating contradictory guidance
