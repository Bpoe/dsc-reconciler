[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $MsiPath,
    [Parameter(Mandatory)][string] $Version
)
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'MSI inspection requires Windows Installer on Windows.' }
$expectedVersion = & "$PSScriptRoot/version.ps1" -Version $Version
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $installer.OpenDatabase((Resolve-Path -LiteralPath $MsiPath).Path, 0)
function Assert([bool] $Condition, [string] $Message) {
    if (-not $Condition) { throw "MSI inspection: $Message" }
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
            finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record) }
        }
    }
    finally {
        [void]$view.Close()
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}
try {
    $summary = $database.SummaryInformation(0)
    try { Assert ($summary.Property(7) -eq 'x64;1033') 'package must target x64' }
    finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($summary) }
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
    Assert ($properties.SecureCustomProperties.Split(';') -contains 'DSC_PATH') 'secure public DSC_PATH'
    foreach ($row in (Rows 'SELECT `Condition` FROM `LaunchCondition`' @('Condition'))) {
        Assert ($row.Condition.Length -le 255) 'launch conditions fit the MSI column limit'
    }
    $searchPaths = @(Rows 'SELECT `Path` FROM `DrLocator`' @('Path'))
    Assert (@($searchPaths | Where-Object Path -ceq '[DSC_SEARCH_DIR]').Count -eq 1) 'native explicit DSC directory search uses a formatted property'
    $files = @(Rows 'SELECT `File`, `Component_`, `FileName`, `Version`, `Language` FROM `File`' @('Id', 'Component', 'Name', 'Version', 'Language'))
    Assert ($files.Count -eq 2) 'only dscd.exe and LICENSE must be packaged'
    $executable = @($files | Where-Object Id -eq 'DaemonFile')
    $license = @($files | Where-Object Id -eq 'LicenseFile')
    Assert ($executable.Count -eq 1 -and ($executable[0].Name -split '\|')[-1] -eq 'dscd.exe') 'daemon payload'
    Assert ($executable[0].Version -ceq "$expectedVersion.0") 'release file version must force replacement of Go PE binaries'
    Assert ($executable[0].Language -ceq '0') 'language-neutral versioned file metadata must satisfy ICE60'
    Assert ($license.Count -eq 1 -and ($license[0].Name -split '\|')[-1] -ceq 'LICENSE' -and
        $license[0].Component -eq 'License') 'repository license payload'
    $media = @(Rows 'SELECT `Cabinet` FROM `Media`' @('Cabinet'))
    Assert ($media.Count -eq 1 -and $media[0].Cabinet.StartsWith('#')) 'embedded executable cabinet'
    $directories = @(Rows 'SELECT `Directory`, `Directory_Parent`, `DefaultDir` FROM `Directory`' @('Id', 'Parent', 'Name'))
    foreach ($expected in @(
        @('INSTALLFOLDER', 'ProgramFiles64Folder', 'dscd'),
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
    foreach ($id in @('DataDirectory', 'ConfigDirectory', 'ResultsDirectory')) {
        $component = @($components | Where-Object Id -eq $id)
        Assert ($component.Count -eq 1 -and ([int]$component[0].Attributes -band 16)) "permanent $id"
    }
    $service = @(Rows 'SELECT `Name`, `DisplayName`, `ServiceType`, `StartType`, `StartName`, `Arguments` FROM `ServiceInstall`' @('Name', 'DisplayName', 'Type', 'Start', 'Account', 'Arguments'))
    Assert ($service.Count -eq 1 -and $service[0].Name -eq 'dscd' -and
        $service[0].DisplayName -eq 'DSC Reconciliation Daemon' -and
        [int]$service[0].Type -eq 16 -and [int]$service[0].Start -eq 2 -and
        $service[0].Account -eq 'LocalSystem') 'automatic LocalSystem service'
    Assert ($service[0].Arguments -ceq '-config-dir "[CONFIGFOLDER]." -results-dir "[RESULTSFOLDER]." -dsc-path "[DSC_PATH]"') 'service arguments'
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
    Assert (@($registry | Where-Object { $_.Root -eq '2' -and $_.Key -eq 'Software\dsc-reconciler\dscd' -and $_.Name -eq 'DSC_PATH' -and $_.Value -eq '[DSC_PATH]' }).Count -eq 1) 'persisted DSC path'
    $actions = @(Rows 'SELECT `Action`, `Type`, `Target` FROM `CustomAction`' @('Name', 'Type', 'Target'))
    $searchInitializer = @($actions | Where-Object Name -eq 'SetDSC_SEARCH_DIR')
    Assert ($searchInitializer.Count -eq 1 -and ([int]$searchInitializer[0].Type -band 63) -eq 51 -and
        $searchInitializer[0].Target -ceq '[DSC_PATH]\..') 'native search-directory property initialization'
    $searchInitialization = [int]($sequence | Where-Object Action -eq 'SetDSC_SEARCH_DIR').Sequence
    $appSearch = [int]($sequence | Where-Object Action -eq 'AppSearch').Sequence
    Assert ($searchInitialization -gt 0 -and $searchInitialization -lt $appSearch) 'DSC parent directory initialized before AppSearch'
    foreach ($action in $actions) {
        Assert (([int]$action.Type -band 63) -eq 51 -or $action.Target -in @('SchedServiceConfig', 'ExecServiceConfig', 'RollbackServiceConfig')) "unexpected custom action $($action.Name)"
    }
    Write-Output "MSI metadata, contents, service, recovery, Event Log, and ACL inspection passed: $MsiPath"
}
catch {
    throw "MSI inspection failed: $($_.Exception.Message)`n$($_.ScriptStackTrace)"
}
finally {
    [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
    [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
}
