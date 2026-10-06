#Requires -RunAsAdministrator
# Opt-in integration test for disposable CI machines, never invoked by go test.
[CmdletBinding()]
param([Parameter(Mandatory)][string] $BinaryPath)

$ErrorActionPreference = 'Stop'
if ((Get-Service -Name dscd -ErrorAction SilentlyContinue) -or [Diagnostics.EventLog]::SourceExists('dscd')) {
    throw 'Refusing to test on a machine with an existing dscd service or event source.'
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('dscd service test ' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
$binary = Join-Path $root 'daemon with spaces.exe'
Copy-Item -LiteralPath $BinaryPath -Destination $binary
$installed = $false
try {
    # Empty inputs: the daemon executable is only an executable-discovery placeholder.
    & "$PSScriptRoot\install.ps1" -BinaryPath $binary -DSCPath $binary `
        -ConfigDir "$root\input" -ResultsDir "$root\results"
    $installed = $true
    foreach ($attempt in 1..2) {
        Start-Service dscd
        $service = Get-Service dscd
        $service.WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
        Stop-Service dscd
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(40))
        $service.Dispose()
    }
    $events = @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'dscd' } -MaxEvents 10)
    if (-not ($events.Message -match 'dscd started') -or -not ($events.Message -match 'dscd stopped')) {
        throw 'Expected lifecycle events were not published.'
    }
    Write-Output 'SCM installation, start, stop, restart and Event Log integration passed.'
}
finally {
    if ($installed -or (Get-Service dscd -ErrorAction SilentlyContinue)) {
        & "$PSScriptRoot\uninstall.ps1"
    }
    elseif ([Diagnostics.EventLog]::SourceExists('dscd')) {
        [Diagnostics.EventLog]::DeleteEventSource('dscd')
    }
    # The exact GUID-named directory was created above and contains only this test's files.
    Remove-Item -LiteralPath $root -Recurse -Force
}
