[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $BinaryPath,
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
$properties = @(
    "-p:ReleaseVersion=$Version"
    "-p:ProductVersion=$productVersion"
    "-p:BinaryPath=$binary"
    "-p:IntermediateOutputPath=obj/Release/$Version/"
    "-p:OutputPath=$output/"
)

# Isolate each release's clean/build tracking so rebuilding an upgrade package
# does not delete the earlier MSI from a shared output directory.
dotnet build "$PSScriptRoot/dscd.wixproj" --configuration Release --nologo --no-incremental @properties

if ($LASTEXITCODE -ne 0) {
    throw "WiX build failed: $LASTEXITCODE"
}
