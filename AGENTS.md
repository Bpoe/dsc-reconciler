# AGENTS.md

## Purpose

`dsc-reconciler` is a small Go repository. Its executable and service are named
`dscd`. The daemon periodically discovers local DSC configuration documents,
also reconciles changed inputs through a filesystem fast path, invokes DSC,
and writes execution results to disk.

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
|   |-- linux/
|   |   `-- nfpm.yaml
|   |-- systemd/
|   |   `-- dscd.service
|   `-- windows/
|       |-- install.ps1
|       |-- uninstall.ps1
|       |-- test-service.ps1
|       `-- msi/
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
| `internal/dsc` | Pass-scoped DSC server processes, MCP initialization, JSON-RPC framing, request timeouts, and execution-result types. |
| `internal/reconcile` | Discovery, ordering, daemon metadata parsing, input snapshots/hashing, periodic and filesystem-triggered scheduling, and coordinating execution with result publication. |
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

DSC owns schema validation, resource semantics, and execution. Inspect configuration
syntax and embedded `metadata.dscd` only; parameter sidecars remain opaque. Associate
`.parameters.yaml` and `.parameters.json` files by basename, reject ambiguous
basenames, and never reconcile sidecars independently. Read each input once,
hash the captured UTF-8 strings and effective operation using the documented
`dscd-input-v2` framing, and submit exactly those strings and operation to DSC.
Hash the original document, including all metadata bytes, and the normalized
`set`/`test` operation. Formatting and ignored-property edits change the submitted
input identity, not the operation. The hash is not a skip key.
Use `go.yaml.in/yaml/v3` nodes for YAML and `encoding/json` raw messages for JSON
to extract metadata without modeling DSC resources. Never strip or reserialize inputs.

Optional `metadata.dscd` in each configuration is owned by `dscd` and extracted
under `internal/reconcile`. Only `operation` has behavior today: exactly `set` or
`test`. Missing metadata, missing `dscd`, empty objects, or an omitted operation
defaults to `set`. Ignore unknown properties at every level, including arbitrary
nested values. Validate known containers and operation values. Resolve YAML
aliases and merges along the metadata path rather than silently enforcing `set`.
Keep `.yaml` and `.json` configuration discovery; `.yml` remains ignored.

For example, `web.yaml` and `web.parameters.yaml` form one input. To make it
audit-only, the configuration includes:

```yaml
metadata:
  dscd:
    operation: test
```

The companion `.dscd.json` mechanism is removed. Those files now follow ordinary
`.json` discovery, not special exclusion or association rules.

**No embedded operation means `set`: placing a configuration in the directory normally
authorizes DSC to enforce that desired state.** Producers must publish audit-only
metadata in the configuration itself. Malformed/unreadable input or an invalid
explicit operation fails only that configuration, publishes an `input` failure
without invoking DSC, and does not block later configurations. Never fall back to
`set` for invalid metadata or log arbitrary metadata contents.
Persist the effective `operation`, including default `set`, in every result.
Use null when input failures prevent resolving the operation; do not claim that
an operation was submitted for an invalid configuration.

Start one short-lived `dsc server` per nonempty executable pass, initialize MCP
and submit configurations sequentially with `invoke_dsc_config`, using each
configuration's effective operation in the same session. A successful `test`
with `inDesiredState: false` is not a daemon execution failure: preserve the raw
`dscResult` and do not interpret resource state. Close and reap
the server before waiting for the next pass. Restart within a pass only after a
server/protocol failure or request timeout; ordinary DSC errors retain the session.
Do not keep DSC alive between passes, introduce an MCP SDK, or interpret resource
state. Server sidecars are direct parameter mappings, not CLI-wrapped envelopes;
the daemon passes them unchanged.

Filesystem watching is a low-latency optimization, not the correctness mechanism.
Use the same discovery and single-candidate execution path for targeted and full
passes. Keep all DSC work serial. Watch the directory using fsnotify; do not
duplicate filename/sidecar rules in the collector. Create/Write events include
atomic rename destinations; old-name Rename, Remove, and Chmod do not trigger
immediate reconciliation. New pairs are published parameter-first; orphan
sidecars do not execute or wait in a pairing queue. Never add a debounce delay.
Coalesce pending notifications, but retain changes received during execution.

Targeted work does not reset the periodic full-pass timer; due full passes take
priority after active work finishes. Revalidate the directory identity at every
full-pass boundary, even without watcher errors; retire stale watches and retry
registration before discovery. Log watcher failures and preserve periodic
fallback. Collector notifications must never wait for the scheduler: use
nonblocking wake-ups and independently stored pending state, and a nonblocking
failure signal. Keep mutex sections limited to in-memory state changes, never
I/O, logging, execution, or cleanup. Close and join every owned watcher/collector.

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
The non-standard dependencies are `golang.org/x/sys` for Windows SCM, Event Log,
Job Objects, protected ACLs and native replacement, and `go.yaml.in/yaml/v3` for
embedded metadata extraction, and `github.com/fsnotify/fsnotify` for directory
notifications on Linux and Windows. Do not add a general service framework or DSC schema
dependency. Use the Go version declared in `go.mod` and CI.

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
- Embedded metadata extraction/defaults/validation, mixed operations within a session,
  operation-aware hashes, persisted operation, and successful tests reporting drift.
- Immediate first reconciliation, a full interval after each completed full pass,
  targeted work without postponing full passes, no catch-up or overlapping passes,
  and prompt cancellation during the wait.
- Targeted configuration/parameter changes, parameter-first publication, atomic
  replacement, coalescing/in-flight changes, nonblocking collector communication,
  watcher failure recovery, and periodic directory-identity revalidation.
- MCP handshake, inline requests and exact input hashes, response IDs and framing,
  session reuse/recovery, bounded shutdown and descendant cleanup.
- Valid result JSON, filename mapping, complete replacement, publication failures,
  and temporary-file cleanup.

The DSC server compatibility target is stable 3.3.0, using the version-tagged
official server tests and implementation linked in the design. Keep input snapshots
bounded to 16 MiB combined (including embedded metadata), and stdout JSON-RPC
frames to 16 MiB. Drain shared stderr
without retaining it; per-attempt `stderr` stays empty and RPC-success `exitCode`
stays null. Never log RPC error text which may contain secrets.
Windows must attach the suspended process to a
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
go mod verify
go build ./...
go test ./...
go vet ./...
gofmt -l .
```

Format changed Go files with `gofmt -w` before finishing. CI must fail when the
formatting check lists files; `gofmt -l` alone does not return a failure status for
unformatted files. Run `go test -race ./...` for concurrency or shared-state
changes on a supported environment. Keep Makefile targets and CI aligned with
these commands. Keep normal Go checks in `ci.yaml`; service installation checks
belong in `service-integration.yaml` on disposable runners, called by releases
or triggered manually. Documentation-only edits need consistency and link checks rather
than unrelated Go tests.

Keep changes focused and preserve unrelated work. Do not add additional watchers, Cobra,
Viper, a database, generic stores, plugin machinery, nested module management,
or a large lint stack speculatively. Future commands are not current requirements.
Keep service-manager details in packaging and avoid platform assumptions in the
core packages.

Windows release packaging uses the native x64 WiX project under
`packaging/windows/msi`. Keep WiX pinned, the UpgradeCode stable, and the
documented release-tag-to-MSI version mapping ordered and collision-free.
The MSI accepts a prebuilt executable and relies on Microsoft DSC 3.3.0+ installed
separately, with `dsc.exe` on the machine/system PATH visible to LocalSystem before
service startup. User PATH and per-user executable aliases are insufficient;
system PATH changes may require a reboot before services see them. Do not add
DSC executable properties, discovery, validation, persistence or PATH modification
to the MSI. The service uses the daemon's default `dsc` executable name.
The MSI owns service/Event Log registration declaratively; never invoke the manual
PowerShell installers from an MSI. Protect both ProgramData directories for
SYSTEM and Administrators and preserve them and user data on uninstall.
Build with the pinned WiX .NET SDK; no native helper, Visual C++ toolchain or
Windows SDK build dependency is needed. Keep `build.ps1` focused on version
mapping and `dotnet build`; run `inspect.ps1` separately in every packaging workflow.
Build/inspect MSI tables in native Windows CI and release jobs; installation
tests belong only in the reusable/manual service-integration workflow on disposable
runners. No DSC bundling, per-user PATH discovery, migration logic or ARM64
packaging. See [the packaging contract](docs/design.md#windows-msi).

Linux AMD64 DEB/RPM packages share nFPM metadata under `packaging/linux` and the
unit in `packaging/systemd`. Pin nFPM in workflows; do not add it to `go.mod`.
Use stable `vX.Y.Z` release tags for package versions. RC tags are unsupported;
preserve the existing stable MSI mapping rather than renumbering versions.
Declare Microsoft `dsc >= 3.3.0` as a package dependency; never bundle or download
DSC in maintainer scripts or configure its repositories. Package-managed binaries
use `/usr/bin` and the vendor unit directory is `/usr/lib/systemd/system`.
New input/results directories are root-owned `0700`. Package removal, including
purge, must preserve administrator documents and results. Keep package inspection
separate from opt-in installation tests on disposable machines; only a running
systemd environment validates service startup.

Update `docs/design.md` and relevant tests when changing behavior or contracts.
Keep release orchestration simple: the existing three workflows, shared platform
packaging scripts, and native Actions artifacts. Release integration must test
the exact uploaded packages before draft creation. Do not add a release library,
custom artifact manifests, or automatic upgrade-baseline discovery. Upgrade
scripts remain available for explicitly supplied packages; automated release
gates currently cover fresh installations, not upgrades.
Update the README and packaging when changing operator-facing options. Inspect
the final diff for scope creep. Report what changed, which checks ran, and any
unverified behavior. Never claim a check passed if it was not run.
