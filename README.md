# dsc-reconciler

`dsc-reconciler` is a lightweight local reconciliation daemon for DSC. The `dscd`
process continuously discovers DSC configuration documents in a configured directory,
evaluates them against the local machine, applies any required changes by default,
and records the results. Optional per-configuration metadata can select audit-only
`test` execution instead.

The project is intentionally simple. `dscd` does not own configuration authoring,
composition, or parameter processing; it inspects only document syntax and its
embedded metadata, delegating resource interpretation and execution to DSC. This
makes it useful both as a standalone local desired-state reconciler and as a building block
for higher-level configuration control planes.

There is no network server, remote configuration store or DSC resource model.
Filesystem notifications trigger targeted reconciliation; periodic full passes
remain the correctness mechanism when notifications are missed or unavailable.
`dscd` extracts only its own metadata; DSC owns testing and applying
configurations. See the
[design and result contract](docs/design.md) and [contributor guidance](AGENTS.md).

## Usage

### Installation
Download the `.deb`, `.rpm`, or `.msi` for your platform from
[GitHub Releases](https://github.com/Bpoe/dsc-reconciler/releases).
Use the filename for your chosen release in the examples below.
The native packages register the service, start it, and enable it to run at boot.

#### Linux
Install Microsoft's `dsc` package first, then install the daemon package using
the command for your distribution:

```sh
# Debian / Ubuntu
sudo apt install ./dscd_0.0.2_amd64.deb

# Fedora / RHEL-compatible systems
sudo dnf install ./dscd-0.0.2.x86_64.rpm
```

#### Windows
Before installing `dscd`, ensure `dsc.exe` is on the **machine/system PATH**
visible to LocalSystem. A per-user executable alias or PATH change in your
terminal is not sufficient; a reboot may be needed after changing the system PATH.

Double-click the downloaded MSI and approve elevation. It installs and starts
the **dscd** service (**DSC Reconciliation Daemon**). The MSI does not install DSC
or change PATH.

### Check the version

Run `dscd --version` (or `.\dscd.exe --version` from its installation directory
on Windows). It prints `dscd <version>` to stdout and exits without requiring DSC
or configuration directories. Release binaries report the full release tag,
such as `dscd v0.0.2`; unstamped source builds report `dscd dev`.

### Add configurations

As an administrator, publish your DSC `.yaml` or `.json` documents directly in
the input directory created by the installer:
- **Linux:** `/etc/dsc/config.d`
- **Windows:** `%ProgramData%\dsc\config.d`

**Adding a configuration authorizes DSC to change the machine by default.**
For audit-only operation, include `metadata.dscd.operation: test` in the
configuration itself. For example, publish `web.yaml`:

```yaml
$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
metadata:
  dscd:
    operation: test
resources:
  - name: Example
    type: Microsoft.DSC.Debug/Echo
    properties:
      output: Hello
```
To enforce desired state instead, use `operation: set` or omit the metadata.
JSON supports the same [embedded metadata](#per-configuration-metadata).

Changes trigger reconciliation of only the affected configuration as soon as the
daemon's current work finishes. For a **new configuration/parameter pair**, publish
the parameter file first, then atomically rename the complete configuration into
place. Orphan parameters do not execute anything; the configuration's arrival
uses the parameters already present. There is no write-correlation delay.
See [filesystem-triggered reconciliation](#filesystem-triggered-reconciliation).

### Read results
Each configuration has a latest-result file. For `web.yaml`, read:

Linux:
```sh
sudo cat /var/lib/dsc/results.d/web.yaml.result.json
```

Windows, in an elevated PowerShell session:
```powershell
Get-Content "$env:ProgramData\dsc\results.d\web.yaml.result.json" -Raw |
    ConvertFrom-Json | Format-List
```

# Development

## Prerequisites and compatibility

- Go **1.27.x** to build; no Go installation is needed to run the compiled binary.
- Microsoft **DSC 3.3.0 or later** with `dsc server` and the resources your documents
  require, installed separately for the account running the daemon. For the Windows
  MSI service, `dsc.exe` must be on the machine/system PATH visible to LocalSystem.
  This is modern DSC, not Windows
  PowerShell's `Start-DscConfiguration`.
- Linux with a local filesystem supporting atomic rename and directory fsync
  (systemd for service installation), or Windows 11 / Windows Server 2022 or newer
  with local NTFS. macOS, network shares and FAT are not supported.

Install DSC from the [official release](https://github.com/PowerShell/DSC/releases/tag/v3.3.0).
The stdio protocol follows DSC's version-tagged
[server tests](https://github.com/PowerShell/DSC/blob/v3.3.0/dsc/tests/dsc_server.tests.ps1)
and [configuration tool](https://github.com/PowerShell/DSC/blob/v3.3.0/dsc/src/server/invoke_dsc_config.rs).
Server mode is available in stable DSC 3.3.0; a preview release is not required.
An opt-in Echo test exercised 3.3.0 on Windows; this does not certify all
resources. Older CLI-only DSC versions, including `3.2.0-preview.14`, are no longer
supported. There is no per-document process fallback.

`golang.org/x/sys` provides Windows Job Objects, SCM, Event Log, ACLs and native
file replacement. `go.yaml.in/yaml/v3` provides YAML syntax parsing and embedded
metadata extraction without modeling DSC resources; JSON uses the standard
library's `encoding/json`.
`github.com/fsnotify/fsnotify` (pinned to v1.10.1) supplies directory notifications
using Linux inotify and Windows ReadDirectoryChangesW.

## Build and check

From the module root, on either platform:

```text
go mod verify
go build ./...
go test ./...
go vet ./...
gofmt -l .
go test -race ./...
```

Race checks require a compatible C compiler (GCC/Clang on Linux, modern
MinGW-w64 on Windows); the production binary does not require one.
Formatting output must be empty: `gofmt -l` does not fail just because it lists
files. Format edits with `gofmt -w cmd internal`.

Linux / Make:

```sh
make check
make race
# make build also produces bin/dscd
go build -o bin/dscd ./cmd/dscd
```

Windows / PowerShell (Make is not needed):

```powershell
go build -o .\bin\dscd.exe .\cmd\dscd
if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
$unformatted = @(gofmt -l .)
if ($LASTEXITCODE -ne 0 -or $unformatted.Count) { throw 'Formatting check failed' }
```

To embed a version in a source build, add
`-ldflags "-X main.version=v0.0.2"` to `go build`. Both platform packaging scripts
stamp the version automatically and build one executable for that platform's
archive and native packages.

Normal tests use temporary files, fakes and controlled helper executables, not
DSC. They do not install services or change machine configuration.
[CI](.github/workflows/ci.yaml) runs module verification and native
build/test/vet/format/race checks on both OSes for branch pushes and pull requests,
plus Windows MSI and Linux package build/inspection without installation.
Release workflows call this same CI before mandatory installation tests.
To run those tests independently on disposable runners, select
**Actions > service-integration > Run workflow**, choose a branch, and enter a
test-only stable version such as `v0.0.3`. This builds that ref with the shared
packaging scripts; it does not create a tag or release.
`packaging\windows\test-service.ps1` is a separate, **administrator-only,
opt-in** SCM integration check; never run it on a production machine.

To explicitly run the real DSC server tests, install `Microsoft.DSC.Debug/Echo`
and set `DSCD_TEST_DSC_PATH` for that test process:

```powershell
$env:DSCD_TEST_DSC_PATH = 'C:\bin\dsc\dsc.exe' # Adjust to your DSC 3.3.0 installation.
go test ./internal/dsc -run TestRealDSCParameterFiles -count=1 -v
Remove-Item Env:\DSCD_TEST_DSC_PATH
```

Without that variable, normal tests skip real DSC execution.

### Build Linux packages

On Linux AMD64, install nFPM **v2.47.0** as a build tool (not a daemon dependency)
and ensure `dpkg-deb`, `rpm`, `rpm2archive` and `systemd-analyze` are available
(Ubuntu's `rpm` package supplies `rpm2archive` via its dependencies):

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0
# Ensure Go's bin directory is on PATH.
VERSION=v0.0.3 bash packaging/linux/build.sh dist
VERSION=v0.0.3 bash packaging/linux/validate.sh dist
```

The builder produces TAR.GZ, DEB, RPM and checksums. Validation extracts packages
without installing them and checks that they contain the archive's executable.

### Build Windows packages

On native Windows with Go, PowerShell 7 and the .NET SDK:

```powershell
.\packaging\windows\build.ps1 -Version v0.0.3 -OutputDirectory .\dist
.\packaging\windows\msi\inspect.ps1 -Version v0.0.3 -MsiPath .\dist\dscd-v0.0.3-windows-amd64.msi
```

The builder produces ZIP, MSI and checksums without installing the service.
WiX is pinned in its project; its native ICE validation must be permitted by
the build machine's policy. Make is not required.

## Releases

Push a new stable `vX.Y.Z` tag on a reviewed commit already in `main`:

```sh
git fetch origin
git tag v0.0.3 <reviewed-commit>
git push origin refs/tags/v0.0.3
```

The [release workflow](.github/workflows/release.yaml) derives the version from
the tag and checks out its exact commit. It rejects unsupported versions,
commits outside `main`, and existing releases (including drafts). RC tags are
not supported. MSI limits are major/minor 0..255 and patch 0..654; the existing
stable mapping `M.m.(100*p+99)` is unchanged.

Release calls CI for native checks, race tests, and package inspection, then
calls [service integration](.github/workflows/service-integration.yaml). CI
builds the release payload once per platform and uploads it with SHA-256
checksums. Integration downloads those exact files from the invoking release
run; it never rebuilds them. Windows and Ubuntu test fresh installation,
real DSC Echo reconciliation, service lifecycle and uninstall/data retention.
Fedora tests RPM lifecycle without systemd, not service startup.
Failed checks prevent draft creation. Failure diagnostics are uploaded as
`windows-install-logs` and `linux-install-logs`.

Automated upgrade testing is deferred. Existing scripts still support explicit
upgrade packages on disposable machines; workflows do not manufacture old
versions or discover/download historical releases. For example:

```sh
sudo bash packaging/linux/test-install.sh ./older.deb ./newer.deb
```

```powershell
.\packaging\windows\msi\test-install.ps1 -Disposable -MsiPath .\older.msi -UpgradeMsiPath .\newer.msi
```

These explicit tests require the same separately provisioned DSC prerequisite.
Installation tests are destructive and disposable-only.

The draft contains (for Linux AMD64 and Windows AMD64):

- `dscd-v0.0.3-linux-amd64.tar.gz`
- `dscd_0.0.3_amd64.deb`
- `dscd-0.0.3-1.x86_64.rpm`
- `dscd-v0.0.3-windows-amd64.zip`
- `dscd-v0.0.3-windows-amd64.msi` (recommended Windows installation)
- `SHA256SUMS` covering all release artifacts

Each archive has a versioned root containing `bin/dscd` or `bin/dscd.exe`,
platform-specific `packaging` assets, the README, design and agent documentation,
and the MIT license. Use the native Linux packages for systemd installation, or
extract an archive for foreground execution. Use the MSI for Windows service installation.
Building from source is not required. DSC and resources are not bundled.
On Linux, verify downloads with `sha256sum --check --ignore-missing SHA256SUMS`.
On Windows, use `Get-FileHash -Algorithm SHA256` and compare the archive's
hash with its entry in `SHA256SUMS`.

After all checks pass, the final job downloads the same artifacts, verifies
their checksums again, and combines the two checksum lists into `SHA256SUMS`.
It creates a **draft normal release**, not a prerelease. Review its assets,
version/source commit, workflow results, and documented coverage in **Releases**,
then publish manually. The workflow never creates/moves a tag or publishes a
release automatically.

### Failures and retries

For build/inspection failures, inspect the CI job logs; for installation failures,
download the diagnostic artifacts and check the separate DSC prerequisite and
service environment. Package checks must not be bypassed to produce a draft.

Re-run failed integration or draft jobs while their original artifacts remain
available. If rebuilding is necessary, use **Re-run all jobs** so new artifacts
also pass integration; the native artifact upload replaces that run's old
platform artifacts. Never selectively rebuild after testing and then skip
integration. If artifacts have expired, rerun the whole workflow.

An existing draft/release stops the run rather than being overwritten. If draft
creation partially failed, inspect it and the uploaded assets manually; remove
an incomplete draft only after review before retrying the same unchanged tag.
For source fixes, merge the fix and push a new version tag. Never move a release
tag. Action SHA pinning is used; attestations, SBOMs, CodeQL and automatic
upgrade-baseline selection remain separate follow-ups.

## Foreground execution

Start with an empty directory if you only want to inspect startup/shutdown.
**Omitted `metadata.dscd.operation` means `set`: adding a DSC document normally
authorizes DSC to enforce its desired state and modify the machine.** Use explicit
`test` metadata for audit-only execution.

Linux:

```sh
mkdir -p ./local-config
./bin/dscd -config-dir ./local-config -results-dir ./local-results \
  -dsc-path /absolute/path/to/dsc -interval 5m -execution-timeout 15m
```

Windows:

```powershell
New-Item -ItemType Directory -Force .\local-config | Out-Null
.\bin\dscd.exe -config-dir .\local-config -results-dir .\local-results `
    -dsc-path 'C:\Program Files\DSC\dsc.exe' -interval 5m -execution-timeout 15m
```

Adjust the executable path to your installation. Paths with spaces are supported.
Logs are structured JSON on stderr. Ctrl+C stops foreground execution; Linux
also handles SIGTERM. Startup errors exit nonzero. A handled stop exits zero;
exceeding the 30-second shutdown bound exits nonzero.

## Configuration

| Flag | Default | Meaning |
| --- | --- | --- |
| `--version` | `false` | Print the dscd build version to stdout and exit without daemon startup. |
| `-config-dir` | Linux `/etc/dsc/config.d`; Windows `%ProgramData%\dsc\config.d` | Existing readable input directory. |
| `-results-dir` | Linux `/var/lib/dsc/results.d`; Windows `%ProgramData%\dsc\results.d` | Results directory, privately created if absent. |
| `-dsc-path` | `dsc` | Executable path or name, resolved once at startup through PATH/PATHEXT. |
| `-interval` | `5m` | Positive delay after each completed full reconciliation pass. Targeted work does not reset it. |
| `-execution-timeout` | `15m` | Positive per-configuration JSON-RPC request timeout. Later documents continue on a fresh server after a timeout. |

Use `-help` for flags. There are no subcommands, daemon configuration files or
daemon environment overrides. The OS `ProgramData` variable selects Windows'
standard data root, with `C:\ProgramData` as fallback. Input and output directories
must be separate and non-nested; aliases through existing symlinks are resolved.
Only one daemon instance may own a directory pair; no multi-process lock is provided.

Discovery is nonrecursive. Ordinary files ending in case-sensitive `.yaml` or
`.json` are eligible; `.dsc.` is not required in filenames. Hidden names,
directories, symlinks, `.yml` files, uppercase extensions and temporary suffixes
such as `.yaml.tmp` are ignored. Go string ordering puts
`10-a.yaml` before `20-b.yaml`. Each pass rediscovers documents; unchanged files
are executed with their effective operation because machine state may drift.
Reconcile all configurations immediately on startup, then wait the full interval
after each completed full pass before starting the next.
Passes never overlap, and slow passes do not cause catch-up runs.

Each pass normally starts **one short-lived `dsc server`**, initializes its
MCP/JSON-RPC stdio session, and uses `invoke_dsc_config` to execute each configuration
sequentially. Different configurations can use `set` and `test` in the same
session. The server is closed before the pass ends; DSC is not kept alive
during the interval. Empty directories start no server. Input errors do not
require starting a server.

Ordinary DSC errors fail that input but retain the same session. Server crashes,
timeouts and invalid protocol responses destroy the current session; the next
configuration starts a fresh one. This restart is fault recovery, not a retry of
the failed input. Server startup/initialization failure publishes failures for
remaining readable inputs, reports a pass-level error and retries next pass.

`*.parameters.yaml` and `*.parameters.json` are reserved parameter sidecars,
never standalone configurations. Match by the configuration filename
without its final extension:

```text
base.yaml       + base.parameters.yaml
security.json   (no sidecar)
web.yaml        + web.parameters.json
```

Names match exactly, case-sensitively; formats may differ. Both parameter formats
for one basename are an error, not a precedence choice. `web.yaml` and `web.json`
together are also invalid, with or without a sidecar. Affected configurations
receive failed input results and unrelated configurations continue. Orphan
sidecars are ignored. Missing/unreadable or nonregular selected sidecars fail
rather than silently falling back to parameter defaults.

### Filesystem-triggered reconciliation

Watching is enabled automatically on Linux and Windows. Creating, modifying, or
atomically replacing a configuration triggers only that configuration. Creating,
modifying, or replacing a parameter sidecar triggers its associated configuration
if present. The same discovery, ambiguity checks, metadata extraction, input
snapshots, DSC execution, and result publication apply to both targeted and full
passes. Unrelated configurations are not executed or republished.

The daemon watches the directory, not individual files, so temporary-file rename
and replacement patterns continue to work. Hidden/temporary names, unsupported
extensions, unrelated files, and attribute-only changes are ignored. Deletion
does not trigger an immediate attempt; periodic discovery observes removals.
Historical results are retained. An orphan sidecar does not wait in a pairing
queue for a future configuration.

All work is serial, including DSC startup, publication, and session cleanup.
Targeted work never resets the full-pass timer. If a full pass becomes due while
targeted work is running, it runs after that work finishes and before more
targeted attempts. Each targeted dispatch uses a short-lived session; no DSC
server is retained while waiting. Pending notifications for the same
configuration coalesce without a debounce delay. A notification during an
attempt can cause a follow-up; exactly one execution per logical save is not
guaranteed.

The collector continues draining notifications while DSC is busy. It records
pending names and uses a nonblocking wake-up signal; watcher failure notification
also never waits for the scheduler. Watcher errors, overflow, or lost watches
are logged but do not stop periodic reconciliation. At every scheduled full
pass, the daemon checks the directory's filesystem identity even if no watcher
error was reported. It retires stale watches and attempts to establish a fresh
watch before scanning. Unavailable directories are retried at later full passes,
without a separate retry timer or parent-directory watch.

For a new pair, publish complete parameters first and then publish the
configuration. Updating parameters for an **existing** configuration intentionally
triggers execution immediately; a subsequent configuration update can trigger
another attempt. This is not an atomic multi-file transaction. Publish via
temporary-file replacement rather than in-place editing, which can expose
incomplete input. Periodic full passes still execute unchanged files to correct
machine drift and recover missed events.

### Per-configuration metadata

The optional `metadata.dscd` object inside a `.yaml` or `.json` configuration
controls its execution policy. The [YAML example above](#add-configurations) and
this JSON configuration select the same audit-only operation:

```json
{
  "$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
  "metadata": {
    "dscd": {
      "operation": "test"
    }
  },
  "resources": [
    {
      "name": "Example",
      "type": "Microsoft.DSC.Debug/Echo",
      "properties": {
        "output": "Hello"
      }
    }
  ]
}
```

Only `operation` has behavior today. It accepts exactly `"set"` (enforce desired
state) or `"test"` (audit only), with case-sensitive property names and values.
Missing `metadata`, missing `dscd`, empty objects, or an omitted `operation`
mean **`set`**. Unrelated metadata namespaces (including `Microsoft.DSC`),
unknown `dscd` properties and unknown top-level fields are ignored, including
arbitrary nested objects, arrays, scalars and nulls. They remain in the original
document sent to DSC.

The document, `metadata`, and `metadata.dscd` must be objects when present.
An invalid known value, including null/non-string `operation`, an empty string,
or unsupported spelling, never falls back to `set`. Malformed JSON/YAML,
multiple YAML documents, ambiguous keys along the metadata path, or unreadable
input produces an `input` failure for only that configuration, without invoking
DSC; later configurations continue. Changes trigger targeted reconciliation,
with periodic full passes as fallback, without restarting the daemon.
Parameter sidecars still work with either format,
and `.yml` remains unsupported.

**The companion `.dscd.json` mechanism has been removed.** Move its operation into
the configuration before upgrading, then remove the obsolete file. Former
companion files are ordinary `.json` configuration candidates, not metadata for
another file. Without embedded metadata, the configuration defaults to `set`.

A successful `test` that reports `inDesiredState: false` is still a successful
execution. `dscd` preserves the structured `dscResult` for consumers to inspect
without interpreting individual resources.

### DSC inputs

Beyond document syntax and `metadata.dscd.operation`, DSC owns validation,
resource discovery, parameter interpretation, defaults, substitution, secure
values and execution. `dscd` does not inspect individual resources or merge either
input. It sends the captured configuration
and optional sidecar text inline through `invoke_dsc_config` with the effective
`set` or `test` operation; DSC handles resource semantics.

**Migration from CLI parameter files:** the server tool expects a direct mapping:

```yaml
# web.parameters.yaml
message: Hello from parameters
```

JSON `{"message":"Hello from parameters"}` is equally valid. Do **not** use the
CLI's outer `parameters:` wrapper with this server. `dscd` deliberately sends the
sidecar unchanged rather than parsing or translating it.

Producers must be trusted and publish complete documents by temporary-file
replacement, not in-place editing. The daemon opens each input once per attempt,
captures its UTF-8 text, hashes that snapshot together with the effective operation,
and sends the same text and operation to DSC. Embedded metadata is part of that
snapshot: formatting changes and ignored properties change the hash even though
they do not change the effective operation.
DSC never reopens the input paths: **bytes hashed equal bytes submitted**.
Replacing any input after capture does not affect that attempt. Capturing
configuration and parameters is not an atomic producer transaction;
producers still coordinate updates.
Inline inputs do not provide the CLI's implicit file-root/`DSC_CONFIG_ROOT`
context; documents must not rely on it.
Keep executable/resource directories and input/output parents non-writable by
untrusted users. Service working directories and environments differ from a
console; install resources for the service identity and do not rely on a user's
profile, PATH or current directory.

## Results and operational bounds

`10-a.yaml` maps to `10-a.yaml.result.json` and `10-a.json` to
`10-a.json.result.json`. Overlong/unrepresentable destination names are rejected
before executing the document. Removing or renaming an input does not remove old
results or undo machine changes.

Every published result retains the fields below, including explicit nulls.
The additive `operation` field records the effective operation, including implicit
`set`, and always matches the operation submitted when DSC is invoked. It is
`null` when invalid metadata or an earlier input failure prevents resolving an
operation; no DSC invocation occurs in that case.
Two fields are included when available: `parameters` is the sidecar basename,
and `inputHash` is the combined SHA-256 identity of the exact submitted
configuration/parameter bytes and effective operation. Changing parameters or
switching `set`/`test` changes the identity; absent and empty parameters differ.
Omitted and explicit `set` metadata have the same operation but different
configuration bytes and therefore different hashes. The operation-aware hash
retains the `dscd-input-v2` framing, and the result envelope remains schema version 1.
Ambiguous, unreadable, or invalid inputs have no hash. Startup failures can retain
the captured hash even though submission could not occur.

```json
{
  "schemaVersion": 1,
  "configuration": "10-a.yaml",
  "operation": "set",
  "startedAt": "2026-10-05T18:00:00Z",
  "finishedAt": "2026-10-05T18:00:01Z",
  "durationMs": 1000,
  "outcome": "succeeded",
  "exitCode": null,
  "dscResult": {"metadata": {}, "results": [], "messages": [], "hadErrors": false},
  "error": null,
  "stderr": ""
}
```

This is an illustrative envelope, not a recorded DSC execution. Outcomes are
`succeeded`, `failed`, or `canceled`; failure objects contain `kind` and `message`.
Success requires a successful JSON-RPC response and valid DSC result with `hadErrors: false`.
Kinds are `input`, `start`, `exit`, `output`, `dsc`, and `canceled`. Timeouts are
`canceled`; frame/shape/ID errors are `output`. Unknown nested fields and numeric
values are preserved in `dscResult`. `exitCode` is null for success and normal
DSC errors; a failed request may record the actual server exit code, including
forced termination. `stderr` remains present but is empty: a shared server stream
cannot be reliably attributed to one configuration.
See the [design](docs/design.md#result-contract) for precedence and field semantics.

Combined configuration/parameter input, including embedded metadata, is limited
to **16 MiB** per attempt; there is no separate metadata file or byte budget.
Invalid UTF-8 is rejected to avoid changing bytes when encoding JSON strings.
Each newline-delimited JSON-RPC stdout frame is limited to **16 MiB**; malformed
or oversized responses fail the attempt and destroy the session. Stderr is always
drained to a discard sink with fixed buffering; no shared diagnostics accumulate
or leak into logs/results. Initialization allows up to ten seconds (or the shorter
execution timeout). At pass completion, close stdin and allow two seconds for
normal server exit, then terminate if needed. Background descendants are cleaned
up when the session ends; resources must use a service manager for persistent services.
Linux uses process groups, plus systemd cgroups when packaged; Windows assigns
DSC to a kill-on-close Job Object **before** its first instruction executes.

On shutdown, scheduling stops, active execution is killed, and publication of a
canceled result is attempted with a five-second cleanup context. Filesystem
syscalls are not interruptible by Go contexts, so the command boundary imposes a
30-second shutdown bound instead of leaking an unbounded publication goroutine.
A hard exit or power loss can leave unpublished `.dscd-*` files; remove those
only after stopping the daemon. Applied resource changes cannot be rolled back.

Result files contain potentially sensitive DSC output. New Linux directories
are `0700` and files `0600`; Windows protects new directories and each file for
the daemon identity, SYSTEM and administrators. Existing directory permissions
are left unchanged. Routine logs contain attempt metadata, not raw DSC output.
Input contents are never copied into daemon metadata or logs. Preserved DSC
results and JSON-RPC error messages may still contain values echoed by DSC/resources; `dscd` does
not redact them. Keep result access restricted accordingly.

Publication writes, syncs and closes a private temporary file before replacement:

- **Linux:** same-directory rename followed by directory fsync. A directory-sync
  error means the new result may be visible but durability is uncertain.
- **Windows:** native `FileRenameInfoEx` replacement on NTFS, preserving existing
  readers. Readers should permit delete sharing; sharing/ACL/antivirus failures
  are reported without deleting the old result. File data is flushed first, but
  there is **no equivalent directory-fsync / power-loss durability guarantee**.

A failed attempt replaces the old success if publication succeeds. Publication
failure is logged separately and leaves readers with a potentially stale prior
result; the next scheduled pass retries naturally. Readers must check timestamps,
input existence and schema version, and tolerate unknown fields.

## Linux service: systemd

Configure an [official Microsoft package repository](https://learn.microsoft.com/en-us/powershell/dsc/install?view=dsc-3.0)
that supplies `dsc` for your distribution, or install Microsoft's
[DSC release package](https://github.com/PowerShell/DSC/releases/tag/v3.3.0)
separately. The daemon packages depend on `dsc >= 3.3.0`; APT/DNF resolve it when
available in configured repositories. They do not add repositories, download DSC
in install scripts, or bundle DSC/resources. Microsoft packages provide
`/usr/bin/dsc`. Install any required resources for root.

If your configured repository supplies DSC, install it with `sudo apt install dsc`
or `sudo dnf install dsc`. Otherwise download the corresponding official
`dsc_3.3.0-1_amd64.deb` or `dsc-3.3.0-1.x86_64.rpm` release asset and install
it with APT/DNF before installing `dscd`.

Download the matching AMD64 package from GitHub Releases, then:

Debian / Ubuntu:

```sh
sudo apt install ./dscd_0.0.3_amd64.deb
```

Fedora / RHEL-compatible systems:

```sh
sudo dnf install ./dscd-0.0.3-1.x86_64.rpm
```

Use the filenames for your selected version. Installation enables and starts
`dscd` when systemd is running:

```sh
systemctl status dscd
journalctl -u dscd -f
```

The supplied unit deliberately runs as **root** because general DSC resources
may require system changes. For narrower resource sets, use a systemd override
to set a least-privileged user/group and provision access to both directories and
resources. No speculative resource-breaking sandbox restrictions are enabled.
`KillMode=control-group` and `TimeoutStopSec=40s` provide final descendant cleanup.

The package installs `/usr/bin/dscd` and the vendor unit at
`/usr/lib/systemd/system/dscd.service`. It creates `/etc/dsc/config.d` and
`/var/lib/dsc/results.d` as root:root `0700` when absent, without recursively
changing existing data permissions. Results are private `0600` files.
Only trusted administrators should publish documents:

```sh
sudo install -m 0600 ./10-example.yaml /etc/dsc/config.d/.10-example.yaml.tmp
sudo mv -f /etc/dsc/config.d/.10-example.yaml.tmp /etc/dsc/config.d/10-example.yaml
```

The daemon performs a full pass immediately on startup and five minutes after
each completed full pass. Filesystem notifications also trigger targeted
reconciliation of new or changed inputs; deploying them does **not** require a
restart. Adding a document authorizes privileged machine changes.

Manage the service:

```sh
sudo systemctl stop dscd
sudo systemctl start dscd
sudo systemctl restart dscd
```

Upgrade by installing a newer package with the same APT/DNF command.
Upgrades preserve documents/results and restart the service only if it was
running. Remove with `sudo apt remove dscd` or
`sudo dnf remove --noautoremove dscd`; removal
stops and disables the service but retains documents, results, DSC and all
DSC-managed machine state. Even `sudo apt purge dscd` retains user data.
Review administrator-created systemd drop-ins separately. During image
construction without running systemd, package installation does not start a
service; enable/start it when booted.
DNF's `--noautoremove` retains DSC even if it was installed only as a dependency;
do not request dependency auto-removal if DSC must remain installed.

## Windows service: native MSI

First install Microsoft DSC **3.3.0 or later** and the required resources
machine-wide. The official Windows x64 [DSC release ZIP](https://github.com/PowerShell/DSC/releases/tag/v3.3.0)
can be extracted by an administrator to `%ProgramFiles%\DSC`, with `dsc.exe`
directly in that directory. Add that directory to the **machine/system PATH**,
as described in [Microsoft's installation instructions](https://learn.microsoft.com/en-us/powershell/dsc/install?view=dsc-3.0).
`dsc.exe` must be discoverable through the PATH visible to **LocalSystem before
installing or starting dscd**. A per-user WinGet/Store alias or a change to the
current PowerShell session's PATH is insufficient. LocalSystem must also be able
to access DSC's resources; protect DSC, resources and PATH directories against
unprivileged writes.

Windows services may retain an older environment after a system PATH change.
Reboot before installing/starting dscd if needed to make the new PATH visible to
the service. A successful `dsc.exe --version` in your interactive shell alone
does not establish LocalSystem access.

Download the MSI from a release and double-click it, approving elevation,
or run:

```powershell
msiexec /i .\dscd-<version>-windows-amd64.msi
Get-Service dscd
```

For unattended installation, use an elevated session:

```powershell
msiexec /i .\dscd-<version>-windows-amd64.msi /qn /norestart
```

The MSI does not accept, discover, validate or save a DSC executable path, change
PATH, validate the DSC version, download DSC or install resources. It uses
`dscd`'s default executable name, `dsc`, resolved at service startup. Verify DSC
3.3.0+ and service-account access before installation. Missing DSC causes service
startup to fail; MSI installation then fails through its normal service-start
handling, without a custom prerequisite message. Inspect the MSI log and the
Application Event Log for details.
This failure need not be prompt: an installation without DSC exceeded the smoke
test's two-minute limit waiting in Windows Installer's service-start action.
Provision the prerequisite before running the MSI.
Go, WiX and PowerShell are not required on the target machine.

The MSI installs `%ProgramFiles%\dscd\dscd.exe`, registers **dscd**
(**DSC Reconciliation Daemon**) as an automatic **LocalSystem** service,
starts it, and configures restart after five seconds with a one-day failure
count reset. It creates `%ProgramData%\dsc\config.d` and `results.d` with
protected access for SYSTEM and Administrators only. Results may contain secrets.

Publish trusted configuration documents and optional parameter sidecars into
`%ProgramData%\dsc\config.d` as an administrator. No restart is required:
full reconciliation runs immediately at startup and five minutes after each
completed full pass, with targeted reconciliation on input changes between
passes. Publish new pairs parameter-first using complete-file replacement.
The MSI uses the default fifteen-minute execution timeout.

The binary automatically detects SCM operation, accepts Stop and Shutdown, and
reports lifecycle progress. JSON messages use Event ID 1; inspect their `level`
property rather than the informational Event Log transport severity. Host
shutdown policy may terminate services sooner than the daemon's own bound.

```powershell
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='dscd'} -MaxEvents 20 |
    Select-Object TimeCreated, Message
Stop-Service dscd
Start-Service dscd
Restart-Service dscd
# Upgrade by installing a newer MSI; DSC must remain on the machine PATH.
msiexec /i .\dscd-<new-version>-windows-amd64.msi /qn /norestart
# Uninstall using the installed package, or Windows Installed apps:
msiexec /x .\dscd-<installed-version>-windows-amd64.msi /qn /norestart
```

Upgrades stop the service before replacing it and restart it afterward.
Uninstall removes the executable, service and package-owned Event Log source,
but retains both data directories, documents, results, Microsoft DSC and
previously reconciled machine state. Standard MSI repair restores package-owned
resources without overwriting configuration documents.

The ZIP and `packaging/windows/install.ps1`, `uninstall.ps1`, and
`test-service.ps1` remain development/manual-testing options, not MSI dependencies.
There is no migration from manually registered services.
Installer build/version details are in [the design](docs/design.md#windows-msi).
Service integration installs the exact release MSI on a disposable Windows
runner, or builds it for a manual test run. Ordinary CI only builds and inspects.

## Validation limits

This refactor passed [native Linux/Windows CI](https://github.com/Bpoe/dsc-reconciler/actions/runs/37988454033)
and [disposable service integration](https://github.com/Bpoe/dsc-reconciler/actions/runs/37988454278).
The latter exercised fresh MSI/DEB installation, actual Echo reconciliation,
service lifecycle, repair/reinstall where applicable, uninstall/data retention,
and Fedora RPM lifecycle. Upload/download artifact IDs and digests matched.
Final release checksum commands also passed against those downloaded packages;
no tag or release was created to test the full release entry point.

Local build, unit/process/filesystem tests, vet and race checks ran on Windows
amd64 and Linux amd64 under WSL. Process cleanup, output limits, replacement of
existing files, concurrent readers, private permissions, paths with spaces,
and the in-memory SCM lifecycle are covered.

The earlier MSI using explicit DSC paths passed native builds, table inspection
and lifecycle tests in [service-integration run 37578288245](https://github.com/Bpoe/dsc-reconciler/actions/runs/37578288245).
That run does not validate the current PATH-based installer. The integration
workflow checks missing-DSC startup failure, PATH-based startup, actual
reconciliation, service/ACL/Event Log/recovery behavior, repair, reinstall,
uninstall and data retention on a disposable runner. Upgrade/payload replacement
and downgrade-rejection checks require explicitly supplied packages and are not
part of the automated release gate.
Host-shutdown delivery and the complete draft-release workflow remain unverified.

Native DEB/RPM builds, metadata, permissions, prerelease ordering and extracted
systemd units were verified locally. With Microsoft DSC 3.3.0 installed,
disposable Ubuntu/Fedora containers exercised installation, upgrade, reinstall,
removal, data/permission preservation and Debian purge without running systemd.
Those checks do not validate service startup.
The DEB lifecycle also passed on a disposable Ubuntu systemd host: initial
enable/start, stop/start, running/stopped/disabled upgrades, reinstall,
remove/reinstall, removal/purge and data/permission preservation. Native RPM
service startup and host shutdown remain
unverified by these local Linux checks. The manual service-integration workflow
provides disposable systemd/SCM smoke checks; its presence does not establish
a completed GitHub Actions run.
The opt-in Echo server test on Windows with DSC `3.3.0` passed for YAML/JSON
configurations with no sidecar and with either inline parameter format in one
initialized session. Other resources,
power-loss durability and other CPU architectures
remain unverified. Linux foreground
cannot contain descendants that deliberately leave its process group; jobs and
cgroups cannot contain work delegated to already-running external services.

## License

[MIT](LICENSE).
