#Requires -RunAsAdministrator
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$service = Get-Service -Name dscd -ErrorAction Stop
if ($service.Status -ne 'Stopped') {
    Stop-Service -Name dscd
    $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(40))
}
$service.Dispose()
& sc.exe delete dscd | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not delete dscd service.' }
if ([Diagnostics.EventLog]::SourceExists('dscd')) {
    [Diagnostics.EventLog]::DeleteEventSource('dscd')
}
Write-Output 'Removed dscd service registration. Binaries, configurations, results and historical events were retained.'
