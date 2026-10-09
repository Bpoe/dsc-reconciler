> Historical implementation brief. Its release workflow and RC requirements are
> superseded by the [current packaging/release contract](design.md#packaging-and-operational-limits)
> and [release instructions](../README.md#releases).

Implement native Linux .deb and .rpm packages for the dsc-reconciler project.

The goal is to make deploying dscd to a Linux machine as easy as installing a standard OS package. Installing the package should deploy the executable, configure systemd, create the necessary directories, and start the service.

This is packaging work. Do not redesign dscd or its reconciliation architecture.

Important: This project has no existing deployments. There is no migration or backward-compatibility work required. Favor conventional Linux packaging and simple implementations over defensive code for hypothetical scenarios.

1. Desired installation experience

On Debian or Ubuntu:

sudo apt install ./dscd_<version>_amd64.deb

On Fedora, RHEL, or another supported RPM-based distribution:

sudo dnf install ./dscd-<version>.x86_64.rpm

Assuming Microsoft DSC is available through the configured package repositories, the package manager should resolve the DSC dependency automatically.

After installation:

systemctl status dscd
journalctl -u dscd

The service should be enabled and running.

Users should then be able to deploy configuration documents simply by copying files into:

/etc/dsc/config.d/

The next reconciliation pass should discover and process those documents.

2. Package contents

Install:

/usr/bin/dscd
/etc/dsc/config.d/
/var/lib/dsc/results.d/
/usr/lib/systemd/system/dscd.service

Use the appropriate vendor systemd unit directory for each distribution. Do not install the package-managed unit into /etc/systemd/system.

The packages should include the compiled Go executable and systemd service definition.

Do not bundle DSC itself.

Do not require Go on the target machine.

Do not include unnecessary source files or development tools.

3. DSC dependency

dscd currently requires Microsoft DSC 3.3.0 or later because it uses the dsc server MCP interface.

Declare the appropriate dependencies.

Debian:

Depends: dsc (>= 3.3.0)

RPM:

Requires: dsc >= 3.3.0

Use package-manager-compatible version syntax and verify Microsoft’s actual package names.

DSC is maintained and distributed independently by Microsoft.

Do not download DSC during package installation.

Do not configure Microsoft’s package repositories automatically. Document that users may need to configure the official Microsoft repository or install DSC separately.

Verify the executable location provided by Microsoft’s packages. The systemd service must use the correct DSC path.

The Microsoft.Filesystem.File/Content resource may require DSC 3.4 preview, but that should not change dscd’s minimum version requirement.

4. Systemd service

The repository already contains:

packaging/systemd/dscd.service

Reuse and update this file. Do not introduce a separate service implementation.

The existing unit references /usr/local/bin. Packaged installations should use package-managed locations.

The effective unit should be approximately:

[Unit]
Description=Local DSC reconciliation daemon
After=local-fs.target
[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/bin/dscd -config-dir /etc/dsc/config.d -results-dir /var/lib/dsc/results.d -dsc-path /usr/bin/dsc
Restart=on-failure
RestartSec=5s
KillMode=control-group
TimeoutStopSec=40s
UMask=0077
StandardOutput=journal
StandardError=journal
[Install]
WantedBy=multi-user.target

Verify that /usr/bin/dsc is the actual location installed by Microsoft’s packages and adjust as necessary.

Retain existing service behavior unless a change is necessary for packaging.

The daemon already defaults to:

* Five-minute reconciliation interval.
* Fifteen-minute per-configuration execution timeout.

Do not duplicate those defaults unnecessarily in the systemd unit.

Do not add aggressive systemd sandbox restrictions that could prevent DSC resources from modifying machine configuration.

Initially, run the service as root.

5. Directory creation and permissions

During installation, ensure these directories exist:

/etc/dsc/config.d
/var/lib/dsc/results.d

For newly created directories:

Owner: root
Group: root
Mode: 0700

Configuration documents authorize privileged machine changes. Unprivileged users must not be able to modify them.

DSC results can contain sensitive information and should not be world-readable.

Do not recursively change permissions on existing files or directories.

Do not delete existing configurations or results.

Use standard packaging mechanisms where possible.

6. Package lifecycle

Implement conventional installation, upgrade, removal, and reinstall behavior.

Initial installation

1. Install the executable and systemd unit.
2. Create the required directories.
3. Reload systemd.
4. Enable the service.
5. Start the service.

Upgrade

1. Stop the service when necessary.
2. Replace package-managed files.
3. Reload systemd.
4. Restart the service if it was running.
5. Preserve existing configurations and results.

Do not allow two instances of dscd to run simultaneously during an upgrade.

Follow distribution-standard service lifecycle behavior rather than creating custom orchestration.

Removal

Removing the package should:

* Stop the service.
* Disable and remove its systemd registration as appropriate.
* Remove the executable and package-owned service definition.
* Preserve configuration documents.
* Preserve reconciliation results.
* Leave DSC installed.
* Leave previously reconciled machine state unchanged.

Purge

Be conservative.

Do not automatically delete user-authored configuration documents or reconciliation results, even during purge.

Do not remove shared parent directories that may be used by DSC or other tools.

Use standard package lifecycle mechanisms wherever available.

Package scripts must behave appropriately when systemd is not running, such as during image construction.

Avoid unnecessary custom shell scripts.

7. Clean installation assumptions

This is the first packaged release of dscd.

There are no existing deployments, migration requirements, or legacy compatibility constraints.

Therefore:

* Do not implement migration detection.
* Do not write migration scripts.
* Do not support legacy executable locations.
* Do not preserve the previous /usr/local/bin/dscd convention.
* Do not introduce compatibility flags or fallback behavior.
* Update the existing systemd unit to use the package-managed paths.
* Replace obsolete manual installation instructions with package installation instructions.

Normal future package upgrades and uninstallation must still work correctly.

Keep the implementation simple.

8. Packaging implementation

Use a lightweight and maintainable packaging tool.

Prefer nFPM unless another approach is demonstrably simpler.

Place packaging definitions under:

packaging/linux/

Keep DEB and RPM definitions consistent.

Avoid duplicating metadata unnecessarily.

Pin the packaging tool version in CI.

The packaging process should:

1. Compile dscd for Linux AMD64.
2. Stage the executable.
3. Include the systemd unit.
4. Generate the DEB package.
5. Generate the RPM package.
6. Verify the generated artifacts.

Do not add packaging dependencies to the Go runtime module.

Do not build DSC.

For now, support Linux AMD64 only.

Structure the packaging so ARM64 can be added later without redesign, but do not implement ARM64 in this change.

9. Versioning

Use the GitHub release version as the source of truth.

The existing release workflow accepts release candidates such as:

v0.0.1-rc.1

Convert release tags into valid Debian and RPM package versions.

Prerelease versions must sort before their corresponding stable releases.

Ensure normal package upgrades will work when moving from a release candidate to a stable release.

Do not hard-code 0.1.0 into the packaging configuration.

Use conventional Linux package filenames.

10. GitHub release integration

The repository already contains:

.github/workflows/release.yaml

Extend this workflow rather than creating a separate release process.

Currently, it produces Linux and Windows binary archives and creates a draft GitHub pre-release.

Preserve the existing behavior.

Add DEB and RPM generation to the Linux build job.

Release artifacts should include:

dscd-<version>-linux-amd64.tar.gz
dscd_<version>_amd64.deb
dscd-<version>-1.x86_64.rpm
dscd-<version>-windows-amd64.zip
SHA256SUMS

Adjust filenames where necessary to follow package-manager conventions, particularly for prerelease versions.

All artifacts must be built from the same selected Git revision.

The workflow should continue to:

* Validate release versions.
* Run existing Go checks.
* Refuse to overwrite existing release tags.
* Create a draft pre-release.
* Publish SHA-256 checksums.

Update checksum generation to include all four release artifacts.

Do not automatically publish a release without the existing review step.

Do not create an APT or RPM repository.

GitHub Releases remains the distribution mechanism.

11. Packaging validation

Add automated validation for the generated packages.

At minimum, verify:

* Both packages build.
* Package metadata contains the expected name, version, architecture, and DSC dependency.
* The executable is included.
* The executable has appropriate permissions.
* The systemd unit is included at the correct path.
* The systemd unit references the correct executable paths.
* The binary supports -help.
* Package file ownership and permissions are appropriate.

Use tools such as:

dpkg-deb --info
dpkg-deb --contents
rpm -qpi
rpm -qpl
systemd-analyze verify

Add installation smoke tests using disposable Linux environments.

For Debian/Ubuntu, validate as much of this lifecycle as practical:

1. Install DSC.
2. Install the generated DEB.
3. Verify the service is registered.
4. Verify the service starts.
5. Verify the configuration directory exists.
6. Verify the results directory exists.
7. Verify directory permissions.
8. Stop and restart the service.
9. Remove the package.
10. Verify configuration and result data are preserved.

Perform equivalent validation for RPM distributions where practical.

A container without systemd does not validate service startup.

Separate package installation tests from real systemd integration tests when necessary.

Reuse or extend the existing manual service-integration workflow where appropriate.

Do not make ordinary Go unit tests install system packages or alter the host.

12. Documentation

Update:

README.md
docs/design.md
AGENTS.md

Document:

* Installing Microsoft DSC.
* Installing the DEB package.
* Installing the RPM package.
* Managing the systemd service.
* Viewing logs with journalctl.
* Deploying DSC configuration files.
* Upgrading and uninstalling.
* Directory locations and permissions.

Keep the normal installation instructions short.

For example:

sudo apt install ./dscd_<version>_amd64.deb
systemctl status dscd
journalctl -u dscd -f

Explain that dscd reconciles immediately on startup and waits five minutes after each completed reconciliation pass.

Deploying configuration files does not require restarting the service.

13. Scope constraints

Do NOT:

* Change reconciliation logic.
* Change the MCP/JSON-RPC implementation.
* Change configuration discovery.
* Add file watching.
* Add an HTTP API.
* Add remote deployment functionality.
* Bundle DSC.
* Install Microsoft’s package repositories.
* Add automatic software updates.
* Create an APT or RPM package feed.
* Implement Windows MSI packaging.
* Add ARM64 packaging yet.
* Implement migration or backward-compatibility logic.
* Introduce unnecessary abstractions.

Keep this focused on packaging the existing Linux daemon.

14. Completion criteria

Before finishing:

1. Build both Linux packages.
2. Inspect package metadata and contents.
3. Run existing Go tests and checks.
4. Validate the systemd unit.
5. Run installation tests where supported.
6. Verify the GitHub release workflow includes both new artifacts.
7. Ensure documentation matches actual behavior.
8. Review the implementation for unnecessary complexity.

Clearly report which installation and service lifecycle behaviors were actually tested and which still require validation on a real machine.

Desired outcome: Installing dscd on Linux should feel like installing any other native system service.

A user should only need to install DSC, install the dscd package, and copy configuration documents into the configuration directory.