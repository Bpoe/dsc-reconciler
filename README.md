# dsc-reconciler

`dsc-reconciler` is a lightweight local reconciliation daemon for DSC. The `dscd`
process continuously discovers DSC configuration documents in a configured directory,
evaluates them against the local machine, applies any required changes, and records
the results.

The project is intentionally simple. `dscd` does not own configuration authoring,
composition, or parameter processing; it treats DSC configuration and parameter files
as inputs and delegates their interpretation and execution to DSC. This makes it
useful both as a standalone local desired-state reconciler and as a building block
for higher-level configuration control planes.

There is no server, remote configuration store, watcher, document parser or
resource model. DSC owns testing and applying configurations. See the
[design and result contract](docs/design.md) and [contributor guidance](AGENTS.md).

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

The sole Go dependency, `golang.org/x/sys`, provides Windows Job Objects, SCM,
Event Log, ACLs and native file replacement. Linux uses the standard library.

## Build and check

From the module root, on either platform:

```text
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

Normal tests use temporary files, fakes and controlled helper executables, not
DSC. They do not install services or change machine configuration.
[CI](.github/workflows/ci.yaml) runs native build/test/vet/format/race checks on
both OSes for pushes and pull requests, plus Linux package build/inspection
without installation. Service smoke tests run separately on
disposable runners through the manual
[service-integration workflow](.github/workflows/service-integration.yaml):
in GitHub, select **Actions > service-integration > Run workflow**.
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
VERSION=v0.0.1-rc.1 bash packaging/linux/build.sh dist
VERSION=v0.0.1-rc.1 bash packaging/linux/validate.sh dist
```

Validation extracts packages without installing them. The manual integration
workflow additionally tests DEB lifecycle with real systemd on Ubuntu and RPM
lifecycle in a Fedora image without systemd; the latter is not service-startup
validation. Installation smoke tests are destructive and disposable-only.

## Release candidates

The manual [release workflow](.github/workflows/release.yaml) creates a **draft
pre-release**, initially `v0.0.1-rc.1`. In GitHub, select **Actions > release >
Run workflow**, choose the source branch, and enter a new
`vX.Y.Z-rc.N` or `vX.Y.Z` tag. Use a revision that has passed CI and review the separate
service-integration results before publishing.

The workflow tests and vets the selected revision on native Linux and Windows
amd64 runners, checks formatting and module integrity, then builds binaries
without a C runtime dependency. It does not run real DSC or install services;
race checks remain in the normal CI workflow.

The draft contains (for Linux AMD64 and Windows AMD64):

- `dscd-v0.0.1-rc.1-linux-amd64.tar.gz`
- `dscd_0.0.1~rc.1_amd64.deb`
- `dscd-0.0.1~rc.1-1.x86_64.rpm`
- `dscd-v0.0.1-rc.1-windows-amd64.zip`
- `dscd-v0.0.1-rc.1-windows-amd64.msi` (recommended Windows installation)
- `SHA256SUMS` covering all release artifacts

Each archive has a versioned root containing `bin/dscd` or `bin/dscd.exe`,
platform-specific `packaging` assets, the README, design and agent documentation,
and the MIT license. Use the native Linux packages for systemd installation, or
extract an archive for foreground execution. Use the MSI for Windows service installation.
Building from source is not required. DSC and resources are not bundled.
On Linux, verify downloads with `sha256sum --check --ignore-missing SHA256SUMS`.
On Windows, use `Get-FileHash -Algorithm SHA256` and compare the archive's
hash with its entry in `SHA256SUMS`.

Both platform jobs must succeed before a tag and draft are created. The tag
points to the exact tested workflow revision, not the latest branch head.
Existing tags are never moved or reused. Review the draft assets and notes in
**Releases**, then publish it while retaining the pre-release designation.
If a run fails after creating its tag, inspect the partial draft/tag before
proceeding with a new candidate version; reruns do not overwrite existing releases.

## Foreground execution

Start with an empty directory if you only want to inspect startup/shutdown.
Adding a DSC document authorizes DSC to modify machine state.

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
| `-config-dir` | Linux `/etc/dsc/config.d`; Windows `%ProgramData%\dsc\config.d` | Existing readable input directory. |
| `-results-dir` | Linux `/var/lib/dsc/results.d`; Windows `%ProgramData%\dsc\results.d` | Results directory, privately created if absent. |
| `-dsc-path` | `dsc` | Executable path or name, resolved once at startup through PATH/PATHEXT. |
| `-interval` | `5m` | Positive delay after each completed reconciliation pass. |
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
are reapplied because machine state may drift. Reconcile immediately on startup,
then wait the full interval after each completed pass before starting the next.
Passes never overlap, and slow passes do not cause catch-up runs.

Each pass normally starts **one short-lived `dsc server`**, initializes its
MCP/JSON-RPC stdio session, and uses `invoke_dsc_config` to apply each configuration
sequentially. The server is closed before the pass ends; DSC is not kept alive
during the interval. Empty directories start no server. Input errors do not
require starting a server.

Ordinary DSC errors fail that input but retain the same session. Server crashes,
timeouts and invalid protocol responses destroy the current session; the next
configuration starts a fresh one. This restart is fault recovery, not a retry of
the failed input. Server startup/initialization failure publishes failures for
remaining readable inputs, reports a pass-level error and retries next pass.

`*.parameters.yaml` and `*.parameters.json` are reserved sidecars, never standalone
configurations. Match by the configuration filename without its final extension:

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

DSC owns parsing, validation, resource discovery, parameter interpretation,
defaults, substitution, secure values and execution. `dscd` does not inspect
individual resources or merge either input. It sends the captured configuration
and optional sidecar text inline through `invoke_dsc_config(operation: "set")`;
DSC handles testing and applying changes.

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
captures its UTF-8 text, hashes that snapshot, and sends the same text to DSC.
DSC never reopens the input paths: **bytes hashed equal bytes submitted**.
Replacing either source after capture does not affect that attempt. Capturing a
pair is not an atomic producer transaction; producers still coordinate updates.
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

Every published result retains the ten fields below, including explicit nulls.
Two additive fields are included when available: `parameters` is the sidecar
basename, and `inputHash` is the combined SHA-256 identity of the exact submitted
bytes. Changing only the parameter file changes the identity; an absent and an
empty sidecar differ. Ambiguous or unreadable inputs have no hash. Startup failures
can retain the captured hash even though submission could not occur.

```json
{
  "schemaVersion": 1,
  "configuration": "10-a.yaml",
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

Combined configuration/parameter input is limited to **16 MiB** per attempt.
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
sudo apt install ./dscd_0.0.1~rc.1_amd64.deb
```

Fedora / RHEL-compatible systems:

```sh
sudo dnf install ./dscd-0.0.1~rc.1-1.x86_64.rpm
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
sudo install -m 0600 ./10-example.yaml /etc/dsc/config.d/10-example.yaml
```

The daemon reconciles immediately on startup, then waits five minutes after
each completed pass. New documents are discovered on the next pass; deploying
them does **not** require a restart. Adding a document authorizes privileged
machine changes.

Manage the service:

```sh
sudo systemctl stop dscd
sudo systemctl start dscd
sudo systemctl restart dscd
```

Upgrade by installing a newer package with the same APT/DNF command. Release
candidates use `~rc.N` in native versions so the stable version sorts later.
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

Copy trusted configuration documents and optional parameter sidecars into
`%ProgramData%\dsc\config.d` as an administrator. No restart is required:
reconciliation runs immediately at startup and then waits five minutes after
each completed pass. The MSI uses the default fifteen-minute execution timeout.

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
The manual service-integration workflow builds, inspects and installs the MSI
on a disposable Windows runner; ordinary CI only builds and inspects it.

## Validation limits

Local build, unit/process/filesystem tests, vet and race checks ran on Windows
amd64 and Linux amd64 under WSL. Process cleanup, output limits, replacement of
existing files, concurrent readers, private permissions, paths with spaces,
and the in-memory SCM lifecycle are covered.

The earlier MSI using explicit DSC paths passed native builds, table inspection
and lifecycle tests in [service-integration run 37578288245](https://github.com/Bpoe/dsc-reconciler/actions/runs/37578288245).
That run does not validate the current PATH-based installer. The updated manual
workflow checks missing-DSC startup failure, PATH-based startup, service/ACL/Event
Log/recovery behavior, upgrade and payload replacement, downgrade rejection,
repair, reinstall, uninstall and data retention on a disposable runner.
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
real DSC execution on Linux, power-loss durability and other CPU architectures
remain unverified. Linux foreground
cannot contain descendants that deliberately leave its process group; jobs and
cgroups cannot contain work delegated to already-running external services.

## License

[MIT](LICENSE).
