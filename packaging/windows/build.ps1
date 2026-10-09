[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $OutputDirectory
)

$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'Windows packaging requires Windows.'
}

& "$PSScriptRoot\msi\version.ps1" -Version $Version | Out-Null

$output = [IO.Path]::GetFullPath($OutputDirectory)
$name = "dscd-$Version-windows-amd64"
$stage = Join-Path ([IO.Path]::GetTempPath()) ('dscd-package-' + [guid]::NewGuid().ToString('N'))
$package = Join-Path $stage $name
$bin = Join-Path $package 'bin'
$docs = Join-Path $package 'docs'
$windows = Join-Path $package 'packaging\windows'
$msiOutput = Join-Path $stage 'msi'
$binary = Join-Path $bin 'dscd.exe'
$originalCGO = $env:CGO_ENABLED
$originalOS = $env:GOOS
$originalArch = $env:GOARCH
Push-Location (Join-Path $PSScriptRoot '..\..')
try {
    New-Item -ItemType Directory -Path $bin, $docs, $windows, $output -Force | Out-Null
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    go build -trimpath -ldflags "-X main.version=$Version" -o $binary ./cmd/dscd
    if ($LASTEXITCODE -ne 0) {
        throw 'Windows binary build failed.'
    }

    $reportedVersion = & $binary --version
    if ($LASTEXITCODE -ne 0 -or $reportedVersion -cne "dscd $Version") {
        throw 'Unexpected dscd version.'
    }

    & $binary -help
    if ($LASTEXITCODE -ne 0) {
        throw 'dscd help failed.'
    }

    Copy-Item README.md, LICENSE, AGENTS.md -Destination $package
    Copy-Item docs\design.md -Destination $docs
    Copy-Item packaging\windows\install.ps1, packaging\windows\uninstall.ps1, packaging\windows\test-service.ps1 `
        -Destination $windows
    Compress-Archive -LiteralPath $package -DestinationPath (Join-Path $output "$name.zip") -Force
    & "$PSScriptRoot\msi\build.ps1" -Version $Version -BinaryPath $binary -OutputDirectory $msiOutput
    Copy-Item -LiteralPath (Join-Path $msiOutput "$name.msi") -Destination $output
    @("$name.zip", "$name.msi") | ForEach-Object {
        $hash = (Get-FileHash -LiteralPath (Join-Path $output $_) -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $_`n"
    } | Set-Content -LiteralPath (Join-Path $output 'SHA256SUMS') -Encoding utf8NoBOM -NoNewline
}
finally {
    Pop-Location
    $env:CGO_ENABLED = $originalCGO
    $env:GOOS = $originalOS
    $env:GOARCH = $originalArch
    if (Test-Path -LiteralPath $stage) {
        Remove-Item -LiteralPath $stage -Recurse -Force
    }
}
