[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $BinaryPath,
    [Parameter(Mandatory)][string] $OutputDirectory,
    [string] $WixVersion = '7.0.0',
    [switch] $AcceptWixEula
)

$ErrorActionPreference = 'Stop'
if ($WixVersion -cne '7.0.0') {
    throw 'This installer requires the pinned WiX 7.0.0 SDK and Util extension.'
}

$project = [xml](Get-Content -Raw -LiteralPath "$PSScriptRoot/dscd.wixproj")
if ($project.Project.Sdk -cne "WixToolset.Sdk/$WixVersion" -or
    $project.Project.ItemGroup.PackageReference.Version -cne $WixVersion) {
    throw 'The workflow WiX version and project SDK/Util pins must match.'
}

if (-not $IsWindows) {
    throw 'WiX MSI binding and validation require Windows.'
}

if (-not $AcceptWixEula) {
    throw 'Review https://github.com/wixtoolset/wix/blob/v7.0.0/OSMFEULA.txt, then pass -AcceptWixEula to accept the WiX v7 OSMF EULA.'
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
    '-p:AcceptEula=wix7'
)

# Isolate each release's clean/build tracking so rebuilding an upgrade package
# does not delete the earlier MSI from a shared output directory.
dotnet build "$PSScriptRoot/dscd.wixproj" --configuration Release --nologo --no-incremental @properties

if ($LASTEXITCODE -ne 0) {
    throw "WiX build failed: $LASTEXITCODE"
}
