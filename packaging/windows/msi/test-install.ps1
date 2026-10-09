# Opt-in machine-wide tests, never run by ordinary Go tests.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $MsiPath,
    [string] $UpgradeMsiPath,
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $ArchivePath,
    [switch] $Disposable
)

$ErrorActionPreference = 'Stop'
if (-not $IsWindows -or -not $Disposable) {
    throw 'Requires Windows and explicit -Disposable consent.'
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run as administrator on a disposable Windows runner.'
}

$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path
if ($UpgradeMsiPath) {
    $UpgradeMsiPath = (Resolve-Path -LiteralPath $UpgradeMsiPath).Path
}

$programFiles = [Environment]::GetFolderPath('ProgramFiles')
$data = Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'dsc'
$binary = Join-Path $programFiles 'dscd\dscd.exe'
$license = Join-Path $programFiles 'dscd\LICENSE'
$dscDirectory = Join-Path $programFiles 'dscd\dsc'
$dscExecutable = Join-Path $dscDirectory 'dsc.exe'
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
$testStage = Join-Path ([IO.Path]::GetTempPath()) ('dscd-msi-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
$installedMsi = $null
$currentMsi = $MsiPath
$started = Get-Date
$archiveDirectory = Join-Path $testStage 'archive'
$archiveRoot = Join-Path $archiveDirectory "dscd-$Version-windows-amd64"
$archiveBinary = Join-Path $archiveRoot 'bin\dscd.exe'
$expectedDSCDirectory = Join-Path $archiveRoot 'bin\dsc'
$machinePath = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
$userPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
$processPath = $env:PATH
$existingDSC = @(Get-Command dsc.exe -CommandType Application -All -ErrorAction SilentlyContinue |
    ForEach-Object { [pscustomobject]@{ Path = $_.Source; Hash = (Get-FileHash -LiteralPath $_.Source).Hash } })
# A separate complete distribution proves uninstall never removes standalone
# DSC. Neither the test nor the package adds it to the machine or user PATH.
$standaloneDSC = Join-Path $testStage 'standalone DSC'
$expectedInstalledRoot = $null

function Assert([bool] $Condition, [string] $Message) {
    if (-not $Condition) {
        throw "MSI smoke test: $Message"
    }
}

function Invoke-Msi([string[]] $Arguments, [string] $LogName, [int] $Expected = 0) {
    $log = Join-Path $logDirectory "$LogName.log"
    $process = Start-Process msiexec.exe -ArgumentList ($Arguments + @('/qn', '/norestart', '/l*v', "`"$log`"")) -WindowStyle Hidden -PassThru
    try {
        if (-not $process.WaitForExit(120000)) {
            $process.Kill()
            [void]$process.WaitForExit(10000)
            throw "msiexec timed out; log: $log"
        }
        Assert ($process.ExitCode -eq $Expected) "msiexec exit $($process.ExitCode), expected $Expected; log: $log"
    }
    finally {
        $process.Dispose()
    }
}

function File-Hashes([string] $Root) {
    $hashes = @{}
    foreach ($file in (Get-ChildItem -LiteralPath $Root -File -Recurse -Force)) {
        $hashes[[IO.Path]::GetRelativePath($Root, $file.FullName)] =
            (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
    }
    return $hashes
}

function Assert-Payload([string] $Actual, [string] $Expected) {
    $actualHashes = File-Hashes $Actual
    $expectedHashes = File-Hashes $Expected
    Assert ($actualHashes.Count -eq $expectedHashes.Count) "complete file set in $Actual"
    foreach ($relative in $expectedHashes.Keys) {
        Assert ($actualHashes[$relative] -ceq $expectedHashes[$relative]) "exact payload $relative"
    }
}

function Assert-Standalone {
    Assert ([Environment]::GetEnvironmentVariable('PATH', 'Machine') -ceq $machinePath) 'machine PATH unchanged'
    Assert ([Environment]::GetEnvironmentVariable('PATH', 'User') -ceq $userPath) 'user PATH unchanged'
    Assert ($env:PATH -ceq $processPath) 'process PATH unchanged'
    Assert-Payload $standaloneDSC $expectedDSCDirectory
    foreach ($entry in $existingDSC) {
        Assert ((Get-FileHash -LiteralPath $entry.Path).Hash -ceq $entry.Hash) "pre-existing standalone DSC preserved: $($entry.Path)"
    }
}

function Extract-Msi([string] $Path, [string] $Name) {
    $directory = Join-Path $testStage $Name
    Invoke-Msi @('/a', "`"$Path`"", "TARGETDIR=`"$directory`"") "$Name-extract"
    $daemon = @(Get-ChildItem -LiteralPath $directory -Filter dscd.exe -File -Recurse)
    Assert ($daemon.Count -eq 1) 'one administratively extracted daemon'
    return $daemon[0].DirectoryName
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
    if ($Version -and $currentMsi -eq $MsiPath) {
        $reportedVersion = & $binary --version
        Assert ($LASTEXITCODE -eq 0 -and $reportedVersion -ceq "dscd $Version") 'installed release version'
        if ($expectedHash) {
            Assert ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ceq $expectedHash) 'MSI and ZIP contain the same executable'
        }
        Assert-Payload $dscDirectory (Join-Path $expectedInstalledRoot 'dsc')
        Assert ((Get-FileHash -LiteralPath $binary).Hash -ceq
            (Get-FileHash -LiteralPath (Join-Path $expectedInstalledRoot 'dscd.exe')).Hash) 'installed daemon exactly matches current MSI'
        $dscVersion = & $dscExecutable --version
        Assert ($LASTEXITCODE -eq 0 -and ($dscVersion -join ' ') -match '\b3\.3\.0\b') 'bundled DSC 3.3.0 runs'
        Assert-Standalone
    }

    $service = Get-Service dscd
    try {
        $service.WaitForStatus('Running', [timespan]::FromSeconds(30))
    }
    finally {
        $service.Dispose()
    }

    $registration = Get-CimInstance Win32_Service -Filter "Name='dscd'"
    Assert ($registration.StartName -eq 'LocalSystem' -and $registration.StartMode -eq 'Auto') 'LocalSystem automatic startup'
    $expectedCommand = '"{0}" -config-dir "{1}\." -results-dir "{2}\."' -f $binary, $config, $results
    Assert ($registration.PathName -ceq $expectedCommand) 'quoted service paths with default bundled DSC lookup'
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
    Assert (Test-Path -LiteralPath (Join-Path $config 'package-smoke.json')) 'reconciled configuration preserved'
    Assert (Test-Path -LiteralPath (Join-Path $results 'package-smoke.json.result.json')) 'DSC result preserved'
    Assert (Test-Path -LiteralPath (Join-Path $config 'package-registry.json')) 'registry audit configuration preserved'
    Assert (Test-Path -LiteralPath (Join-Path $results 'package-registry.json.result.json')) 'registry audit result preserved'
    foreach ($directory in @($data, $config, $results)) {
        Assert-ACL $directory
    }

    Assert (@(Get-ChildItem -LiteralPath $dscDirectory -File -Recurse -Force -ErrorAction SilentlyContinue).Count -eq 0) 'all MSI-owned DSC files removed'
    Assert-Standalone
}

function Assert-Reconciliation {
    $service = Get-Service dscd
    try {
        Stop-Service dscd
        $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
    }
    finally {
        $service.Dispose()
    }

    $resultPath = Join-Path $results 'package-smoke.json.result.json'
    $registryResultPath = Join-Path $results 'package-registry.json.result.json'
    foreach ($path in @($resultPath, $registryResultPath)) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path
        }
    }

    $document = '{"$schema":"https://aka.ms/dsc/schemas/v3/bundled/config/document.json","resources":[{"name":"Package smoke","type":"Microsoft.DSC.Debug/Echo","properties":{"output":"dscd package smoke"}}]}'
    [IO.File]::WriteAllText((Join-Path $config 'package-smoke.json'), $document)
    # Unlike Echo, Registry needs its shipped manifest and registry.exe.
    # Audit an existing machine value: the test does not change OS state.
    $productName = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').ProductName
    $registryDocument = @{
        '$schema' = 'https://aka.ms/dsc/schemas/v3/bundled/config/document.json'
        metadata = @{ dscd = @{ operation = 'test' } }
        resources = @(@{
            name = 'Bundled Registry audit'
            type = 'Microsoft.Windows/Registry'
            properties = @{
                keyPath = 'HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
                valueName = 'ProductName'
                valueData = @{ String = $productName }
            }
        })
    } | ConvertTo-Json -Depth 10
    [IO.File]::WriteAllText((Join-Path $config 'package-registry.json'), $registryDocument)
    Start-Service dscd
    $deadline = [Diagnostics.Stopwatch]::StartNew()
    while ((-not (Test-Path -LiteralPath $resultPath) -or -not (Test-Path -LiteralPath $registryResultPath)) -and
        $deadline.Elapsed.TotalSeconds -lt 60) {
        Start-Sleep -Milliseconds 500
    }

    Assert (Test-Path -LiteralPath $resultPath) 'reconciliation produced a fresh result'
    $result = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json
    Assert ($result.configuration -ceq 'package-smoke.json' -and $result.outcome -ceq 'succeeded') 'successful DSC reconciliation'
    Assert ($result.dscResult.hadErrors -eq $false -and $result.dscResult.results.Count -eq 1) 'one successful resource'
    Assert ($result.dscResult.results[0].result.afterState.output -ceq 'dscd package smoke') 'actual DSC Echo execution'
    Assert (Test-Path -LiteralPath $registryResultPath) 'bundled Registry reconciliation produced a result'
    $registryResult = Get-Content -Raw -LiteralPath $registryResultPath | ConvertFrom-Json
    Assert ($registryResult.outcome -ceq 'succeeded' -and $registryResult.operation -ceq 'test' -and
        $registryResult.dscResult.hadErrors -eq $false -and $registryResult.dscResult.results.Count -eq 1) 'actual bundled Registry audit'
    Assert ($registryResult.dscResult.results[0].result.inDesiredState -eq $true -and
        $registryResult.dscResult.results[0].result.actualState.valueData.String -ceq $productName) 'Registry manifest and helper executable work under LocalSystem'
}

function Assert-ZIP {
    $directory = Join-Path $testStage 'ZIP foreground with spaces'
    $inputDirectory = Join-Path $directory 'config'
    $outputDirectory = Join-Path $directory 'results'
    $customResourceDirectory = Join-Path $directory 'custom resources'
    New-Item -ItemType Directory -Path $inputDirectory, $outputDirectory, $customResourceDirectory -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $config 'package-smoke.json'), (Join-Path $config 'package-registry.json') `
        -Destination $inputDirectory
    $archiveDSC = Join-Path (Split-Path $archiveBinary) 'dsc'
    $customManifest = Get-Content -Raw -LiteralPath (Join-Path $archiveDSC 'echo.dsc.resource.json') | ConvertFrom-Json
    $customManifest.type = 'DSCD.Package/CustomDiscovery'
    foreach ($operation in @('get', 'set', 'test', 'export')) {
        $customManifest.$operation.executable = Join-Path $archiveDSC 'dscecho.exe'
    }
    $customManifest.schema.command.executable = Join-Path $archiveDSC 'dscecho.exe'
    [IO.File]::WriteAllText((Join-Path $customResourceDirectory 'custom.dsc.resource.json'),
        ($customManifest | ConvertTo-Json -Depth 10))
    $customDocument = '{"$schema":"https://aka.ms/dsc/schemas/v3/bundled/config/document.json","resources":[{"name":"Custom discovery","type":"DSCD.Package/CustomDiscovery","properties":{"output":"custom discovery preserved"}}]}'
    [IO.File]::WriteAllText((Join-Path $inputDirectory 'package-custom.json'), $customDocument)
    # DSC_RESOURCE_PATH replaces manifest discovery through PATH. Both this
    # custom manifest and the bundled Registry manifest must remain discoverable.
    $resourcePath = $env:DSC_RESOURCE_PATH
    try {
        $env:DSC_RESOURCE_PATH = $customResourceDirectory
        $process = Start-Process -FilePath $archiveBinary -ArgumentList @('-config-dir', "`"$inputDirectory`"",
            '-results-dir', "`"$outputDirectory`"", '-interval', '1h') -WorkingDirectory $directory -PassThru `
            -RedirectStandardOutput (Join-Path $logDirectory 'zip-stdout.log') -RedirectStandardError (Join-Path $logDirectory 'zip-stderr.log')
    }
    finally {
        $env:DSC_RESOURCE_PATH = $resourcePath
    }
    try {
        $deadline = [Diagnostics.Stopwatch]::StartNew()
        $resultFiles = @('package-smoke.json.result.json', 'package-registry.json.result.json', 'package-custom.json.result.json')
        foreach ($name in $resultFiles) {
            $resultPath = Join-Path $outputDirectory $name
            while (-not (Test-Path -LiteralPath $resultPath) -and $deadline.Elapsed.TotalSeconds -lt 60 -and
                -not $process.HasExited) {
                Start-Sleep -Milliseconds 250
            }
            Assert (Test-Path -LiteralPath $resultPath) "ZIP foreground result $name"
            $result = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json
            Assert ($result.outcome -ceq 'succeeded' -and $result.dscResult.hadErrors -eq $false) "ZIP bundled resource execution $name"
            if ($name -ceq 'package-custom.json.result.json') {
                Assert ($result.dscResult.results[0].result.afterState.output -ceq 'custom discovery preserved') 'inherited custom manifest discovery retained'
            }
        }
        Assert (-not $process.HasExited) 'ZIP daemon continues foreground execution'
    }
    finally {
        if (-not $process.HasExited) {
            $process.Kill()
            Assert ($process.WaitForExit(40000)) 'ZIP foreground daemon terminates'
        }
        $process.Dispose()
    }
    Assert-Standalone
}

try {
    New-Item -ItemType Directory -Path $testStage -Force | Out-Null
    Expand-Archive -LiteralPath $ArchivePath -DestinationPath $archiveDirectory
    $expectedHash = (Get-FileHash -LiteralPath $archiveBinary -Algorithm SHA256).Hash
    Copy-Item -LiteralPath $expectedDSCDirectory -Destination $standaloneDSC -Recurse
    & "$PSScriptRoot\inspect.ps1" -MsiPath $MsiPath -Version $Version -ArchivePath $ArchivePath
    $expectedInstalledRoot = Extract-Msi $MsiPath 'expected-install'
    Assert-Payload (Join-Path $expectedInstalledRoot 'dsc') $expectedDSCDirectory
    Invoke-Msi @('/i', "`"$MsiPath`"") 'install'
    $installedMsi = $MsiPath
    Assert-Installed
    Assert-Reconciliation
    Assert-ZIP
    $service = Get-Service dscd
    try {
        Stop-Service dscd
        $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
        # A present but unusable bundle must not silently fall back to PATH.
        $hiddenExecutable = "$dscExecutable.unavailable"
        Move-Item -LiteralPath $dscExecutable -Destination $hiddenExecutable
        try {
            New-Item -ItemType Directory -Path $dscExecutable | Out-Null
            $missingStarted = Get-Date
            $rejected = $false
            try {
                Start-Service dscd -ErrorAction Stop
            }
            catch {
                $rejected = $true
            }

            Assert $rejected 'service startup rejects an unusable bundled DSC executable'
            $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
            $failures = @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'dscd'; StartTime = $missingStarted })
            Assert (@($failures.Message -match 'DSC executable').Count -gt 0) 'unusable bundled DSC produces an actionable startup error'
        }
        finally {
            if (Test-Path -LiteralPath $dscExecutable -PathType Container) {
                Remove-Item -LiteralPath $dscExecutable
            }
            Move-Item -LiteralPath $hiddenExecutable -Destination $dscExecutable
        }

        Start-Service dscd
        $service.WaitForStatus('Running', [timespan]::FromSeconds(30))
    }
    finally {
        $service.Dispose()
    }

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
                if ($name -eq 'ProductVersion') {
                    $versions += [version]$record.StringData(1)
                }
                else {
                    $codes += $record.StringData(1)
                }

                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record)
                $view.Close()
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
            }
            [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($db)
        }
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($productInspector)
        Assert ($versions[1] -gt $versions[0] -and $codes[1] -ne $codes[0]) 'upgrade requires higher version and new ProductCode'
        $oldFingerprint = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
        $expectedInstalledRoot = Extract-Msi $UpgradeMsiPath 'expected-upgrade'
        Invoke-Msi @('/i', "`"$UpgradeMsiPath`"") 'upgrade'
        $installedMsi = $UpgradeMsiPath
        $currentMsi = $UpgradeMsiPath
        Assert-Installed
        Assert ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -cne $oldFingerprint) 'upgrade replaces the old executable payload'
        Assert ((Get-Content -Raw -LiteralPath (Join-Path $config 'retained.txt')) -ceq 'configuration marker') 'configuration data survives upgrade'
        Assert ((Get-Content -Raw -LiteralPath (Join-Path $results 'retained.txt')) -ceq 'result marker') 'result data survives upgrade'
        Invoke-Msi @('/i', "`"$MsiPath`"") 'downgrade-rejected' 1603
        Assert-Installed
        Write-Output 'MSI upgrade, exact bundled payload, data preservation, and downgrade rejection passed.'
        Assert-Reconciliation
    }
    else {
        Write-Output 'Upgrade tests skipped: supply -UpgradeMsiPath to enable them.'
    }
    Stop-Service dscd
    (Get-Service dscd).WaitForStatus('Stopped', [timespan]::FromSeconds(40))
    Remove-Item -LiteralPath $binary
    Remove-Item -LiteralPath $dscExecutable
    Remove-Item -LiteralPath (Join-Path $dscDirectory 'registry.exe')
    Remove-Item -LiteralPath (Join-Path $dscDirectory 'registry.dsc.manifests.json')
    Invoke-Msi @('/fa', "`"$currentMsi`"") 'repair'
    Assert-Installed
    Assert-Reconciliation
    Invoke-Msi @('/x', "`"$currentMsi`"") 'uninstall'
    $installedMsi = $null
    Assert-Uninstalled
    Invoke-Msi @('/i', "`"$currentMsi`"") 'reinstall'
    $installedMsi = $currentMsi
    Assert-Installed
    Invoke-Msi @('/x', "`"$currentMsi`"") 'final-uninstall'
    $installedMsi = $null
    Assert-Uninstalled
    Write-Output "MSI bundled payload, install, recovery, repair, reinstall, and uninstall passed. Logs: $logDirectory"
}
finally {
    try {
        Get-WinEvent -FilterHashtable @{ LogName = 'Application'; StartTime = $started } -ErrorAction Continue |
            Where-Object ProviderName -eq 'dscd' |
            Format-List TimeCreated, Id, Message | Out-File (Join-Path $logDirectory 'events.log')
    }
    finally {
        try {
            if ($installedMsi) {
                Invoke-Msi @('/x', "`"$installedMsi`"") 'cleanup-uninstall'
            }
        }
        finally {
            if (Test-Path -LiteralPath $testStage) {
                Remove-Item -LiteralPath $testStage -Recurse -Force
            }
        }
    }

    # Only logs and retained data are left on this disposable machine.
    # No recursive deletion or machine-state reversal is part of the installer.
}
