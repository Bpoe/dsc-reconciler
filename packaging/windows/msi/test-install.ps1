# Opt-in machine-wide tests, never run by ordinary Go tests.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $MsiPath,
    [string] $UpgradeMsiPath,
    [switch] $Disposable
)
$ErrorActionPreference = 'Stop'
if (-not $IsWindows -or -not $Disposable) { throw 'Requires Windows and explicit -Disposable consent.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run as administrator on a disposable Windows runner.'
}
$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path
if ($UpgradeMsiPath) { $UpgradeMsiPath = (Resolve-Path -LiteralPath $UpgradeMsiPath).Path }
# Check only the machine PATH, excluding interactive-user executable aliases.
# The runner must provision this PATH for LocalSystem before invoking the test.
$originalPath = $env:PATH
try {
    $env:PATH = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
    $dscExecutable = (Get-Command dsc.exe -CommandType Application -ErrorAction Stop).Source
}
finally { $env:PATH = $originalPath }
$dscVersionOutput = & $dscExecutable --version
if ($LASTEXITCODE -ne 0) { throw 'The supplied DSC executable does not run.' }
$match = [regex]::Match(($dscVersionOutput -join ' '), '\b(\d+\.\d+\.\d+)\b')
if (-not $match.Success -or [version]$match.Groups[1].Value -lt [version]'3.3.0') {
    throw 'Smoke tests require actual Microsoft DSC 3.3.0 or later, supplied by CI, not the installer.'
}
$programFiles = [Environment]::GetFolderPath('ProgramFiles')
$data = Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'dsc'
$binary = Join-Path $programFiles 'dscd\dscd.exe'
$license = Join-Path $programFiles 'dscd\LICENSE'
$config = Join-Path $data 'config.d'
$results = Join-Path $data 'results.d'
$eventKey = 'HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\dscd'
$settingsKey = 'HKLM:\SOFTWARE\dsc-reconciler\dscd'
if ((Get-Service dscd -ErrorAction SilentlyContinue) -or
    (Test-Path $eventKey) -or (Test-Path $settingsKey) -or
    (Test-Path (Split-Path $binary)) -or (Test-Path $data)) {
    throw 'Refusing to test on a machine with existing dscd installation or data.'
}
$logDirectory = Join-Path $PSScriptRoot ("test-output/" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
$installedMsi = $null
$currentMsi = $MsiPath
$started = Get-Date

function Assert([bool] $Condition, [string] $Message) {
    if (-not $Condition) { throw "MSI smoke test: $Message" }
}
function Invoke-Msi([string[]] $Arguments, [string] $LogName, [int] $Expected = 0) {
    $log = Join-Path $logDirectory "$LogName.log"
    $process = Start-Process msiexec.exe -ArgumentList ($Arguments + @('/qn', '/norestart', '/l*v', "`"$log`"")) -WindowStyle Hidden -PassThru
    try {
        if (-not $process.WaitForExit(120000)) {
            $process.Kill()
            throw "msiexec timed out; log: $log"
        }
        Assert ($process.ExitCode -eq $Expected) "msiexec exit $($process.ExitCode), expected $Expected; log: $log"
    }
    finally { $process.Dispose() }
}
function Assert-ACL([string] $Path) {
    $acl = Get-Acl -LiteralPath $Path
    Assert $acl.AreAccessRulesProtected "inheritance disabled on $Path"
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    Assert ($rules.Count -eq 2) "only two ACL entries on $Path"
    foreach ($sid in @('S-1-5-18', 'S-1-5-32-544')) {
        $rule = @($rules | Where-Object { $_.IdentityReference.Value -eq $sid })
        Assert ($rule.Count -eq 1 -and $rule[0].AccessControlType -eq 'Allow' -and
            $rule[0].FileSystemRights -eq 'FullControl' -and
            ($rule[0].InheritanceFlags -band [Security.AccessControl.InheritanceFlags]::ContainerInherit) -and
            ($rule[0].InheritanceFlags -band [Security.AccessControl.InheritanceFlags]::ObjectInherit)) "full inheritable control for $sid on $Path"
    }
}
function Assert-Installed {
    Assert (Test-Path -LiteralPath $binary -PathType Leaf) 'installed executable'
    Assert (Test-Path -LiteralPath $license -PathType Leaf) 'installed MIT license'
    Assert ((Get-Content -Raw -LiteralPath $license).StartsWith('MIT License')) 'repository MIT notice'
    $service = Get-Service dscd
    try { $service.WaitForStatus('Running', [timespan]::FromSeconds(30)) }
    finally { $service.Dispose() }
    $registration = Get-CimInstance Win32_Service -Filter "Name='dscd'"
    Assert ($registration.StartName -eq 'LocalSystem' -and $registration.StartMode -eq 'Auto') 'LocalSystem automatic startup'
    $expectedCommand = '"{0}" -config-dir "{1}\." -results-dir "{2}\."' -f $binary, $config, $results
    Assert ($registration.PathName -ceq $expectedCommand) 'quoted service paths with default DSC lookup'
    Assert (-not (Test-Path $settingsKey)) 'no persisted DSC path'
    foreach ($directory in @($data, $config, $results)) {
        Assert (Test-Path -LiteralPath $directory -PathType Container) "directory $directory"
        Assert-ACL $directory
    }
    Assert ([Diagnostics.EventLog]::SourceExists('dscd')) 'Application Event Log source'
    $serviceSettings = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\dscd'
    Assert ($serviceSettings.FailureActionsOnNonCrashFailures -eq 1) 'native MSI persists the non-crash failure recovery flag'
    $failureActions = $serviceSettings.FailureActions
    Assert ($failureActions -and [BitConverter]::ToUInt32($failureActions, 0) -eq 86400) 'recovery reset after one day'
    # The registry's SERVICE_FAILURE_ACTIONS data contains action/delay pairs.
    Assert ([BitConverter]::ToUInt32($failureActions, 12) -eq 3) 'three recovery actions'
    $offset = [BitConverter]::ToUInt32($failureActions, 16)
    foreach ($index in 0..2) {
        Assert ([BitConverter]::ToUInt32($failureActions, $offset + $index * 8) -eq 1 -and
            [BitConverter]::ToUInt32($failureActions, $offset + $index * 8 + 4) -eq 5000) 'restart after five seconds'
    }
}
function Assert-Uninstalled {
    Assert (-not (Get-Service dscd -ErrorAction SilentlyContinue)) 'service removed'
    Assert (-not (Test-Path -LiteralPath $binary)) 'executable removed'
    Assert (-not (Test-Path -LiteralPath $license)) 'package-owned license removed'
    Assert (-not (Test-Path $eventKey)) 'Event Log source removed'
    Assert (-not (Test-Path $settingsKey)) 'MSI settings removed'
    Assert ((Get-Content -Raw -LiteralPath (Join-Path $config 'retained.txt')) -ceq 'configuration marker') 'configuration data preserved'
    Assert ((Get-Content -Raw -LiteralPath (Join-Path $results 'retained.txt')) -ceq 'result marker') 'result data preserved'
    foreach ($directory in @($data, $config, $results)) { Assert-ACL $directory }
    Assert (Test-Path -LiteralPath $dscExecutable) 'DSC left installed'
}

try {
    Invoke-Msi @('/i', "`"$MsiPath`"") 'install'
    $installedMsi = $MsiPath
    Assert-Installed
    $service = Get-Service dscd
    try {
        Stop-Service dscd
        $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
        # Verify missing DSC at the service boundary. A negative MSI install can
        # remain in native StartServices beyond the test's two-minute limit.
        $hiddenExecutable = "$dscExecutable.unavailable"
        Move-Item -LiteralPath $dscExecutable -Destination $hiddenExecutable
        try {
            $missingStarted = Get-Date
            $rejected = $false
            try { Start-Service dscd -ErrorAction Stop } catch { $rejected = $true }
            Assert $rejected 'service startup rejects missing DSC on PATH'
            $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
            $failures = @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'dscd'; StartTime = $missingStarted })
            Assert ($failures.Message -match 'resolve DSC executable') 'missing DSC produces an actionable startup error'
        }
        finally { Move-Item -LiteralPath $hiddenExecutable -Destination $dscExecutable }
        Start-Service dscd
        $service.WaitForStatus('Running', [timespan]::FromSeconds(30))
    }
    finally { $service.Dispose() }
    $oldProcess = (Get-CimInstance Win32_Service -Filter "Name='dscd'").ProcessId
    Stop-Process -Id $oldProcess -Force
    $deadline = [Diagnostics.Stopwatch]::StartNew()
    do {
        Start-Sleep -Milliseconds 250
        $service = Get-CimInstance Win32_Service -Filter "Name='dscd'"
    } while ($deadline.Elapsed.TotalSeconds -lt 30 -and
        ($service.State -ne 'Running' -or $service.ProcessId -eq $oldProcess))
    Assert ($service.State -eq 'Running' -and $service.ProcessId -ne $oldProcess) 'unexpected termination restarts the daemon'
    $events = @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'dscd'; StartTime = $started })
    Assert (($events.Message -match 'dscd started') -and ($events.Message -match 'dscd stopped')) 'structured lifecycle events'
    # .txt files remain opaque user data and do not trigger DSC execution.
    [IO.File]::WriteAllText((Join-Path $config 'retained.txt'), 'configuration marker')
    [IO.File]::WriteAllText((Join-Path $results 'retained.txt'), 'result marker')
    if ($UpgradeMsiPath) {
        $productInspector = New-Object -ComObject WindowsInstaller.Installer
        $versions = @()
        $codes = @()
        foreach ($path in @($MsiPath, $UpgradeMsiPath)) {
            $db = $productInspector.OpenDatabase($path, 0)
            foreach ($name in @('ProductVersion', 'ProductCode')) {
                $view = $db.OpenView("SELECT ``Value`` FROM ``Property`` WHERE ``Property``='$name'")
                $view.Execute()
                $record = $view.Fetch()
                if ($name -eq 'ProductVersion') { $versions += [version]$record.StringData(1) }
                else { $codes += $record.StringData(1) }
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record)
                $view.Close()
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
            }
            [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($db)
        }
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($productInspector)
        Assert ($versions[1] -gt $versions[0] -and $codes[1] -ne $codes[0]) 'upgrade requires higher version and new ProductCode'
        # PE overlay bytes are ignored by Windows. A unique old-payload marker
        # proves replacement even when both test MSIs were built from one binary.
        Stop-Service dscd
        $service = Get-Service dscd
        try { $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40)) }
        finally { $service.Dispose() }
        [IO.File]::AppendAllText($binary, "MSI upgrade replacement probe $([guid]::NewGuid())")
        $oldFingerprint = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
        Start-Service dscd
        Assert-Installed
        # Upgrades continue using the service's machine PATH.
        Invoke-Msi @('/i', "`"$UpgradeMsiPath`"") 'upgrade'
        $installedMsi = $UpgradeMsiPath
        $currentMsi = $UpgradeMsiPath
        Assert-Installed
        Assert ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -cne $oldFingerprint) 'upgrade replaces the old executable payload'
        Assert ((Get-Content -Raw -LiteralPath (Join-Path $config 'retained.txt')) -ceq 'configuration marker') 'configuration data survives upgrade'
        Assert ((Get-Content -Raw -LiteralPath (Join-Path $results 'retained.txt')) -ceq 'result marker') 'result data survives upgrade'
        Invoke-Msi @('/i', "`"$MsiPath`"") 'downgrade-rejected' 1603
        Assert-Installed
        Write-Output 'MSI upgrade, PATH-based DSC lookup, data preservation, and downgrade rejection passed.'
    }
    else {
        Write-Output 'Upgrade tests skipped: supply -UpgradeMsiPath to enable them.'
    }
    Stop-Service dscd
    (Get-Service dscd).WaitForStatus('Stopped', [timespan]::FromSeconds(40))
    Remove-Item -LiteralPath $binary
    Invoke-Msi @('/fa', "`"$currentMsi`"") 'repair'
    Assert-Installed
    Invoke-Msi @('/x', "`"$currentMsi`"") 'uninstall'
    $installedMsi = $null
    Assert-Uninstalled
    Invoke-Msi @('/i', "`"$currentMsi`"") 'reinstall'
    $installedMsi = $currentMsi
    Assert-Installed
    Invoke-Msi @('/x', "`"$currentMsi`"") 'final-uninstall'
    $installedMsi = $null
    Assert-Uninstalled
    Write-Output "MSI prerequisite, install, recovery, repair, reinstall, and uninstall passed. Logs: $logDirectory"
}
finally {
    if ($installedMsi) { Invoke-Msi @('/x', "`"$installedMsi`"") 'cleanup-uninstall' }
    # Logs and retained data are intentionally left on this disposable machine.
    # No recursive deletion or machine-state reversal is part of the installer.
}
