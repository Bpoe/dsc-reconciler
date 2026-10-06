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
- Microsoft **DSC 3.1.0** and the resources your documents require, installed for
  the account running the daemon. This is the modern `dsc` CLI, not Windows
  PowerShell's `Start-DscConfiguration`.
- Linux with a local filesystem supporting atomic rename and directory fsync
  (systemd for service installation), or Windows 11 / Windows Server 2022 or newer
  with local NTFS. macOS, network shares and FAT are not supported.

Install DSC from the [official release](https://github.com/PowerShell/DSC/releases/tag/v3.1.0).
The invocation and envelope were verified against the official version-tagged
[CLI](https://github.com/PowerShell/DSC/blob/v3.1.0/docs/reference/cli/config/set.md)
and [result](https://github.com/PowerShell/DSC/blob/v3.1.0/docs/reference/schemas/outputs/config/set.md)
references. An opt-in Echo smoke test also exercised DSC `3.2.0-preview.14`;
this does not certify all resources or that release.
Other releases may work if they preserve that contract, but are not certified.
The daemon does not check or pin the installed version at runtime.

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
both OSes for pushes and pull requests. Service smoke tests run separately on
disposable runners through the manual
[service-integration workflow](.github/workflows/service-integration.yaml):
in GitHub, select **Actions > service-integration > Run workflow**.
`packaging\windows\test-service.ps1` is a separate, **administrator-only,
opt-in** SCM integration check; never run it on a production machine.

To explicitly run the real DSC parameter-file tests, install `Microsoft.DSC.Debug/Echo`
and set `DSCD_TEST_DSC_PATH` for that test process:

```powershell
$env:DSCD_TEST_DSC_PATH = 'C:\path\to\dsc.exe'
go test ./internal/dsc -run TestRealDSCParameterFiles -count=1 -v
Remove-Item Env:\DSCD_TEST_DSC_PATH
```

Without that variable, normal tests skip real DSC execution.

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
| `-interval` | `5m` | Positive ticker interval, not a delay after pass completion. |
| `-execution-timeout` | `15m` | Positive per-document timeout. Later documents continue after a timeout. |

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
are reapplied because machine state may drift. A slow pass never overlaps another
pass; a pending tick may cause the next pass immediately.

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

DSC owns parsing, validation, defaults, substitution and secure values.
`dscd` does not inspect or merge either file. It passes the sidecar using
`dsc config --parameters-file <sidecar> set --file <configuration> --output-format json`.
As before, DSC's `set` operation handles testing and applying changes itself.

Producers must be trusted and publish complete documents by temporary-file
replacement, not in-place editing. The daemon rechecks and hashes both files
before execution, but the observed hash is not an immutable snapshot: DSC may
read newer bytes if paths change afterward. There is no protection against
malicious path replacement.
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
basename, and `inputHash` is the combined SHA-256 identity of the observed input
bytes. Changing only the parameter file changes the identity; an absent and an
empty sidecar differ. Ambiguous or unreadable inputs have no hash.

```json
{
  "schemaVersion": 1,
  "configuration": "10-a.yaml",
  "startedAt": "2026-10-05T18:00:00Z",
  "finishedAt": "2026-10-05T18:00:01Z",
  "durationMs": 1000,
  "outcome": "succeeded",
  "exitCode": 0,
  "dscResult": {"metadata": {}, "results": [], "messages": [], "hadErrors": false},
  "error": null,
  "stderr": ""
}
```

This is an illustrative envelope, not a recorded DSC execution. Outcomes are
`succeeded`, `failed`, or `canceled`; failure objects contain `kind` and `message`.
Success requires a zero exit and valid DSC envelope with `hadErrors: false`.
Kinds are `input`, `start`, `exit`, `output`, `dsc`, and `canceled`. Timeouts are
`canceled`; output-cap termination is `output`. Valid DSC payloads are retained
even on nonzero exits. Unknown nested fields and numeric values are preserved.
See the [design](docs/design.md#result-contract) for precedence and field semantics.

Captured stdout is limited to **16 MiB**, stderr to **1 MiB**. Exceeding either
terminates the process tree and fails the attempt. Truncated stdout is unusable;
stderr retains a prefix and explicit marker. Pipe draining after parent exit or
cancellation is limited to two seconds. Background descendants are cleaned up
after every attempt; resources must use a service manager for persistent services.
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
Parameter contents are never copied into daemon metadata or logs. Preserved DSC
output and stderr may still contain values echoed by DSC/resources; `dscd` does
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

Install DSC and its resources first. As an administrator:

```sh
sudo install -m 0755 bin/dscd /usr/local/bin/dscd
sudo install -d -m 0700 /etc/dsc/config.d /var/lib/dsc/results.d
sudo install -m 0644 packaging/systemd/dscd.service /etc/systemd/system/dscd.service
# Edit ExecStart if DSC is not at /usr/local/bin/dsc.
sudo systemctl daemon-reload
sudo systemctl enable --now dscd
sudo systemctl status dscd
sudo journalctl -u dscd -f
```

The supplied unit deliberately runs as **root** because general DSC resources
may require system changes. For narrower resource sets, use a systemd override
to set a least-privileged user/group and provision access to both directories and
resources. No speculative resource-breaking sandbox restrictions are enabled.
`KillMode=control-group` and `TimeoutStopSec=40s` provide final descendant cleanup.

Stop/restart and remove (retaining documents/results):

```sh
sudo systemctl stop dscd
sudo systemctl start dscd
sudo systemctl restart dscd
sudo systemctl disable --now dscd
sudo rm /etc/systemd/system/dscd.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/dscd
```

Review and remove any administrator-created drop-ins separately. Removing the
daemon never undoes DSC-managed state.

## Windows service: native SCM

Use an elevated PowerShell session. Copy the built executable to a permanent,
administrator-controlled location and install DSC/resources machine-wide:

```powershell
New-Item -ItemType Directory -Force 'C:\Program Files\dscd' | Out-Null
Copy-Item .\bin\dscd.exe 'C:\Program Files\dscd\dscd.exe'
.\packaging\windows\install.ps1 `
    -BinaryPath 'C:\Program Files\dscd\dscd.exe' `
    -DSCPath 'C:\Program Files\DSC\dsc.exe'
Start-Service dscd
Get-Service dscd
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='dscd'} -MaxEvents 20 |
    Select-Object TimeCreated, Message
```

The installer registers automatic startup/recovery and the `dscd` Application
Event Log source, quotes paths, and provisions new input/results directories.
It deliberately defaults to **LocalSystem**. Review existing directory ACLs and
resource privileges before starting. Installation refuses to overwrite an
existing service. If an installation step fails, it reports the error; inspect
and remove a partially registered service before retrying.

The binary automatically detects SCM operation, accepts Stop and Shutdown, and
reports lifecycle progress. JSON messages use Event ID 1; inspect their `level`
property rather than the informational Event Log transport severity. Host
shutdown policy may terminate services sooner than the daemon's own bound.
For a restricted service account, configure that identity in SCM and grant only
the needed executable, resource, input, result and Event Log access.

```powershell
Stop-Service dscd
Start-Service dscd
Restart-Service dscd
.\packaging\windows\uninstall.ps1
# Optional, after removal:
Remove-Item -LiteralPath 'C:\Program Files\dscd\dscd.exe'
```

Uninstallation stops the service, removes its SCM registration and Event Log
source, and retains binaries, documents, results and historical events. Changing
flags requires updating the registered binary command line or reinstalling.

## Validation limits

Local build, unit/process/filesystem tests, vet and race checks ran on Windows
amd64 and Linux amd64 under WSL. Process cleanup, output limits, replacement of
existing files, concurrent readers, private permissions, paths with spaces,
and the in-memory SCM lifecycle are covered.

Actual service registration and system shutdown were **not** exercised on this
shared machine. The manual service-integration workflow provides disposable
systemd/SCM smoke checks; its presence does not establish a completed run.
The opt-in Echo test on Windows with DSC `3.2.0-preview.14` passed for YAML/JSON
configurations with no sidecar and with either parameter format. Other resources,
real DSC execution on Linux, power-loss durability and other CPU architectures
remain unverified. Linux foreground
cannot contain descendants that deliberately leave its process group; jobs and
cgroups cannot contain work delegated to already-running external services.

## License

[MIT](LICENSE).
