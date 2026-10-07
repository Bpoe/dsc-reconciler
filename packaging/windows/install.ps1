#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $BinaryPath,
    [Parameter(Mandatory)][string] $DSCPath,
    [string] $ConfigDir = "$env:ProgramData\dsc\config.d",
    [string] $ResultsDir = "$env:ProgramData\dsc\results.d"
)

$ErrorActionPreference = 'Stop'
if (Get-Service -Name dscd -ErrorAction SilentlyContinue) {
    throw 'The dscd service already exists. Stop and remove it before reinstalling.'
}
$BinaryPath = (Resolve-Path -LiteralPath $BinaryPath).Path
$DSCPath = (Resolve-Path -LiteralPath $DSCPath).Path
$ConfigDir = [IO.Path]::GetFullPath($ConfigDir)
$ResultsDir = [IO.Path]::GetFullPath($ResultsDir)
foreach ($path in @($BinaryPath, $DSCPath, $ConfigDir, $ResultsDir)) {
    if ($path.Contains('"')) {
        throw 'Paths must not contain double quotes.'
    }
}

if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $DSCPath -PathType Leaf)) {
    throw 'BinaryPath and DSCPath must name executable files.'
}

foreach ($directory in @($ConfigDir, $ResultsDir)) {
    if (-not (Test-Path -LiteralPath $directory)) {
        New-Item -ItemType Directory -Path $directory | Out-Null
        & icacls.exe $directory /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Cannot protect $directory"
        }
    }

    if (-not (Test-Path -LiteralPath $directory -PathType Container)) {
        throw "Not a directory: $directory"
    }
}

if (-not [Diagnostics.EventLog]::SourceExists('dscd')) {
    [Diagnostics.EventLog]::CreateEventSource('dscd', 'Application')
}

function Quote-Path([string] $Value) {
    # Double trailing backslashes so they do not escape the closing argument quote.
    return '"' + ($Value -replace '(\\+)$', '$1$1') + '"'
}

$command = '{0} -config-dir {1} -results-dir {2} -dsc-path {3} -interval 5m -execution-timeout 15m' -f `
    (Quote-Path $BinaryPath), (Quote-Path $ConfigDir), (Quote-Path $ResultsDir), (Quote-Path $DSCPath)
New-Service -Name dscd -DisplayName 'DSC reconciliation daemon' -BinaryPathName $command -StartupType Automatic `
    -Description 'Periodically applies local DSC documents and publishes their latest results.' | Out-Null
& sc.exe failure dscd reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Could not configure service recovery. Installation is incomplete.'
}

& sc.exe failureflag dscd 1 | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Could not enable recovery for nonzero service exits. Installation is incomplete.'
}

Write-Output 'Installed dscd as LocalSystem. Review directory ACLs and resource permissions, then Start-Service dscd.'
