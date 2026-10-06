# AGENTS.md

## Purpose

`dsc-reconciler` is a small Go repository. Its executable and service are named
`dscd`. The daemon periodically discovers local DSC configuration documents,
invokes DSC, and writes execution results to disk.

Linux and Windows are required v1 platforms, including foreground execution,
systemd and native Windows SCM operation. Keep the core loop shared; use
`*_linux.go` and `*_windows.go` for process, filesystem and service differences.
macOS is not supported. Cross-compilation is not runtime validation.

This file tells coding agents how to work in the repository. Read
[docs/design.md](docs/design.md) for service behavior, defaults, execution
semantics, and file contracts. Keep that document and the implementation aligned.
Read the README, relevant code, tests, and build configuration before editing.

A seasoned Go developer should immediately understand the code: small packages
organized by responsibility, standard library first, concrete types first, and
interfaces defined by consumers. Add abstractions only for present requirements.

## Repository shape

Use one Go module at the repository root and a shallow layout:

```text
dsc-reconciler/
|-- AGENTS.md
|-- cmd/
|   `-- dscd/
|       `-- main.go
|-- internal/
|   |-- config/
|   |   |-- config.go
|   |   `-- config_test.go
|   |-- dsc/
|   |   |-- client.go
|   |   `-- client_test.go
|   |-- reconcile/
|   |   |-- reconciler.go
|   |   `-- reconciler_test.go
|   `-- results/
|       |-- writer.go
|       `-- writer_test.go
|-- packaging/
|   |-- systemd/
|   |   `-- dscd.service
|   `-- windows/
|       |-- install.ps1
|       |-- uninstall.ps1
|       `-- test-service.ps1
|-- docs/
|   `-- design.md
|-- go.mod
|-- go.sum                 # Only when dependencies require it.
|-- README.md
|-- LICENSE
`-- Makefile
```

Create files when needed; do not build empty scaffolding. Keep tests beside the
code they exercise. Add files within existing packages before creating new
packages. Keep implementation under `internal`; there is no public Go library
surface. Do not add `pkg/`, nested modules, `go.work`, `src/`, a parallel test tree,
or a generic project-layout directory collection. See Go's
[module organization guidance](https://go.dev/doc/modules/layout).

## Package boundaries

| Location | Owns |
| --- | --- |
| `cmd/dscd` | Dependency wiring, logging setup, signals, lifecycle, and exit status. |
| `internal/config` | Typed daemon options, flag parsing, defaults, and validation. |
| `internal/dsc` | DSC arguments, process execution, output capture, and execution-result types. |
| `internal/reconcile` | Discovery, ordering, the periodic loop, and coordinating execution with result publication. |
| `internal/results` | Result serialization, destination naming, permissions, and safe file replacement. |

Keep `main` boring: load config, create the DSC client and result writer, create
the reconciler, and run until shutdown. A small `run` helper is fine for clear
error handling and cleanup. No application host, DI container, service locator,
or startup framework.

Define small interfaces such as `DSC` and `ResultWriter` in `reconcile`, where they
are consumed. Concrete implementations satisfy them implicitly. Shared execution
result types can live in `internal/dsc`. Keep dependencies acyclic: the client and
writer must not depend on the scheduling loop. The client does not write results;
the writer does not invoke DSC or schedule work.

DSC owns document parsing, validation, resource semantics, and execution. Treat
configuration documents as opaque inputs. Do not introduce a YAML parser to
inspect files DSC can consume. Parsing DSC execution output is a separate,
necessary boundary responsibility; it does not justify modeling DSC resources.

## Go conventions

- Use short, lowercase package names and clear identifiers. Prefer
  `reconcile.New`, `dsc.NewClient`, and `results.NewWriter` over names such as
  `NewReconciliationManager` or `NewDSCCommandLineClient`.
- Start with concrete structs and ordinary functions. Avoid interface hierarchies,
  `internal/interfaces`, `internal/abstractions`, and generic utility packages.
- Pass dependencies explicitly. Avoid mutable package globals and hidden `init`
  side effects. Export only what another package needs.
- Pass `context.Context` first for operations requiring cancellation. Preserve the
  caller's context through the execution path.
- Return errors with useful operation and path context. Wrap with `%w` when callers
  need the cause; inspect with `errors.Is` or `errors.As`.
- Use errors for expected failures, not panic/recover. Keep `os.Exit` at the command
  boundary and ensure necessary cleanup runs before exiting.
- Prefer early error returns and straightforward control flow. Use `defer` at the
  right lifetime; do not accumulate open files across a long loop.
- Add concurrency only for a concrete need. Every goroutine needs an owner and an
  exit path. Keep the initial reconciliation loop serial.
- Use `path/filepath` for filesystem paths and `os/exec` with separate arguments
  for DSC invocation. Do not construct shell commands.
- Format with `gofmt` and document exported declarations. Follow
  [Effective Go](https://go.dev/doc/effective_go) for core naming and style.

Prefer the standard library: `log/slog`, `context`, `os/exec`, `os`,
`path/filepath`, `encoding/json`, `time`, `flag`, `os/signal`, and sorting helpers.
The only non-standard dependency is `golang.org/x/sys` for Windows SCM, Event Log,
Job Objects, protected ACLs and native replacement. Do not add a general service
framework. Use the Go version declared in `go.mod` and CI.

## Configuration, logging, and results

Keep daemon configuration small and typed. Use the standard `flag` package and
make parsing independently testable, using a local `flag.FlagSet` where useful.
Return configuration errors to the command boundary. Keep defaults, help text,
README examples, and service arguments consistent with `docs/design.md`.

Use structured `log/slog` logging configured at the command boundary. Include
stable context such as `config_path`, `result_path`, `duration`, `exit_code`, and
`error`. Log lifecycle events, useful outcomes, and actionable failures. Keep
routine enumeration details at debug level. Log errors where handled, rather
than at every layer. Do not log secrets, full documents, or raw DSC output.

Treat the JSON result envelope and filename mapping as contracts. Update their
documentation and tests together. Preserve meaningful failure results and keep
execution success distinct from successful publication. The writer owns safe
replacement and temporary-file cleanup; verify filesystem guarantees for each
supported platform. Never truncate a live result before a replacement is ready.
See the persistence requirements in `docs/design.md`.

## Testing

Use the standard `testing` package, adjacent `*_test.go` files, `t.TempDir`, and
table-driven tests where cases naturally share structure. Normal tests must run
without DSC installed and without changing the developer's machine configuration.
Use small handwritten fakes for consumer-side interfaces and controlled helper
processes for the executable boundary. Keep real-DSC tests explicitly opt-in.

Prioritize observable behavior:

- Config parsing, defaults, invalid options, and startup failures.
- Discovery, filtering, empty directories, and deterministic ordering: files
  created as `20-b.yaml` and `10-a.yaml` execute as `10-a.yaml`, `20-b.yaml`.
- Continued processing after a document failure and publication of failure results.
- Cancellation, clean shutdown, and no overlapping passes.
- DSC argument passing, separate stdout/stderr, and execution/output errors.
- Valid result JSON, filename mapping, complete replacement, publication failures,
  and temporary-file cleanup.

The DSC compatibility target is 3.1.0, using the version-tagged official CLI and
set-result references linked in the design. Keep the 16 MiB stdout / 1 MiB stderr
limits effective through `io.Copy` fast paths; do not embed an unbounded buffer
that exposes `ReadFrom`. Windows must attach the suspended process to a
kill-on-close job before resuming it. Linux must kill the entire process group.
Use native `FileRenameInfoEx` replacement on Windows, not portable `os.Rename`
or a remove-then-rename sequence. Keep Windows power-loss durability claims
distinct from Linux directory-fsync guarantees.

Test a single reconciliation pass directly. Avoid long sleeps; use small
synchronization points for lifecycle tests. Do not add tests that merely repeat
implementation details, a mocking framework, or an arbitrary coverage quota.

## Checks and completion

For Go changes, run these commands from the module root:

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .
```

Format changed Go files with `gofmt -w` before finishing. CI must fail when the
formatting check lists files; `gofmt -l` alone does not return a failure status for
unformatted files. Run `go test -race ./...` for concurrency or shared-state
changes on a supported environment. Keep Makefile targets and CI aligned with
these commands. Documentation-only edits need consistency and link checks rather
than unrelated Go tests.

Keep changes focused and preserve unrelated work. Do not add watchers, Cobra,
Viper, a database, generic stores, plugin machinery, nested module management,
or a large lint stack speculatively. Future commands are not current requirements.
Keep service-manager details in packaging and avoid platform assumptions in the
core packages.

Update `docs/design.md` and relevant tests when changing behavior or contracts.
Update the README and packaging when changing operator-facing options. Inspect
the final diff for scope creep. Report what changed, which checks ran, and any
unverified behavior. Never claim a check passed if it was not run.
