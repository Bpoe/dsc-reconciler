[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $BinaryPath,
    [Parameter(Mandatory)][string] $OutputDirectory,
    [string] $WixVersion = '7.0.0',
    [switch] $AcceptWixEula
)
$ErrorActionPreference = 'Stop'
if ($WixVersion -cne '7.0.0') { throw 'This installer requires the pinned WiX 7.0.0 SDK and Util extension.' }
$project = [xml](Get-Content -Raw -LiteralPath "$PSScriptRoot/dscd.wixproj")
if ($project.Project.Sdk -cne "WixToolset.Sdk/$WixVersion" -or
    $project.Project.ItemGroup.PackageReference.Version -cne $WixVersion) {
    throw 'The workflow WiX version and project SDK/Util pins must match.'
}
if (-not $IsWindows) { throw 'WiX MSI binding and validation require Windows.' }
if (-not $AcceptWixEula) {
    throw 'Review https://github.com/wixtoolset/wix/blob/v7.0.0/OSMFEULA.txt, then pass -AcceptWixEula to accept the WiX v7 OSMF EULA.'
}
$productVersion = & "$PSScriptRoot/version.ps1" -Version $Version
$binary = (Resolve-Path -LiteralPath $BinaryPath).Path
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'BinaryPath must be dscd.exe.' }
$stream = [IO.File]::OpenRead($binary)
$reader = [IO.BinaryReader]::new($stream)
try {
    if ($reader.ReadUInt16() -ne 0x5A4D) { throw 'BinaryPath must be a Windows PE executable.' }
    $stream.Position = 0x3C
    $stream.Position = $reader.ReadUInt32()
    if ($reader.ReadUInt32() -ne 0x4550 -or $reader.ReadUInt16() -ne 0x8664) {
        throw 'BinaryPath must target Windows x64 (AMD64).'
    }
}
finally { $reader.Dispose() }
$output = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $output -Force | Out-Null
$nativeDirectory = Join-Path $PSScriptRoot "obj/Release/$Version/native"
New-Item -ItemType Directory -Path $nativeDirectory -Force | Out-Null
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$visualStudio = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $visualStudio) { throw 'Building the native MSI path helper requires Visual Studio x64 C++ build tools.' }
$properties = @(
    "-p:ReleaseVersion=$Version"
    "-p:ProductVersion=$productVersion"
    "-p:BinaryPath=$binary"
    "-p:SearchHelperPath=$(Join-Path $nativeDirectory 'search-directory.dll')"
    "-p:IntermediateOutputPath=obj/Release/$Version/"
    "-p:OutputPath=$output/"
    '-p:AcceptEula=wix7'
)
dotnet restore "$PSScriptRoot/dscd.wixproj" --nologo --verbosity normal @properties
if ($LASTEXITCODE -ne 0) { throw "WiX dependency restore failed: $LASTEXITCODE" }
Import-Module (Join-Path $visualStudio 'Common7/Tools/Microsoft.VisualStudio.DevShell.dll')
Enter-VsDevShell -VsInstallPath $visualStudio -SkipAutomaticLocation -DevCmdArguments '-arch=x64 -host_arch=x64'
$searchHelper = Join-Path $nativeDirectory 'search-directory.dll'
& cl.exe /nologo /LD /W4 /WX /O2 /MT "$PSScriptRoot/search-directory.c" `
    "/Fo$nativeDirectory/" "/Fe$searchHelper" /link msi.lib "/IMPLIB:$nativeDirectory/search-directory.lib"
if ($LASTEXITCODE -ne 0) { throw "Native MSI path helper build failed: $LASTEXITCODE" }
# Isolate each release's clean/build tracking so rebuilding an upgrade package
# does not delete the earlier MSI from a shared output directory.
dotnet build "$PSScriptRoot/dscd.wixproj" --configuration Release --nologo --no-incremental --no-restore @properties
if ($LASTEXITCODE -ne 0) { throw "WiX build failed: $LASTEXITCODE" }
$msi = Join-Path $output "dscd-$Version-windows-amd64.msi"
& "$PSScriptRoot/inspect.ps1" -MsiPath $msi -Version $Version
