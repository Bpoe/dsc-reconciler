[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $BinaryPath,
    [Parameter(Mandatory)][string] $DSCDirectory,
    [Parameter(Mandatory)][string] $OutputDirectory
)

$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'WiX MSI binding and validation require Windows.'
}

$productVersion = & "$PSScriptRoot/version.ps1" -Version $Version
$binary = (Resolve-Path -LiteralPath $BinaryPath).Path
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw 'BinaryPath must be dscd.exe.'
}

$output = [IO.Path]::GetFullPath($OutputDirectory)
$dscDirectory = (Resolve-Path -LiteralPath $DSCDirectory).Path
if (-not (Test-Path -LiteralPath $dscDirectory -PathType Container) -or
    -not (Test-Path -LiteralPath (Join-Path $dscDirectory 'dsc.exe') -PathType Leaf) -or
    -not (Test-Path -LiteralPath (Join-Path $dscDirectory 'LICENSE') -PathType Leaf) -or
    -not (Test-Path -LiteralPath (Join-Path $dscDirectory 'NOTICE.txt') -PathType Leaf)) {
    throw 'DSCDirectory must contain the complete DSC distribution, upstream LICENSE and NOTICE.txt.'
}
$stage = Join-Path ([IO.Path]::GetTempPath()) ('dscd-msi-build-' + [guid]::NewGuid().ToString('N'))
$intermediate = Join-Path $stage 'obj'
$buildOutput = Join-Path $stage 'output'

try {
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    $properties = @(
        "-p:ReleaseVersion=$Version"
        "-p:ProductVersion=$productVersion"
        "-p:BinaryPath=$binary"
        "-p:DSCDirectory=$dscDirectory"
        "-p:BaseIntermediateOutputPath=$intermediate/"
        "-p:IntermediateOutputPath=$intermediate/"
        "-p:MSBuildProjectExtensionsPath=$intermediate/"
        "-p:OutputPath=$buildOutput/"
    )
    dotnet build "$PSScriptRoot/dscd.wixproj" --configuration Release --nologo --no-incremental @properties

    if ($LASTEXITCODE -ne 0) {
        throw "WiX build failed: $LASTEXITCODE"
    }

    New-Item -ItemType Directory -Path $output -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $buildOutput "dscd-$Version-windows-amd64.msi") -Destination $output
}
finally {
    if (Test-Path -LiteralPath $stage) {
        Remove-Item -LiteralPath $stage -Recurse -Force
    }
}
