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
| DSC client | Own pass-scoped DSC servers, MCP initialization and serial JSON-RPC requests; produce execution results. | No discovery, input reads/hashing or result-file writes. |
| Result writer | Encode the result contract and publish files safely. | No DSC invocation or resource-state interpretation. |

The loop supplies captured configuration/parameter strings, source identity and
a cancellation context to a DSC session, then gives its result to the writer.
Execution and publication have
separate errors. A result remains useful on execution failure: its source
identity, timestamps, and diagnostics must still be populated.

## Configuration and startup

The initial configuration uses the standard `flag` package:

| Flag | Go field | Default | Meaning |
| --- | --- | --- | --- |
| `-config-dir` | `ConfigDir` | Linux: `/etc/dsc/config.d`; Windows: `%ProgramData%\dsc\config.d` | Directory containing DSC documents. |
| `-results-dir` | `ResultsDir` | Linux: `/var/lib/dsc/results.d`; Windows: `%ProgramData%\dsc\results.d` | Directory containing latest execution results. |
| `-interval` | `Interval` | `5m` | Delay after each completed reconciliation pass; must be positive. |
| `-dsc-path` | `DSCPath` | `dsc` | Executable path or name to resolve at startup using PATH (and PATHEXT on Windows). |
| `-execution-timeout` | `ExecutionTimeout` | `15m` | Positive maximum duration of each configuration JSON-RPC request. |

```sh
dscd -config-dir /etc/dsc/config.d -results-dir /var/lib/dsc/results.d -interval 5m -dsc-path dsc
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
not supply this protection. The daemon does not change permissions or ownership
of existing directories. Existing directories must already have trustworthy ACLs; an
untrusted user able to replace directories can defeat path-based protections.
Configuration files must be readable by the service account and writable only
by trusted producers. The Windows MSI provisions and protects its input/output
directories for LocalSystem and administrators without recursively rewriting
unrelated files.

Invalid startup configuration or an unavailable executable causes a clear error
and a nonzero exit. Successful startup leads to an immediate reconciliation pass.

## Discovery and reconciliation

Scan direct children of `ConfigDir`. Ordinary files with case-sensitive `.yaml`
or `.json` extensions are configuration documents; `.dsc.` is not required in
filenames. Ignore directories, symlinks, hidden names, `.yml` files and temporary
suffixes such as `example.yaml.tmp`. Sort configurations by filename using Go
string ordering.

The suffixes `*.parameters.yaml` and `*.parameters.json` are reserved for
parameter sidecars. These files are never independently reconciled, including
orphan sidecars. For each configuration, remove its final extension and look for
`<basename>.parameters.yaml` or `<basename>.parameters.json` in the same directory.
Association uses exact, case-sensitive basenames. Configuration and parameter
serialization formats may differ: `web.yaml` with `web.parameters.json` is valid.

Duplicate parameter formats are an error; there is no precedence. Duplicate
configuration basenames (`web.yaml` and `web.json`) are also an error, even
without a parameter file. Discovery records an input failure for each affected
configuration, naming the conflicting files; none is executed, and unrelated
configurations continue. A selected sidecar that is a directory, symlink or
unreadable file is an input failure, not permission to silently use defaults.

Each pass uses that ordered list and runs one document at a time. Recheck that a
configuration and selected sidecar are still readable regular files before
reading them once into memory; if either disappeared or became unreadable,
record an input failure and continue. This is operational
validation, not a security boundary against a malicious local writer. Trusted
producers must publish using temporary files and replacement, not in-place edits.

`dscd` remains the long-running daemon. Each nonempty pass normally uses one
short-lived `dsc server`, started lazily before its first executable configuration.
All configurations in that pass share its initialized session and are submitted
sequentially. Empty passes, or passes with only invalid/unreadable inputs, start no
server. The session is always closed before the pass returns. DSC is never kept
running while `dscd` waits between passes.

Reconcile immediately on startup. After each pass completes, including all result
publication attempts and server shutdown, wait the full configured interval before starting the next
pass. Pass-level failures use the same delay before retrying. Slow passes never
cause catch-up runs: a 12-minute pass with a five-minute interval starts its next
pass 17 minutes after the previous start. There is exactly one active pass and
one DSC invocation at a time within a daemon instance. Cancellation interrupts
the wait and prevents further passes.

```text
validate startup
run one pass immediately:
    discover and sort configurations
    capture each input, start server if needed, submit sequentially, publish results
    close the server
until canceled:
    wait the full interval after pass completion, or stop on cancellation
    if not canceled: run one pass
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

## DSC server transport and outcome

The compatibility target is **Microsoft DSC 3.3.0**, the stable release with
`dsc server` and `invoke_dsc_config`; a preview release is not required.
Older CLI-only versions are not supported and there is
no per-document CLI fallback. The contract follows DSC's version-tagged
[server tests](https://github.com/PowerShell/DSC/blob/v3.3.0/dsc/tests/dsc_server.tests.ps1)
and [configuration tool implementation](https://github.com/PowerShell/DSC/blob/v3.3.0/dsc/src/server/invoke_dsc_config.rs).
Real DSC execution remains opt-in; other versions require this same protocol.

Start the resolved executable as `dsc server`, without a shell. Stdin/stdout carry
newline-delimited JSON-RPC 2.0. Initialize with request ID 1, protocol version
`2024-11-05`, `capabilities: {"tools": {}}`, and client information
`{"name": "dscd", "version": "dev"}`. Require matching protocol, server information
and tool capabilities, then send `notifications/initialized` without an ID.
Initialization is bounded by the smaller of ten seconds and the execution timeout.

Each configuration sends one `tools/call` request naming `invoke_dsc_config`,
with arguments `operation: "set"`, `configuration: "<captured text>"`, and
`parameters: "<captured text>"` only when a sidecar exists. IDs increase within
each session, starting configuration requests at 2. Only one request is outstanding.
Validate JSON-RPC version, response shape and exact ID. Ignore well-formed
notifications without IDs and tolerate additive fields. Malformed/truncated JSON,
unexpected requests or response shapes, mismatched IDs and pipe failures make
the session unusable.

The nested `response.result.structuredContent.result` object is preserved as
`dscResult`. A well-formed JSON-RPC error or MCP `isError: true` is a configuration
failure, not a transport failure. Its useful diagnostic text is saved in the
protected result but never logged. A valid result with `hadErrors: true` is also
a configuration failure. These outcomes do not restart the server.

On process exit, protocol/pipe failure or request timeout, terminate and reap
the unusable server, publish the current attempt, and start a fresh session for
the next configuration. Restarting inside a pass is fault recovery only; do not
retry the failed input within that pass. Cancellation stops further attempts.
If starting or initializing a session fails, publish `start` failures for the
remaining readable/unambiguous inputs and return one pass-level error. Do not
repeatedly attempt startup for every file; retry on the next pass. Discovery/input
failures still retain their own classification.

Parse only the result envelope needed to assess execution. Require a JSON object
with object-valued `metadata`, array-valued `results` and `messages`, and a boolean
`hadErrors`. Preserve its payload without interpreting individual resource properties
or rejecting unknown fields. Success means a successful RPC with a valid DSC result
and `hadErrors: false`, not a per-operation process exit or permanent compliance.

Configuration and parameter contents remain opaque to `dscd`. DSC owns parsing,
validation, resource discovery, parameter interpretation, defaults, substitution,
secure values, testing and configuration execution. The daemon does
not parse YAML/JSON inputs, merge documents or parameters, interpolate values,
reinterpret resource identities, or infer dependencies between configurations.
Two documents that manage conflicting state may continually undo each other;
document authors must resolve that conflict.

**Parameter-content compatibility:** this server tool expects a direct parameter
mapping, for example `message: Hello` or `{"message":"Hello"}`, not the CLI
parameter-file envelope `{"parameters": {...}}`. The daemon sends sidecar text
unchanged and does not unwrap, merge or translate it. Existing wrapped sidecars
must be updated by their producers. Empty sidecars are sent as empty strings,
not silently omitted; DSC decides whether they are valid.

The reconciliation input identity includes both configuration and parameter
content when a sidecar is present. Read each file once per attempt into immutable
strings, compute the hash from those strings, and submit those exact same strings.
No input pathname is passed to DSC and DSC never reopens these files. JSON escaping
does not change the string bytes after decoding: **bytes hashed equal bytes
submitted**. Reject invalid UTF-8 rather than allowing JSON encoding to replace
bytes. `inputHash` is `sha256:` followed
by the lowercase hexadecimal SHA-256 of the concatenation of
`"dscd-input-v1\0"`, the 32-byte configuration digest, a one-byte sidecar-present
marker (0 or 1), and, when present, the 32-byte parameter digest. This framing
distinguishes missing from empty sidecars and keeps file boundaries unambiguous.
Names are recorded separately; changing either file's bytes changes the hash.
The hash identifies the exact desired-state input submitted for an attempt,
not a skip/reconciliation cache key. For input/start failures it may describe a
captured input that could not be submitted. Replacing a source file after capture
cannot change that attempt. Reading a pair is not an atomic producer transaction;
trusted producers must publish complete files and coordinate paired updates.
Inline submission also provides no source-path `DSC_CONFIG_ROOT` semantics.
Documents must not rely on the old CLI's implicit file-root context.

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
| `parameters` | optional string | Selected parameter-sidecar basename; omitted when no unique sidecar was selected. Never contains parameter values. |
| `inputHash` | optional string | Combined SHA-256 of the captured strings submitted to DSC; omitted when capture fails. Start failures can retain a hash of input not submitted. |
| `startedAt` | string | UTC RFC 3339 timestamp with fractional seconds as needed. |
| `finishedAt` | string | UTC timestamp in the same format. |
| `durationMs` | integer | Nonnegative elapsed milliseconds, measured with a monotonic clock. |
| `outcome` | string | `succeeded`, `failed`, or `canceled`. |
| `exitCode` | integer or null | Actual server exit code, if available after a fatal request failure/termination. Null for RPC success and ordinary DSC errors; never synthesized as zero. |
| `dscResult` | object or null | Preserved JSON object from DSC, or null when unavailable/unusable. |
| `error` | object or null | Failure classification and diagnostic message; null on success. |
| `stderr` | string | Empty: shared server stderr is drained and discarded, not attributed to individual attempts. |

An error object contains `kind` and `message`. Initial kinds are `input`, `start`,
`exit`, `output`, `dsc`, and `canceled`. Cancellation/deadline expiry is `canceled`;
snapshot failures are `input`, startup/handshake failures `start`, unexpected server
EOF/exit `exit`, framing/ID/shape/pipe failures `output`, and valid RPC/tool/DSC
errors `dsc`.
An execution deadline is classified as `canceled` with a deadline diagnostic, but
does not cancel the daemon or stop later documents. An internal output-limit
termination is `output`, not `canceled` or the induced nonzero `exit`. Initialization
deadline expiry is also `canceled`. Human-readable error
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

The ten original fields are always present, including explicit nulls. The additive
`parameters` and `inputHash` fields retain schema version 1 and are present when
available. The daemon records its
own envelope timestamps and duration; it preserves DSC's metadata separately in
`dscResult`. Do not synthesize successful output. Invalid stdout produces an
`output` error; it is not embedded as a fabricated DSC JSON object or dumped into
routine logs.

Raw input contents are never copied into daemon metadata or logs. DSC or a
resource may echo values in `dscResult` or JSON-RPC error text. Protected result
files can therefore contain those values; this feature does not introduce output
redaction or secret handling. Shared stderr cannot be reliably partitioned into
attempts, so it is continuously drained to a discard sink with fixed buffering,
never retained, published or logged. The result's `stderr` field remains present
but empty.

Schema version 1 is retained: names, outcome classifications, nullable exit codes
and the nested DSC payload keep their roles. With server transport, consumers
must use `outcome`/`hadErrors`, not require `exitCode == 0` on success or expect
per-attempt stderr. The hash framing is unchanged while its capture-to-submission
guarantee is stronger.

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
| DSC returns unusable RPC output or reports errors | Publish a failed attempt; restart only an unusable session. |
| Server startup/initialization fails | Publish remaining input failures as described above, log a pass-level error, retry next pass. |
| Result publication fails | Log source, destination, and error; continue; retry through later reconciliation. |
| Shutdown is requested | Stop scheduling, cancel active execution, clean up, and exit normally. |

Use `log/slog`, initially with a JSON handler at info level on stderr. Include
stable context such as `config_path`, `result_path`, `duration`, `exit_code`, and
`error_kind`. Log lifecycle events and attempt summaries. Keep per-entry discovery
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

Windows starts DSC suspended, assigns it to a per-session Job Object with
`KILL_ON_JOB_CLOSE` and no breakaway permission, then resumes the primary thread.
Cancellation terminates the job and the direct process; closing the job after
completion also removes lingering descendants. If assignment fails, the
suspended process is killed and the attempt fails before resources execute.
Job termination does not reach work delegated to an existing service, WMI or
other broker. Neither platform can undo applied changes, kill kernel-stuck
processes promptly, or provide a security sandbox for privileged resources.

At normal pass completion, close server stdin, allow two seconds for EOF-driven
exit, and wait/reap it. If it does not exit, terminate the contained process tree
and report the forced shutdown as a pass-level error. Cancellation and fatal
request failures terminate immediately. Owned pipe readers/writers are closed
and joined during cleanup, so inherited resource handles cannot retain the
session between passes. Remaining resource descendants are terminated when
the session closes. Resources needing persistent services must use a service
manager rather than leaving background children in the DSC process tree.

## Packaging and operational limits

Linux/systemd and Windows/SCM are both v1 packaging targets. Put the Linux
unit at `packaging/systemd/dscd.service`, run the daemon in the foreground, collect
stderr through the service manager, and use a deliberate service account with
the permissions required by the managed resources. The supplied unit deliberately
uses root because arbitrary DSC resources may require system modification; change
it to a least-privileged identity when the selected resources permit. Do not daemonize internally
or invoke `sudo` from the daemon. Keep systemd details outside core packages.

Linux AMD64 native DEB and RPM packages use shared nFPM definitions under
`packaging/linux`. They install `/usr/bin/dscd` and the vendor unit at
`/usr/lib/systemd/system/dscd.service`, with `/usr/bin/dsc` supplied by Microsoft's
independent `dsc` package. The declared minimum dependency is `dsc >= 3.3.0`;
the daemon packages neither bundle DSC nor configure Microsoft repositories.
Go and packaging tools are build-time requirements only.

Installation provisions missing `/etc/dsc/config.d` and
`/var/lib/dsc/results.d` directories as root:root `0700`, without recursively
changing existing permissions. It reloads systemd and enables/starts `dscd`
when systemd is running. Upgrades replace package files and restart only a
previously running service. Removal stops/disables the service and retains
administrator documents and results, including during DEB purge; shared parent
directories and DSC-managed machine state are never removed by scripts.
Retain DSC on DNF removal with `--noautoremove` if it was dependency-installed.
Image construction without running systemd skips runtime service operations
while recording enablement for the image.
Publish trusted documents into the input directory; a restart is not needed.
The first pass is immediate and the default wait after each completed pass is
five minutes. Use systemd drop-ins for local service customizations rather than
editing the package-owned unit.

The service manager handles restarts and final process cleanup. Provision one
instance for each directory pair. The Windows MSI is under
`packaging/windows/msi`, runs as LocalSystem, quotes all executable/path arguments,
and register the `dscd` Application Event Log source. Foreground logs are JSON on
stderr; SCM sends JSON messages to that source (Event ID 1, informational transport;
the JSON `level` records error severity). Logs are not stored in result files.
See [README.md](../README.md) for installation, stop, removal and log access.

Release candidates are built from a single selected revision by the manually
triggered `release.yaml` workflow. Native Linux and Windows amd64 jobs test,
build and package the daemon with platform service assets and the MIT license.
Only after both jobs succeed does the workflow tag that revision and create a
draft pre-release with both archives, the Windows MSI and SHA-256 checksums
covering every release artifact. Publishing remains
an explicit review step; release tags are not reused. DSC and its resources are
separate prerequisites, not bundled release dependencies.
The Linux job also builds and inspects native packages; checksums cover both
archives, both Linux packages and the Windows MSI. Release tag `vX.Y.Z-rc.N` maps to native version
`X.Y.Z~rc.N` (RPM release `1`), sorting before stable `X.Y.Z`.
Native packages contain the executable, unit and license, not development
sources. Linux ARM64 packages and package feeds are not part of this release.

### Windows MSI

The recommended Windows x64 installation is a per-machine WiX MSI, not a
PowerShell service installer. It uses Windows Installer's standard directory
properties: `[ProgramFiles64Folder]dscd\dscd.exe`,
`[CommonAppDataFolder]dsc\config.d`, and
`[CommonAppDataFolder]dsc\results.d`. The executable inherits Program Files'
administrator-controlled permissions. The `dsc` data root and both directories
have protected inheritable DACLs permitting only SYSTEM and Administrators full access; no
recursive permission changes to unrelated content are performed.
The directories remain after uninstall, including when empty.

Install Microsoft DSC 3.3.0 or later separately. Microsoft's
[Windows installation documentation](https://learn.microsoft.com/en-us/powershell/dsc/install?view=dsc-3.0)
and [3.3.0 release assets](https://github.com/PowerShell/DSC/releases/tag/v3.3.0)
provide ZIP distributions as well as other installation methods. Add the DSC
directory to the **machine/system PATH visible to LocalSystem** before installing
or starting dscd. For example, extract the archive to `%ProgramFiles%\DSC` and add
that directory to the system PATH. This is an operator choice, not an installer
search location. An interactive user's PATH or per-user WinGet/Store executable
alias is insufficient. Windows services may retain an older environment after
PATH changes; reboot before installation/startup if needed. Operators must verify
DSC 3.3.0+ and LocalSystem access to DSC/resources and protect PATH directories
and resource files against untrusted writes.

The MSI does not accept a DSC executable path, discover or validate DSC, persist
its location, or modify PATH. It neither bundles nor downloads DSC. The daemon's
existing startup validation resolves the default executable name `dsc` through
the service process's PATH/PATHEXT. If resolution fails, the service fails to
start and installation fails through native MSI service-start handling; there
is no custom prerequisite launch condition or tailored MSI discovery error.
An installation without DSC exceeded the smoke test's two-minute limit in native
`StartServices`; do not depend on a prompt prerequisite rejection or bounded
rollback time. Provision and verify the service environment before installation.
Service, registry, directory and ACL operations remain declarative.

Native service tables own `dscd`, display name `DSC Reconciliation Daemon`,
automatic startup and LocalSystem identity. The command line supplies only
`-config-dir` and `-results-dir`; the daemon's default `dsc` name, five-minute
interval and fifteen-minute execution timeout remain authoritative.
Reconciliation starts immediately, then waits five minutes after each completed
pass. Deploying trusted configurations does not require a service restart.
Service recovery restarts after five seconds and resets its failure count after
one day. WiX's rollback-aware Util service configuration sets recovery actions;
the native MSI service configuration sets recovery for nonzero service exits
as well. Microsoft documents that this latter flag takes effect at the next
system start (also applicable to `sc failureflag`) and that
`MsiConfigureServices` has reliability limitations. Its specific WiX warning
is suppressed; immediate non-crash recovery is not claimed. The smoke
test checks the flag and actual recovery after forced process termination.
Event Log registration is package-owned under HKLM, uses the existing
Application source `dscd` and preserves Event ID 1 / JSON logging.

Major upgrades use a stable UpgradeCode and a new ProductCode per package
version, stop the service before replacement and restart it afterward.
DSC must remain available on the service's machine PATH during upgrades and
repair. Installation is transactional where Windows Installer
supports rollback. No migration or alternate legacy layouts are supported.
Uninstall stops and removes the service, executable and Event Log registration,
but leaves data directories, documents, results, DSC and reconciled machine
state intact. Ordinary MSI repair restores package-owned resources and never
ships or replaces configuration documents.

Release tags map deterministically to MSI's three-field ProductVersion:
`vM.m.p-rc.N` becomes `M.m.(100*p+N)`, and `vM.m.p` becomes
`M.m.(100*p+99)`. Major and minor must be 0..255, patch 0..654, and candidate
number 1..98. Unsupported suffixes, leading zeros and out-of-range fields fail
the build rather than colliding. For example, `v0.0.1-rc.1` is `0.0.101`,
`v0.0.1-rc.2` is `0.0.102`, and `v0.0.1` is `0.0.199`.
This preserves ordering into the next patch/minor/major; publish intended
upgrades in increasing release order. The full tag remains in artifact names
and human-readable package metadata. Release workflows still create drafts
marked pre-release, even when a stable-form tag is selected.

The WiX SDK and Util extension versions are pinned only in `dscd.wixproj`;
no Go source compilation happens inside the installer project. The packaging
build accepts a previously built executable.
`build.ps1` maps the release version and invokes `dotnet build`, including its
normal dependency restore. No native helper, Visual C++ tools, developer shell
or Windows SDK build dependency is required. The .NET SDK and PowerShell are
build tools only. CI's MinGW compiler remains solely for Go race checks.
Go's unversioned PE receives release-version and language-neutral MSI file
metadata to ensure upgrade replacement; that intentional override's warning
is also suppressed. All other WiX warnings fail the build.
Separate build-time PowerShell scripts validate the version and inspect Windows Installer
tables for x64 architecture, metadata, paths, payload, service settings,
recovery, Event Log registration, directory ACLs and the absence of DSC discovery,
path persistence and PATH modification. Each packaging workflow invokes
`inspect.ps1` after `build.ps1`. PowerShell, Go, .NET and WiX
are not target-machine prerequisites. Review WiX's
[maintenance fee terms](https://docs.firegiant.com/wix/osmf/) before building.
The project records acceptance for automated builds with
`<AcceptEula>wix7</AcceptEula>`; WiX enforces EULA acceptance without a custom
script switch or guard.
Ordinary CI and release builds inspect the MSI but never install it.
The manual `service-integration.yaml` workflow installs it on a disposable
Windows runner with DSC 3.3.0 visible through the machine PATH and an
empty input directory, verifies SCM, ACLs and Event Log behavior, cycles the
service and uninstalls while checking data retention. MSI logs are retained as
workflow artifacts. Defining those checks is not evidence they have run;
actual install/upgrade/shutdown behavior must be reported separately.
The runner exposes the extracted DSC executable through a symlink in an existing
machine PATH directory containing spaces, avoiding reliance on a newly edited
PATH reaching the already-running SCM. This is disposable test setup only;
the MSI does not create that link or change the environment. The test also hides
the fixture temporarily to verify a direct service restart fails with a DSC
resolution error, then restores it before continuing the MSI lifecycle checks.
The previous installer with explicit DSC paths passed native MSI checks in
[service-integration run 37578288245](https://github.com/Bpoe/dsc-reconciler/actions/runs/37578288245),
including executable replacement, repair, reinstall, uninstall and data retention.
That historical run does not validate the current PATH-based installer. The same
run passed Linux systemd integration. Host-shutdown delivery and full draft-release
execution were not tested by that workflow.

The execution timeout defaults to 15 minutes and covers one configuration request,
including a blocked stdin write or waiting for its response. A timed-out request
is `canceled` and its session is destroyed before later documents continue.
Capture at most 16 MiB of combined configuration/parameter UTF-8 input per attempt.
Each newline-delimited stdout frame is limited to 16 MiB; exceeding that limit is
an `output` failure and destroys the session. Notifications do not accumulate.
Shared stderr is continuously discarded using fixed buffering. JSON framing and
decoding add bounded allocations beyond these byte limits; they are not a
whole-process memory quota. These fixed limits avoid extra operator flags.

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
[AGENTS.md](../AGENTS.md). `ci.yaml` runs these checks natively on Linux and Windows
for pushes and pull requests. Service-registration smoke tests run separately
through `service-integration.yaml`, triggered manually with `workflow_dispatch`,
only on disposable hosted runners.
The Linux integration job installs Microsoft's DSC package and exercises native
DEB install, stop/start, running/stopped upgrades, reinstall, removal and purge
with real systemd. A separate Fedora image exercises RPM installation/lifecycle
without a systemd manager; it does not certify RPM service startup.
Normal tests cover the SCM handler using in-memory control/status channels;
they do not register a service, reboot a machine, invoke real DSC, or change
host desired state. SCM stop-during-startup, Stop, Shutdown, interrogation,
startup failure and shutdown bounds are tested.

Initial local verification exercised Windows amd64 and Linux amd64 under WSL,
including build/test/vet, race checks, helper-process descendant cleanup,
bounded output, spaces in paths, and concurrent complete-result visibility.
An opt-in Windows test against DSC `3.3.0` verified MCP initialization,
EOF shutdown, Echo defaults and inline parameter overrides for all six
configuration/parameter-format combinations in one shared server session.
This is not native Linux hardware, general DSC resource or power-loss validation.
Native DEB/RPM build and artifact inspection passed locally, including ownership,
permissions, unit verification and prerelease ordering. Disposable Ubuntu/Fedora
containers with DSC 3.3.0 exercised package lifecycle and data preservation
without running systemd, including DEB purge. This does not establish service
startup or real DSC resource execution.
The DEB lifecycle additionally passed on a disposable Ubuntu systemd host,
including enable/start, stop/start, running/stopped/disabled upgrades, reinstall,
remove/reinstall, removal/purge and data/permission preservation. Native RPM
systemd startup, Windows service registration, host-shutdown delivery and other
DSC/resource compatibility still need disposable deployment validation.
CI definitions are not evidence of an already completed CI run.
