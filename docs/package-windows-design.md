Implement a native Windows MSI installer for the dsc-reconciler project using WiX Toolset.

The goal is to make installing dscd on Windows as simple as installing any other native Windows service.

The installer should deploy the executable, configure the Windows service, create the necessary directories, and start the service.

This is packaging work. Do not redesign dscd or its reconciliation architecture.

Important: There are no existing deployments of this project. Do not implement migration logic, support legacy installation layouts, or add compatibility mechanisms for hypothetical existing installations.

Favor native Windows Installer functionality and a simple, maintainable implementation.

1. Desired installation experience

A user should be able to download:

dscd-<version>-windows-amd64.msi

Double-click the MSI, approve the administrator elevation prompt, and have dscd installed as a Windows service.

The installer should also support silent installation:

msiexec /i dscd-<version>-windows-amd64.msi /qn /norestart

After installation:

Get-Service dscd

should show the service running with automatic startup.

Users should be able to deploy DSC configurations by copying files into:

C:\ProgramData\dsc\config.d\

The next reconciliation pass should discover and apply those configurations.

Installing or updating configuration documents must not require restarting the Windows service.

2. Installation layout

Install the following:

C:\Program Files\dscd\
    dscd.exe
C:\ProgramData\dsc\
    config.d\
    results.d\
Windows Services:
    dscd

Use standard Windows Installer directory properties rather than hard-coding C:\ or assuming Windows is installed on a particular drive.

The actual locations should resolve to:

* [ProgramFiles64Folder]dscd\
* [CommonAppDataFolder]dsc\config.d\
* [CommonAppDataFolder]dsc\results.d\

Install the executable under Program Files with permissions that prevent unprivileged modification.

Configuration and result directories must be protected as described below.

Do not bundle the DSC executable or its resources.

Do not require Go, PowerShell, or WiX on the target machine.

3. Microsoft DSC prerequisite

dscd currently requires Microsoft DSC 3.3.0 or later because it uses the dsc server MCP interface.

DSC should remain a separately installed product.

Before installing or starting dscd, the operator must install Microsoft DSC 3.3.0+
and make dsc.exe available on the machine/system PATH visible to LocalSystem.
The service uses dscd's existing default executable name, dsc, resolved at startup.

An interactive user's PATH or a per-user WinGet/Store executable alias is insufficient.
After changing the system PATH, reboot before installation/startup if needed so
Windows services receive the updated environment. Verify DSC's version and ensure
LocalSystem can execute it and access its resources. Protect the DSC installation,
resources and PATH directories from unprivileged modification.

For example, an administrator can extract the official DSC Windows x64 archive
to C:\Program Files\DSC and add that directory to the system PATH. This is an
operator-selected location, not a discovery convention built into the MSI.

Do not add DSC path properties, file/directory/registry searches, prerequisite
custom actions, version checks, saved DSC paths or PATH changes to the MSI.
Missing DSC fails at daemon startup and uses normal MSI service-start failure
and rollback handling, without a custom discovery error.

Do not download DSC during installation.

Do not install DSC automatically.

Do not modify DSC’s installation.

The Microsoft.Filesystem.File/Content resource may require DSC 3.4 preview, but that should not change the minimum version requirement for dscd.

4. Windows service registration

The repository already implements native Windows Service Control Manager integration.

Reuse it.

The installer should register:

Service name: dscd
Display name: DSC Reconciliation Daemon
Startup type: Automatic
Account: LocalSystem

The executable should be:

[ProgramFiles64Folder]dscd\dscd.exe

The service command line should pass the necessary paths:

-config-dir "[CommonAppDataFolder]dsc\config.d"
-results-dir "[CommonAppDataFolder]dsc\results.d"

The daemon already defaults to:

* Five-minute reconciliation interval.
* Fifteen-minute per-configuration execution timeout.
* DSC executable name dsc, resolved through the service's PATH/PATHEXT.

Do not duplicate these defaults unnecessarily in the service command line.

Use native WiX service functionality such as ServiceInstall, ServiceControl, and appropriate service configuration elements.

Do not register the service by invoking sc.exe, New-Service, or the existing PowerShell installer from an MSI custom action.

The MSI should own service registration and removal through Windows Installer.

5. Service recovery

Configure the service to restart after unexpected failures.

The existing PowerShell installation script uses:

Restart after 5 seconds.
Reset the failure count after one day.

Preserve equivalent behavior.

Use native Windows Installer/WiX functionality where supported.

The service must also stop cleanly when Windows shuts down or the package is uninstalled.

Do not change the existing SCM control handling in the Go application unless testing reveals an actual problem.

6. Directory permissions

Create the following directories if they do not exist:

%ProgramData%\dsc\config.d
%ProgramData%\dsc\results.d

Both directories should be writable only by:

* LocalSystem
* Administrators

Normal users should not have write access.

Configuration documents may authorize privileged changes to the machine, so allowing unprivileged modification would create a serious security vulnerability.

DSC results may contain sensitive information and should not be world-readable.

Use native WiX/MSI ACL mechanisms where practical.

Do not invoke icacls.exe through custom actions unless there is a compelling reason.

Avoid recursively modifying permissions on unrelated files or directories.

Do not delete user-authored configurations or result files.

7. Windows Event Log integration

The existing dscd implementation writes structured events to the Windows Application Event Log.

The current PowerShell installer registers the event source:

dscd

The MSI should perform equivalent registration.

Prefer native MSI registry entries or other supported WiX functionality instead of launching PowerShell.

Ensure:

* The event source exists after installation.
* dscd can write events as LocalSystem.
* Event registration does not depend on an interactive user profile.
* The source is removed appropriately during uninstall.
* Upgrades do not unnecessarily remove or recreate the source.

After installation, users should be able to inspect events using:

Get-WinEvent -FilterHashtable @{
    LogName = 'Application'
    ProviderName = 'dscd'
} -MaxEvents 20

Preserve the existing event format and logging behavior.

Do not modify the application’s logging implementation unless necessary.

8. Package lifecycle

Implement conventional Windows Installer behavior.

Initial installation

1. Require the operator to provision DSC on the LocalSystem-visible machine PATH beforehand.
2. Install dscd.exe.
3. Create the configuration and results directories.
4. Apply directory permissions.
5. Register the Event Log source.
6. Register the Windows service.
7. Configure automatic startup and service recovery.
8. Start the service.

The installation should fail clearly if an essential step cannot be completed.

Use Windows Installer rollback behavior where possible.

Upgrade

Installing a newer MSI should upgrade the existing package.

The upgrade should:

1. Stop the running service.
2. Replace the executable.
3. Update service registration when necessary.
4. Restart the service.
5. Preserve configuration documents.
6. Preserve reconciliation results.
7. Continue using the machine PATH for DSC without persisting a selected executable.

Do not allow two service instances to run simultaneously during an upgrade.

Use normal MSI major-upgrade functionality.

Maintain a stable UpgradeCode across releases and use proper ProductCode and package identity/version rules.

Do not create a custom updater.

Uninstallation

Uninstalling the MSI should:

* Stop the service.
* Remove the service registration.
* Remove the installed executable.
* Remove package-owned Event Log registration.
* Preserve configuration documents.
* Preserve reconciliation results.
* Leave Microsoft DSC installed.
* Leave previously reconciled machine state unchanged.

Do not automatically delete the following directories merely because the package is removed:

%ProgramData%\dsc\config.d
%ProgramData%\dsc\results.d

There is no need to implement a custom cleanup or migration tool.

Repair

Use standard MSI repair behavior.

Repair should restore missing package-owned files and service registration where supported without overwriting user-authored configuration documents.

Do not build elaborate repair logic beyond normal Windows Installer capabilities.

9. WiX implementation

Use a current supported stable version of WiX Toolset.

Pin the WiX version in the build workflow.

Place installer source files under:

packaging/windows/msi/

Use a conventional WiX project structure.

Keep the installer implementation small.

Prefer declarative WiX authoring over custom actions.

Avoid adding PowerShell scripts, C# helper executables, custom bootstrapper applications, or third-party installer frameworks unless genuinely necessary.

Use the existing Go build process to produce:

dscd.exe

Then use WiX to create the MSI.

The installer should not compile Go source itself.

Use the pinned WiX .NET SDK. Keep build.ps1 limited to version mapping, build
arguments and dotnet build with its implicit restore. Run inspect.ps1 separately
after building. Do not add a native helper, Visual C++ toolchain, developer-shell
initialization or Windows SDK build dependency.

Do not bundle development tools or source files.

The MSI should support Windows x64 only for this release.

Do not implement Windows ARM64 packaging yet.

10. Versioning

Use the GitHub release version as the source of truth.

The existing release workflow supports release candidates such as:

v0.0.1-rc.1

MSI’s ProductVersion is more restrictive than SemVer, so handle this explicitly.

Implement a deterministic mapping from the project’s release tags to valid MSI product versions.

Requirements:

* Prereleases must sort before their corresponding stable releases.
* Each new release intended as an upgrade must have a valid higher MSI version.
* Multiple release candidates must not accidentally share the same effective MSI version.
* ProductCode must change appropriately for major upgrades.
* UpgradeCode must remain stable.
* MSI version-field limits must be respected.

Keep the mapping simple and document it.

Do not silently discard the prerelease suffix in a way that causes MSI version collisions.

Do not hard-code the first release version into the WiX project.

Use the full GitHub version in the release artifact filename and any human-readable version information where appropriate.

11. GitHub release integration

The repository already contains:

.github/workflows/release.yaml

Extend this workflow rather than creating a separate release process.

The Windows build job currently produces a ZIP archive containing the executable and service installation scripts.

Continue producing the ZIP.

Add MSI generation to the same Windows build job.

The release should include:

dscd-<version>-windows-amd64.zip
dscd-<version>-windows-amd64.msi

alongside the Linux release artifacts.

All artifacts must be built from the same selected Git revision.

The workflow should:

1. Validate the release version.
2. Check out the selected revision.
3. Run the existing Go checks.
4. Build the Windows AMD64 executable.
5. Verify that the executable runs.
6. Build the MSI using WiX.
7. Inspect and validate the MSI.
8. Upload both Windows artifacts.

Update the draft-release job to include the MSI.

Update SHA256SUMS to include the MSI and all existing release artifacts.

The Linux packaging work may add DEB and RPM files to the release workflow. Preserve those changes if they are present.

Do not replace the release workflow wholesale.

Continue using draft GitHub pre-releases.

Do not automatically publish releases.

Do not create a WinGet manifest or Chocolatey package in this change.

12. Installer validation

Add automated tests appropriate for a native Windows installer.

At minimum, validate:

* MSI builds successfully.
* MSI architecture is x64.
* Product name is correct.
* Manufacturer/publisher metadata is appropriate.
* MSI version is correct.
* UpgradeCode is stable.
* Package contains dscd.exe.
* Executable installation path is correct.
* Service registration is present.
* Service startup configuration is correct.
* Service recovery behavior is configured.
* Event Log registration is present.
* Required directories and ACL definitions are included.
* The service passes only config/results paths and uses the default DSC name.
* No DSC discovery, saved path, helper custom action or PATH modification exists.

Use suitable MSI inspection tools.

Do not consider a successful WiX build alone sufficient validation.

13. Windows installation smoke tests

Add an installation smoke test using a disposable Windows environment.

Prefer a GitHub Actions Windows runner or an equivalent disposable VM.

Do not run MSI installation tests as part of ordinary Go unit tests.

The test should:

1. Build or obtain the generated MSI.
2. Make DSC 3.3.0+ available on the machine PATH already visible to LocalSystem.
3. Install the MSI silently.
4. Verify dscd.exe exists under Program Files.
5. Verify the dscd service exists.
6. Verify the service runs as LocalSystem.
7. Verify automatic startup.
8. Verify the service reaches Running state.
9. Verify both ProgramData directories exist.
10. Verify their access permissions.
11. Verify Application Event Log integration.
12. Stop and start the service.
13. Uninstall the MSI silently.
14. Verify the service is removed.
15. Verify the executable is removed.
16. Verify configuration and result data remain intact.

For the smoke test, use an empty configuration directory initially.

The daemon should not need to execute an actual DSC configuration simply to prove that the service starts.

Use an appropriate test timeout and capture MSI logs on failure.

The smoke-test environment must be disposable because the MSI modifies machine-wide state.

Where practical, also test:

* Reinstall after uninstall.
* Upgrade from an earlier test MSI.
* A direct service restart with missing DSC fails and logs a resolution error.
  Do not rely on an MSI prerequisite rejection: native StartServices can wait
  beyond the test timeout when DSC is absent.
* DSC lookup through a machine PATH directory containing spaces.

Keep the tests proportional to the project’s size.

If a real Windows service test cannot run in CI, document the limitation clearly rather than claiming it was validated.

14. Existing PowerShell scripts

The repository currently includes:

packaging/windows/install.ps1
packaging/windows/uninstall.ps1
packaging/windows/test-service.ps1

These scripts currently perform manual service registration and testing.

The MSI must not depend on them.

Do not invoke the PowerShell installation scripts from WiX custom actions.

The MSI should use native Windows Installer mechanisms for installing and controlling the service.

The scripts may remain available for manual development and testing if they are still useful.

However:

* Do not duplicate their logic unnecessarily.
* Do not add compatibility code for installations made using those scripts.
* Do not implement migration from manually installed services.
* Do not complicate MSI upgrades to account for hypothetical previous deployments.

There are no existing deployments to migrate.

The MSI should be the recommended installation method.

15. Documentation

Update:

README.md
docs/design.md
AGENTS.md

Document:

* Installing Microsoft DSC as a prerequisite.
* Installing the MSI interactively.
* Installing the MSI silently.
* Adding DSC to the machine/system PATH visible to LocalSystem before startup,
  including the possible reboot after a PATH change.
* Managing the Windows service.
* Viewing Windows Event Log messages.
* Deploying configuration documents.
* Upgrading the MSI.
* Uninstalling the MSI.
* Installation paths and directory permissions.

Keep the normal installation instructions short.

For example:

msiexec /i .\dscd-<version>-windows-amd64.msi
Get-Service dscd

For unattended deployment:

msiexec /i .\dscd-<version>-windows-amd64.msi /qn /norestart

Explain that dscd reconciles immediately on startup and then waits five minutes after each completed reconciliation pass.

Configuration documents can be deployed without restarting the service.

Do not recommend building the project from source as the normal installation procedure.

16. Scope constraints

Do NOT:

* Change the reconciliation engine.
* Change DSC MCP/JSON-RPC integration.
* Change configuration discovery.
* Add file watching.
* Add an HTTP server.
* Add remote deployment functionality.
* Bundle Microsoft DSC.
* Download DSC automatically.
* Add automatic software updates.
* Create a WinGet or Chocolatey package.
* Implement ARM64 packaging.
* Implement migration or legacy compatibility code.
* Introduce an application bootstrapper.
* Add unnecessary custom actions.
* Introduce dependencies on PowerShell for MSI installation.
* Modify Linux packaging unnecessarily.

Keep this focused on creating a conventional Windows MSI installer for the existing daemon.

17. Validation

Before finishing:

1. Build the Windows executable.
2. Build the MSI.
3. Inspect the MSI metadata and contents.
4. Validate the service registration configuration.
5. Validate installation directory permissions.
6. Run the existing Go tests and checks.
7. Run installation smoke tests where supported.
8. Verify release workflow integration.
9. Verify SHA-256 checksums include the MSI.
10. Update the documentation.
11. Review the implementation for unnecessary complexity.

Clearly report which behaviors were actually validated and which still require testing on a real Windows machine.

Desired outcome: Installing dscd on Windows should feel like installing any other native Windows service.

The user should only need to install DSC, install the MSI, and copy DSC configuration documents into the configuration directory.
