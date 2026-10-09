[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version
)

$ErrorActionPreference = 'Stop'

# Preserve the stable mapping used by previously built MSI packages.
if ($Version -cnotmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z') {
    throw 'Expected vMAJOR.MINOR.PATCH.'
}

$major = [System.Numerics.BigInteger]::Parse($Matches[1])
$minor = [System.Numerics.BigInteger]::Parse($Matches[2])
$patch = [System.Numerics.BigInteger]::Parse($Matches[3])
if ($major -gt 255 -or $minor -gt 255 -or $patch -gt 654) {
    throw 'MSI releases require major/minor <= 255 and patch <= 654.'
}

"$major.$minor.$($patch * 100 + 99)"
