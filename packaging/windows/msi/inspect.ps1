[CmdletBinding(DefaultParameterSetName = 'Archive')]
param(
    [Parameter(Mandatory)][string] $MsiPath,
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory, ParameterSetName = 'Archive')][string] $ArchivePath,
    [Parameter(Mandatory, ParameterSetName = 'Staged')][string] $DSCDirectory
)

$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'MSI inspection requires Windows Installer on Windows.'
}

$expectedVersion = & "$PSScriptRoot/version.ps1" -Version $Version
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $installer.OpenDatabase((Resolve-Path -LiteralPath $MsiPath).Path, 0)
$stage = Join-Path ([IO.Path]::GetTempPath()) ('dscd-inspect-' + [guid]::NewGuid().ToString('N'))

function Assert([bool] $Condition, [string] $Message) {
    if (-not $Condition) {
        throw "MSI inspection: $Message"
    }
}

function Rows([string] $Sql, [string[]] $Columns) {
    $view = $database.OpenView($Sql)
    try {
        [void]$view.Execute()
        while ($record = $view.Fetch()) {
            try {
                $row = @{}
                for ($i = 0; $i -lt $Columns.Count; $i++) {
                    $row[$Columns[$i]] = $record.StringData($i + 1)
                }
                [pscustomobject]$row
            }
            finally {
                [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record)
            }
        }
    }
    finally {
        [void]$view.Close()
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}

function File-Hashes([string] $Root) {
    $hashes = @{}
    foreach ($file in (Get-ChildItem -LiteralPath $Root -File -Recurse -Force)) {
        $relative = [IO.Path]::GetRelativePath($Root, $file.FullName)
        $hashes[$relative] = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
    }
    return $hashes
}

try {
    New-Item -ItemType Directory -Path $stage | Out-Null
    if ($ArchivePath) {
        Expand-Archive -LiteralPath $ArchivePath -DestinationPath (Join-Path $stage 'zip')
        $archiveRoot = Join-Path $stage "zip\dscd-$Version-windows-amd64"
        $expectedDSC = Join-Path $archiveRoot 'bin\dsc'
    }
    else {
        $expectedDSC = (Resolve-Path -LiteralPath $DSCDirectory).Path
    }
    Assert (Test-Path -LiteralPath (Join-Path $expectedDSC 'dsc.exe') -PathType Leaf) 'expected complete DSC payload'
    Assert (Test-Path -LiteralPath (Join-Path $expectedDSC 'NOTICE.txt') -PathType Leaf) 'upstream notices retained'
    $expectedLicenseHash = '7c77a44a8acd9b41fdc209864a8016b3d430b5d0e09309818d5b7444336df744'
    Assert ((Get-FileHash -LiteralPath (Join-Path $expectedDSC 'LICENSE') -Algorithm SHA256).Hash.ToLowerInvariant() -ceq
        $expectedLicenseHash) 'verified upstream DSC MIT license'
    $expectedHashes = File-Hashes $expectedDSC
    $summary = $database.SummaryInformation(0)

    try {
        Assert ($summary.Property(7) -eq 'x64;1033') 'package must target x64'
    }
    finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($summary)
    }

    $properties = @{}
    foreach ($row in (Rows 'SELECT `Property`, `Value` FROM `Property`' @('Name', 'Value'))) {
        $properties[$row.Name] = $row.Value
    }

    Assert ($properties.ProductName -eq 'DSC Reconciliation Daemon') 'product name'
    Assert ($properties.Manufacturer -eq 'dsc-reconciler contributors') 'publisher'
    Assert ($properties.ProductVersion -eq $expectedVersion) 'product version'
    Assert ($properties.UpgradeCode -eq '{536AF323-17B1-40ED-870C-751F063452AA}') 'stable UpgradeCode'
    Assert ($properties.ALLUSERS -eq '1') 'per-machine installation'
    Assert ($properties.ARPCOMMENTS -eq "dscd $Version") 'human-readable release version'
    $tables = @(Rows 'SELECT `Name` FROM `_Tables`' @('Name'))

    foreach ($table in @('AppSearch', 'DrLocator', 'RegLocator', 'Signature', 'Environment')) {
        Assert ($table -notin $tables.Name) "no prerequisite discovery or PATH modification ($table)"
    }

    foreach ($name in @($properties.Keys) + ($properties.SecureCustomProperties -split ';')) {
        Assert ($name -notmatch '(?i)dsc') "no DSC path property ($name)"
    }

    foreach ($row in (Rows 'SELECT `Condition` FROM `LaunchCondition`' @('Condition'))) {
        Assert ($row.Condition.Length -le 255) 'launch conditions fit the MSI column limit'
        Assert ($row.Condition -notmatch '(?i)dsc') 'no DSC prerequisite launch condition'
    }

    $files = @(Rows 'SELECT `File`, `Component_`, `FileName`, `Version`, `Language` FROM `File`' @('Id', 'Component', 'Name', 'Version', 'Language'))
    Assert ($files.Count -eq 2 + $expectedHashes.Count) 'daemon, repository license and complete DSC payload only'
    $executable = @($files | Where-Object Id -eq 'DaemonFile')
    $license = @($files | Where-Object Id -eq 'LicenseFile')
    Assert ($executable.Count -eq 1 -and ($executable[0].Name -split '\|')[-1] -eq 'dscd.exe') 'daemon payload'
    Assert ($executable[0].Version -ceq "$expectedVersion.0") 'release file version must force replacement of Go PE binaries'
    Assert ($executable[0].Language -ceq '0') 'language-neutral versioned file metadata must satisfy ICE60'
    Assert ($license.Count -eq 1 -and ($license[0].Name -split '\|')[-1] -ceq 'LICENSE' -and
        $license[0].Component -eq 'License') 'repository license payload'
    $media = @(Rows 'SELECT `Cabinet` FROM `Media`' @('Cabinet'))
    Assert ($media.Count -gt 0 -and @($media | Where-Object { -not $_.Cabinet.StartsWith('#') }).Count -eq 0) 'all cabinets embedded'
    $directories = @(Rows 'SELECT `Directory`, `Directory_Parent`, `DefaultDir` FROM `Directory`' @('Id', 'Parent', 'Name'))
    foreach ($expected in @(
        @('INSTALLFOLDER', 'ProgramFiles64Folder', 'dscd'),
        @('DSCFOLDER', 'INSTALLFOLDER', 'dsc'),
        @('DATAFOLDER', 'CommonAppDataFolder', 'dsc'),
        @('CONFIGFOLDER', 'DATAFOLDER', 'config.d'),
        @('RESULTSFOLDER', 'DATAFOLDER', 'results.d'))) {
        $dir = @($directories | Where-Object Id -eq $expected[0])
        Assert ($dir.Count -eq 1 -and $dir[0].Parent -eq $expected[1] -and
            ($dir[0].Name -split '\|')[-1] -eq $expected[2]) "directory $($expected[0])"
    }

    $components = @(Rows 'SELECT `Component`, `Directory_`, `Attributes`, `KeyPath` FROM `Component`' @('Id', 'Directory', 'Attributes', 'KeyPath'))
    $daemon = @($components | Where-Object Id -eq 'Daemon')
    Assert ($daemon.Count -eq 1 -and $daemon[0].Directory -eq 'INSTALLFOLDER' -and
        $daemon[0].KeyPath -eq 'DaemonFile' -and ([int]$daemon[0].Attributes -band 256)) 'x64 executable component'
    $licenseComponent = @($components | Where-Object Id -eq 'License')
    Assert ($licenseComponent.Count -eq 1 -and $licenseComponent[0].Directory -eq 'INSTALLFOLDER' -and
        $licenseComponent[0].KeyPath -eq 'LicenseFile') 'license installed alongside executable'
    $directoryPaths = @{ DSCFOLDER = '' }
    function DSC-RelativeDirectory([string] $Id) {
        if ($directoryPaths.ContainsKey($Id)) {
            return $directoryPaths[$Id]
        }
        $directory = @($directories | Where-Object Id -eq $Id)
        if ($directory.Count -ne 1 -or -not $directory[0].Parent) {
            return $null
        }
        $parent = DSC-RelativeDirectory $directory[0].Parent
        if ($null -eq $parent) {
            return $null
        }
        $name = (($directory[0].Name -split ':')[0] -split '\|')[-1]
        $relative = if ($parent) { Join-Path $parent $name } else { $name }
        $directoryPaths[$Id] = $relative
        return $relative
    }
    $bundledFiles = @{}
    foreach ($file in ($files | Where-Object { $_.Id -notin @('DaemonFile', 'LicenseFile') })) {
        $component = @($components | Where-Object Id -eq $file.Component)
        Assert ($component.Count -eq 1) "component for $($file.Id)"
        Assert ($component[0].KeyPath -eq $file.Id -and ([int]$component[0].Attributes -band 256) -and
            -not ([int]$component[0].Attributes -band 16)) 'one non-permanent x64 file-keyed component per DSC file'
        Assert (@($files | Where-Object Component -eq $file.Component).Count -eq 1) 'independent DSC file repair components'
        $relativeDirectory = DSC-RelativeDirectory $component[0].Directory
        Assert ($null -ne $relativeDirectory) 'all DSC files are under INSTALLFOLDER\dsc'
        $fileName = ($file.Name -split '\|')[-1]
        $relative = if ($relativeDirectory) { Join-Path $relativeDirectory $fileName } else { $fileName }
        Assert ($expectedHashes.ContainsKey($relative) -and -not $bundledFiles.ContainsKey($relative)) "DSC archive file $relative"
        $bundledFiles[$relative] = $true
    }
    Assert ($bundledFiles.Count -eq $expectedHashes.Count) 'all upstream files and licenses harvested'

    foreach ($id in @('DataDirectory', 'ConfigDirectory', 'ResultsDirectory')) {
        $component = @($components | Where-Object Id -eq $id)
        Assert ($component.Count -eq 1 -and ([int]$component[0].Attributes -band 16)) "permanent $id"
    }
    $service = @(Rows 'SELECT `Name`, `DisplayName`, `ServiceType`, `StartType`, `StartName`, `Arguments` FROM `ServiceInstall`' @('Name', 'DisplayName', 'Type', 'Start', 'Account', 'Arguments'))
    Assert ($service.Count -eq 1 -and $service[0].Name -eq 'dscd' -and
        $service[0].DisplayName -eq 'DSC Reconciliation Daemon' -and
        [int]$service[0].Type -eq 16 -and [int]$service[0].Start -eq 2 -and
        $service[0].Account -eq 'LocalSystem') 'automatic LocalSystem service'
    Assert ($service[0].Arguments -ceq '-config-dir "[CONFIGFOLDER]." -results-dir "[RESULTSFOLDER]."') 'service uses daemon default bundled DSC lookup'
    $control = @(Rows 'SELECT `Name`, `Event`, `Wait` FROM `ServiceControl`' @('Name', 'Event', 'Wait'))
    Assert ($control.Count -eq 1 -and $control[0].Name -eq 'dscd' -and
        [int]$control[0].Event -eq 171 -and [int]$control[0].Wait -eq 1) 'native start, stop, uninstall'
    $recovery = @(Rows 'SELECT `ServiceName`, `FirstFailureActionType`, `SecondFailureActionType`, `ThirdFailureActionType`, `ResetPeriodInDays`, `RestartServiceDelayInSeconds` FROM `Wix4ServiceConfig`' @('Name', 'First', 'Second', 'Third', 'Reset', 'Delay'))
    Assert ($recovery.Count -eq 1 -and $recovery[0].Name -eq 'dscd' -and
        $recovery[0].First -eq 'restart' -and $recovery[0].Second -eq 'restart' -and
        $recovery[0].Third -eq 'restart' -and $recovery[0].Reset -eq '1' -and
        $recovery[0].Delay -eq '5') 'service recovery'
    $nonCrash = @(Rows 'SELECT `Name`, `Event`, `ConfigType`, `Argument`, `Component_` FROM `MsiServiceConfig`' @('Name', 'Event', 'Type', 'Argument', 'Component'))
    Assert ($nonCrash.Count -eq 1 -and $nonCrash[0].Name -eq 'dscd' -and
        $nonCrash[0].Event -eq '5' -and $nonCrash[0].Type -eq '4' -and
        $nonCrash[0].Argument -eq '1' -and $nonCrash[0].Component -eq 'Daemon') 'recovery on handled nonzero SCM exits'
    $sequence = @(Rows 'SELECT `Action`, `Sequence` FROM `InstallExecuteSequence`' @('Action', 'Sequence'))
    $installServices = [int]($sequence | Where-Object Action -eq 'InstallServices').Sequence
    $configureServices = [int]($sequence | Where-Object Action -eq 'MsiConfigureServices').Sequence
    $startServices = [int]($sequence | Where-Object Action -eq 'StartServices').Sequence
    Assert ($configureServices -gt $installServices -and $configureServices -lt $startServices) 'native failure flag configured before service startup'
    $initialize = [int]($sequence | Where-Object Action -eq 'InstallInitialize').Sequence
    $removeExisting = [int]($sequence | Where-Object Action -eq 'RemoveExistingProducts').Sequence
    $processComponents = [int]($sequence | Where-Object Action -eq 'ProcessComponents').Sequence
    Assert ($removeExisting -gt $initialize -and $removeExisting -lt $processComponents) 'transactional early major upgrade replaces unmodified unversioned DSC files'
    $permissions = @(Rows 'SELECT `LockObject`, `Table`, `SDDLText` FROM `MsiLockPermissionsEx`' @('Object', 'Table', 'SDDL'))
    Assert ($permissions.Count -eq 4) 'protected executable, parent, and data directories'
    $installPermission = @($permissions | Where-Object Object -eq 'INSTALLFOLDER')
    Assert ($installPermission.Count -eq 1 -and $installPermission[0].Table -eq 'CreateFolder' -and
        $installPermission[0].SDDL -ceq 'O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)') 'protected executable directory'

    foreach ($id in @('DATAFOLDER', 'CONFIGFOLDER', 'RESULTSFOLDER')) {
        $permission = @($permissions | Where-Object Object -eq $id)
        Assert ($permission.Count -eq 1 -and $permission[0].Table -eq 'CreateFolder' -and
            $permission[0].SDDL -ceq 'O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)') "protected ACL $id"
    }

    $registry = @(Rows 'SELECT `Root`, `Key`, `Name`, `Value` FROM `Registry`' @('Root', 'Key', 'Name', 'Value'))
    $eventKey = 'SYSTEM\CurrentControlSet\Services\EventLog\Application\dscd'
    foreach ($entry in @(@('EventMessageFile', '#%[System64Folder]EventCreate.exe'), @('TypesSupported', '#7'), @('CustomSource', '#1'))) {
        $value = @($registry | Where-Object { $_.Root -eq '2' -and $_.Key -eq $eventKey -and $_.Name -eq $entry[0] })
        Assert ($value.Count -eq 1 -and $value[0].Value -ceq $entry[1]) "Event Log $($entry[0])"
    }

    Assert (@($registry | Where-Object { $_.Key -ne $eventKey }).Count -eq 0) 'only Event Log registry entries; no DSC path persistence'
    Assert ('Settings' -notin $components.Id) 'no DSC settings component'
    $actions = @(Rows 'SELECT `Action`, `Type`, `Target` FROM `CustomAction`' @('Name', 'Type', 'Target'))
    foreach ($action in $actions) {
        Assert ($action.Target -in @('SchedServiceConfig', 'ExecServiceConfig', 'RollbackServiceConfig')) "unexpected custom action $($action.Name)"
    }

    # Administrative extraction expands cabinets without registering or starting
    # services. Compare bytes, not only table filenames or version metadata.
    $extracted = Join-Path $stage 'msi'
    $log = Join-Path $stage 'extract.log'
    $msi = (Resolve-Path -LiteralPath $MsiPath).Path
    $process = Start-Process msiexec.exe -ArgumentList @('/a', "`"$msi`"", '/qn', '/norestart',
        "TARGETDIR=`"$extracted`"", '/l*v', "`"$log`"") -PassThru -WindowStyle Hidden
    try {
        if (-not $process.WaitForExit(120000)) {
            $process.Kill()
            [void]$process.WaitForExit(10000)
            throw "Administrative extraction timed out: $log"
        }
        Assert ($process.ExitCode -eq 0) "administrative extraction exit $($process.ExitCode)"
    }
    finally {
        $process.Dispose()
    }
    $extractedDaemon = @(Get-ChildItem -LiteralPath $extracted -Filter dscd.exe -File -Recurse)
    Assert ($extractedDaemon.Count -eq 1) 'one extracted daemon'
    $extractedRoot = $extractedDaemon[0].DirectoryName
    $actualHashes = File-Hashes (Join-Path $extractedRoot 'dsc')
    Assert ($actualHashes.Count -eq $expectedHashes.Count) 'extracted DSC file count'
    foreach ($relative in $expectedHashes.Keys) {
        Assert ($actualHashes[$relative] -ceq $expectedHashes[$relative]) "exact upstream DSC bytes: $relative"
    }
    if ($ArchivePath) {
        foreach ($pair in @(@('dscd.exe', 'bin\dscd.exe'), @('LICENSE', 'LICENSE'))) {
            Assert ((Get-FileHash -LiteralPath (Join-Path $extractedRoot $pair[0])).Hash -ceq
                (Get-FileHash -LiteralPath (Join-Path $archiveRoot $pair[1])).Hash) "identical MSI/ZIP $($pair[0])"
        }
    }
    Write-Output "MSI tables and exact DSC payload comparison passed: $MsiPath"
}
catch {
    throw "MSI inspection failed: $($_.Exception.Message)`n$($_.ScriptStackTrace)"
}
finally {
    try {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
    }
    finally {
        if (Test-Path -LiteralPath $stage) {
            Remove-Item -LiteralPath $stage -Recurse -Force
        }
    }
}
