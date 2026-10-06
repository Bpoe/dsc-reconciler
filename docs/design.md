# dsc-reconciler design

## Status

This describes the v1 implementation of `dsc-reconciler`, a Go repository whose
executable and service are named `dscd`. Linux and Windows are both required:
foreground operation on either, systemd on Linux, and native SCM on Windows.
Supported filesystem targets are local Linux filesystems with rename/fsync
semantics and local NTFS on Windows 11 / Windows Server 2022 or newer. Network
shares, FAT, macOS, and hostile local producers are outside the v1 contract.

The core design is a periodic, serial loop over local documents, with DSC owning
resource execution and the daemon publishing results safely. The concrete
defaults, discovery rules, and JSON envelope below establish an initial
specification. They can evolve deliberately with corresponding tests and
documentation. Runtime and operational validation limits are recorded below;
the existence of implementation or CI configuration alone is not a test result.

## Purpose and scope

`dscd` repeatedly applies local desired state. Its job is to discover, order,
execute, record, and repeat. It requires local configuration files and the DSC
executable; it has no network service dependency of its own. DSC resources may
have their own dependencies.

```text
       Local document producers
                   |
                   | publishes complete DSC documents
                   v
         configuration directory
                   |
                   v
          dscd reconciliation loop -----> DSC executable -----> local machine
                   |                          |
                   |<--- execution result ----|
                   v
            results directory
                   |
                   v
         Operators and local tools
```

The filesystem is the input and output boundary. V1 does not include a network
listener, database, remote fetcher, desired-state compiler, resource merger,
cross-document dependency graph, distributed scheduler, plugin framework, or
general result-sink abstraction. It does not replace DSC's own resource processing.

## Service components

The repository layout and coding rules are in [AGENTS.md](../AGENTS.md).

| Component | Responsibility | Boundary |
| --- | --- | --- |
| Daemon entry point | Load options, configure logging, connect components, establish cancellation, report process exit. | No discovery or DSC outcome logic. |
| Configuration loader | Typed daemon options and validation. | No DSC document parsing. |
| Reconciliation loop | Discover and sort documents; coordinate periodic execution and publication. | No process construction or file replacement mechanics. |
| DSC client | Construct and run DSC commands; capture output; produce an execution result. | No scheduling or result-file writes. |
| Result writer | Encode the result contract and publish files safely. | No DSC invocation or resource-state interpretation. |

The loop supplies a document path and cancellation context to the DSC client,
then gives its execution result to the writer. Execution and publication have
separate errors. A result remains useful on execution failure: its source
identity, timestamps, and diagnostics must still be populated.

## Configuration and startup

The initial configuration uses the standard `flag` package:

| Flag | Go field | Default | Meaning |
| --- | --- | --- | --- |
| `-config-dir` | `ConfigDir` | Linux: `/etc/dsc/config.d`; Windows: `%ProgramData%\dsc\config.d` | Directory containing DSC documents. |
| `-results-dir` | `ResultsDir` | Linux: `/var/lib/dsc/results.d`; Windows: `%ProgramData%\dsc\results.d` | Directory containing latest execution results. |
| `-interval` | `Interval` | `30s` | Periodic reconciliation interval; must be positive. |
| `-dsc-path` | `DSCPath` | `dsc` | Executable path or name to resolve at startup using PATH (and PATHEXT on Windows). |
| `-execution-timeout` | `ExecutionTimeout` | `15m` | Positive maximum duration per document, including process/output handling. |

```sh
dscd -config-dir /etc/dsc/config.d -results-dir /var/lib/dsc/results.d -interval 30s -dsc-path dsc
```

There is no daemon config file, environment-variable override layer, or subcommand
tree in v1. Windows uses the OS `ProgramData` location (falling back to
`C:\ProgramData`); this is not a separate daemon override layer. DSC may still use
its own documented environment and resource lookup. Under SCM, the executable
automatically detects service operation; no service-mode flag is needed.

At startup, validate flags, resolve paths, verify the input directory exists and
can be read, resolve the DSC executable, and ensure the results directory can be
used. Keep input and output directories separate. Create the results directory
if missing using restrictive permissions; never create a missing input directory
silently. Startup probes temporary-file creation, sync, replacement and removal
without touching any existing result. Directory paths are made absolute and existing symlink prefixes are
resolved. Equal or nested input/output directories are rejected. An empty
existing input directory is valid. These startup checks do
not replace handling filesystem errors during operation.

Initial Linux permissions are `0700` for a newly created results directory and
`0600` for result files. Packaging may deliberately provision group access for
local readers. Windows creates results directories and protects each result
temporary file with a protected DACL granting full access only to SYSTEM,
administrators and the daemon identity. Windows `0600` mode bits alone would
not supply this protection. Do not change permissions or ownership of existing
directories. Existing directories must already have trustworthy ACLs; an
untrusted user able to replace directories can defeat path-based protections.
Configuration files must be readable by the service account and writable only
by trusted producers. The Windows installer provisions new input/output
directories for LocalSystem and administrators.

Invalid startup configuration or an unavailable executable causes a clear error
and a nonzero exit. Successful startup leads to an immediate reconciliation pass.

## Discovery and reconciliation

Scan direct children of `ConfigDir`. Select regular files with case-sensitive
extensions `.yaml`, `.yml`, or `.json`. Ignore directories, symlinks, hidden names,
and files whose final extension is not eligible, such as `example.yaml.tmp`.
Sort the selected basenames using Go string ordering.

Each pass uses that ordered list and runs one document at a time. Recheck that a
candidate is still an eligible regular file before execution; if it disappeared
or became unreadable, record an input failure and continue. This is operational
validation, not a security boundary against a malicious local writer. Trusted
producers must publish using temporary files and replacement, not in-place edits.

Use a ticker for subsequent passes. A slow pass may be followed by a pass when
a tick is pending; there is no promise of an additional full interval after its
completion. Missed periods do not form a durable work queue. There is exactly
one active pass and one DSC invocation at a time within a daemon instance.

```text
validate startup
run one pass immediately
until canceled:
    wait for ticker or cancellation
    on tick: run one pass
```

For every attempted document, obtain an execution result, then attempt to
publish it before moving to the next document. An execution error does not skip
publication. A publication error is logged separately and does not suppress
attempts on later documents. Cancellation stops new attempts.

Every pass rediscovers inputs. File changes are considered on subsequent passes;
the daemon does not cache a permanent document list or skip unchanged documents,
because machine state may drift even when a file has not changed.

Removal stops future attempts and does not undo machine changes. Existing result
files remain until a producer or operator removes them. They are historical
evidence of a last attempt, not proof that the corresponding input still exists.
Renaming a document creates a new identity and leaves the old result behind.

## DSC invocation and outcome

The initial invocation is equivalent to:

```sh
dsc config set --file /absolute/path/to/document.yaml --output-format json
```

Pass arguments directly through `os/exec`, without a shell. Use the resolved
executable and an absolute input path. Capture stdout and stderr separately.
The compatibility target is **Microsoft DSC 3.1.0**, not Windows PowerShell DSC.
The invocation and required output fields were verified against the official
version-tagged [CLI reference](https://github.com/PowerShell/DSC/blob/v3.1.0/docs/reference/cli/config/set.md)
and [set-result reference](https://github.com/PowerShell/DSC/blob/v3.1.0/docs/reference/schemas/outputs/config/set.md).
The daemon does not invoke `--version` or enforce a version-string match at
startup. Other v3 releases are compatible only if they preserve those arguments,
the required envelope, and process/resource behavior; they are not automatically
certified. Normal tests use a synthetic fixture of the documented envelope.
Real DSC/resource execution remains an explicit opt-in deployment check.

Use `config set` directly because DSC documents that this operation validates
the input, tests resources, and applies changes where needed. A separate
daemon-managed test/set sequence would duplicate that responsibility. Force JSON
output explicitly. See Microsoft's
[`dsc config set` reference](https://learn.microsoft.com/en-us/powershell/dsc/reference/cli/config/set?view=dsc-3.0).

Parse only the result envelope needed to assess execution. Require a JSON object
with object-valued `metadata`, array-valued `results` and `messages`, and a boolean
`hadErrors`. Preserve its payload without interpreting individual resource properties or
rejecting unknown fields. Successful execution requires a zero process exit and
a valid result with `hadErrors: false`. A nonzero exit, reported DSC errors, or
missing/malformed expected output is a failed attempt. This reports DSC execution
success, not a permanent compliance guarantee. See the
[DSC set-result schema](https://learn.microsoft.com/en-us/powershell/dsc/reference/schemas/outputs/config/set?view=dsc-3.0).

The input remains an opaque file supplied to DSC. Do not parse YAML, merge
documents, reinterpret resource identities, or infer dependencies between
files. Two documents that manage conflicting state may continually undo each
other; document authors must resolve that conflict.

The path-based boundary does not bind an attempt to an immutable source revision:
replacement between discovery and DSC opening the file may affect which bytes
are executed. V1 therefore records source identity and execution times without
claiming an exact content hash or observed generation. Strong revision
correlation would require a separately designed snapshot/input contract.

## Result contract

Each source has one latest-result file. Append `.result.json` to its full basename:

```text
10-network.yaml -> 10-network.yaml.result.json
10-network.json -> 10-network.json.result.json
```

Keeping the original extension avoids collisions between different input formats.
Reject unrepresentable destination names clearly before executing the input;
never truncate or normalize names into collisions. Result names cannot contain
directory components and must be valid UTF-8 for the JSON identity. V1 limits
result basenames to 255 bytes on Linux or 255 UTF-16 code units on Windows,
and rejects paths exceeding Linux's 4095-byte or Windows' extended-path limit.
Only one daemon instance may own a given directory pair;
multi-process coordination is outside v1.

The initial envelope uses the following fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `schemaVersion` | integer | `1` for this envelope contract. |
| `configuration` | string | Exact source basename, including its extension. |
| `startedAt` | string | UTC RFC 3339 timestamp with fractional seconds as needed. |
| `finishedAt` | string | UTC timestamp in the same format. |
| `durationMs` | integer | Nonnegative elapsed milliseconds, measured with a monotonic clock. |
| `outcome` | string | `succeeded`, `failed`, or `canceled`. |
| `exitCode` | integer or null | Actual exit code when available; null if no numeric exit code is available. |
| `dscResult` | object or null | Preserved JSON object from DSC, or null when unavailable/unusable. |
| `error` | object or null | Failure classification and diagnostic message; null on success. |
| `stderr` | string | Captured DSC diagnostic output; empty if none. |

An error object contains `kind` and `message`. Initial kinds are `input`, `start`,
`exit`, `output`, `dsc`, and `canceled`. Classify cancellation first, then input or
start failure, then nonzero exit, then malformed output, then DSC-reported errors.
An execution deadline is classified as `canceled` with a deadline diagnostic, but
does not cancel the daemon or stop later documents. An internal output-limit
termination is `output`, not `canceled` or the induced nonzero `exit`; only caller
cancellation or the execution deadline takes precedence over this limit failure.
Retain an available DSC result even when the process fails. Human-readable error
messages are diagnostic text, not a stable interface for branching logic.

For example, this complete envelope represents failure to start DSC:

```json
{
  "schemaVersion": 1,
  "configuration": "10-network.yaml",
  "startedAt": "2026-10-05T18:00:00Z",
  "finishedAt": "2026-10-05T18:00:00.002Z",
  "durationMs": 2,
  "outcome": "failed",
  "exitCode": null,
  "dscResult": null,
  "error": {
    "kind": "start",
    "message": "start DSC: executable is unavailable"
  },
  "stderr": ""
}
```

All listed fields are present, including explicit nulls. The daemon records its
own envelope timestamps and duration; it preserves DSC's metadata separately in
`dscResult`. Do not synthesize successful output. Invalid stdout produces an
`output` error; it is not embedded as a fabricated DSC JSON object or dumped into
routine logs.

Readers must tolerate unknown fields and reject unsupported major envelope
versions. Additive optional fields may retain version 1; changes to field meaning,
required fields, or existing outcome semantics require a versioned contract
change. DSC's nested schema has its own version and compatibility requirements.

There is no in-progress record, attempt history, desired generation, or
resource-status translation in v1. A reader checks freshness
using timestamps and input existence, and must tolerate seeing the previous
completed attempt while a new one runs. Clock corrections can affect wall-clock
ordering; `durationMs` is the elapsed-time measurement.

## Publication and persistence failures

The writer serializes the complete envelope to a unique temporary file in the
results directory, syncs and closes it, then replaces the target. Check each
operation and clean up an unpublished temporary file after failure. Never remove
or truncate the old result before the replacement is ready. Readers should see
either the complete previous envelope or the complete replacement.

On Linux, sync the containing directory after replacement to
complete the intended crash-durability sequence. Atomic visibility and durable
storage are distinct: a directory-sync error after replacement means the new file
may already be visible but durability is uncertain. Report that condition rather
than claiming the previous file was preserved or the write fully succeeded.

Do not assume a portable `os.Rename` call establishes these guarantees on every
platform. The Windows implementation instead uses
`SetFileInformationByHandle(FileRenameInfoEx)` with `REPLACE_IF_EXISTS` and
`POSIX_SEMANTICS`, after syncing and closing the complete temporary file. It
does not fall back to remove/copy or to `os.Rename`. This supports existing open
readers on local NTFS; readers should allow `FILE_SHARE_DELETE`. A reader denying
delete sharing, antivirus software, read-only target, incompatible filesystem or
ACL may prevent publication, in which case the previous result is retained.
The native API's [replacement semantics](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information)
preserve old open handles and direct new opens to the new file.

Windows has no equivalent unprivileged directory-fsync guarantee here. File
data is flushed before replacement, but namespace persistence across sudden
power loss is **not** promised to match Linux. Linux also syncs newly created
directory entries and their parents. Even Linux durability depends on the
filesystem and hardware honoring flushes; no power-loss test is claimed.
Result permissions come from the new private temporary file, not the old file.
Administrators needing non-privileged readers must design that access explicitly.
See Go's
[`os.Rename` documentation](https://pkg.go.dev/os#Rename).

A new failed attempt replaces an old success when publication succeeds. If
publication fails before replacement, the previous complete result remains and
may become stale. Logs are the available signal for that storage failure; the
daemon cannot reliably record the failure into the same broken result store.
The next scheduled attempt is the normal retry opportunity.

## Errors, logging, and shutdown

| Condition | Behavior |
| --- | --- |
| Invalid options, unreadable input directory, unusable results directory, or missing executable at startup | Log clearly and exit nonzero. |
| Input-directory scan fails after startup | Log the failed pass and retry on the next interval. |
| A document cannot be read or DSC fails | Publish a failed attempt and continue to later documents. |
| DSC exits zero but output is unusable or reports errors | Publish a failed attempt; do not claim success. |
| Result publication fails | Log source, destination, and error; continue; retry through later reconciliation. |
| Shutdown is requested | Stop scheduling, cancel active execution, clean up, and exit normally. |

Use `log/slog`, initially with a JSON handler at info level on stderr. Include
stable context such as `config_path`, `result_path`, `duration`, `exit_code`, and
`error`. Log lifecycle events and attempt summaries. Keep per-entry discovery
noise at debug level and log errors where handled rather than at every layer.
DSC output and resource properties may contain secrets: store only in protected
result files and do not mirror them into routine logs.

In foreground mode, `signal.NotifyContext` owns shutdown at the command boundary
(SIGINT/SIGTERM on Linux, Ctrl+C on Windows). SCM Stop and Shutdown controls
cancel the same execution path; the handler reports StartPending, Running and
StopPending with checkpoints while draining. Pass its context
through reconciliation into `exec.CommandContext`; never launch another document
after cancellation. Attempt to publish a canceled result after active execution
has stopped, with a five-second publication context independent of the canceled
execution context. Synchronous filesystem calls cannot be interrupted portably;
the context is checked between operations, not falsely presented as a hard
I/O deadline. The command/service boundary exits nonzero if shutdown has not
finished within 30 seconds. That hard exit can leave an unpublished `.dscd-*`
temporary file, but never publishes partial JSON. Systemd allows 40 seconds
before its final forced cleanup. SCM host shutdown policy can allow less than
30 seconds; publication during forced termination is best-effort.

DSC resources may launch child processes. Process-tree cleanup and inherited
stdout/stderr handles require platform-specific containment. Linux starts each
DSC in a new process group and sends SIGKILL to the group on cancellation and
after the parent exits. Systemd additionally cleans up the whole service cgroup,
including ordinary processes that created a new session. In foreground Linux,
resources deliberately leaving the process group are not contained.

Windows starts DSC suspended, assigns it to a per-attempt Job Object with
`KILL_ON_JOB_CLOSE` and no breakaway permission, then resumes the primary thread.
Cancellation terminates the job and the direct process; closing the job after
completion also removes lingering descendants. If assignment fails, the
suspended process is killed and the attempt fails before resources execute.
Job termination does not reach work delegated to an existing service, WMI or
other broker. Neither platform can undo applied changes, kill kernel-stuck
processes promptly, or provide a security sandbox for privileged resources.

Cancellation is immediate termination, not a graceful resource shutdown.
`exec.Cmd.WaitDelay` is two seconds, bounding waits for inherited output handles
after exit/cancellation. A parent that exits while descendants hold its pipes
can produce an `output` failure; its remaining descendants are then terminated.
Resources are not allowed to leave persistent background children in their DSC
process tree; use an actual service manager for persistent services.

## Packaging and operational limits

Linux/systemd and Windows/SCM are both v1 packaging targets. Put the Linux
unit at `packaging/systemd/dscd.service`, run the daemon in the foreground, collect
stderr through the service manager, and use a deliberate service account with
the permissions required by the managed resources. The supplied unit deliberately
uses root because arbitrary DSC resources may require system modification; change
it to a least-privileged identity when the selected resources permit. Do not daemonize internally
or invoke `sudo` from the daemon. Keep systemd details outside core packages.

The service manager handles restarts and final process cleanup. Provision one
instance for each directory pair. Windows installation/removal scripts are under
`packaging/windows`, default to LocalSystem, quote all executable/path arguments,
and register the `dscd` Application Event Log source. Foreground logs are JSON on
stderr; SCM sends JSON messages to that source (Event ID 1, informational transport;
the JSON `level` records error severity). Logs are not stored in result files.
See [README.md](../README.md) for installation, stop, removal and log access.

The execution timeout defaults to 15 minutes and is configurable, without adding
parallel execution. Capture at most 16 MiB of stdout and 1 MiB of stderr per
attempt. Crossing either limit cancels the process tree and yields an `output`
failure; excess bytes are discarded without unbounded buffering. Truncated
stdout is not parsed or preserved. Stderr retains its prefix plus a truncation
marker. Limits are fixed in v1 rather than adding more operator options. JSON
decoding/serialization adds bounded allocations beyond the captured byte counts;
these limits are not a whole-process memory quota.

## Validation and implementation order

Implement the typed config, DSC process boundary, and result writer first. Add a
testable single pass and then the periodic lifecycle. Wire them in `cmd/dscd`,
then add packaging and operator documentation. Keep the initial dependency set
limited to the Go standard library plus `golang.org/x/sys`, justified by native
Windows SCM, Event Log, process jobs, ACLs and replacement APIs. OS file suffixes
isolate these details; core reconciliation does not branch on operating system.

Tests use temporary directories, small fakes defined against the consumer's
interfaces, and controlled helper processes. Cover sorted discovery, ignored
entries, input changes between passes, continuation after errors, cancellation,
non-overlap, stdout/stderr separation, result classification, JSON compatibility,
filename collisions, failed publication, and complete replacement visibility.
Tests must not require DSC or mutate the host's desired state. Real-DSC tests are
opt-in and must run only in a disposable, explicitly configured environment.

Use the build, test, vet, formatting, and race checks specified in
[AGENTS.md](../AGENTS.md). CI runs them natively on Linux and Windows, plus
opt-in service-registration smoke tests only on disposable hosted runners.
Normal tests cover the SCM handler using in-memory control/status channels;
they do not register a service, reboot a machine, invoke real DSC, or change
host desired state. SCM stop-during-startup, Stop, Shutdown, interrogation,
startup failure and shutdown bounds are tested.

Initial local verification exercised Windows amd64 and Linux amd64 under WSL,
including build/test/vet, race checks, helper-process descendant cleanup,
bounded output, spaces in paths, and concurrent complete-result visibility.
This is not native Linux hardware or power-loss validation. Actual service
installation, host-shutdown delivery and real DSC/resource compatibility must
still be verified in a disposable deployment environment; CI definitions are
not evidence of an already completed CI run.
