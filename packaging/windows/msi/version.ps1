[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version
)

$ErrorActionPreference = 'Stop'

# Keep patch*100 + candidate within MSI's 16-bit third field. Stable sorts last.
if ($Version -cnotmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.([1-9][0-9]*))?\z') {
    throw 'Expected vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N.'
}

$major = [System.Numerics.BigInteger]::Parse($Matches[1])
$minor = [System.Numerics.BigInteger]::Parse($Matches[2])
$patch = [System.Numerics.BigInteger]::Parse($Matches[3])
$candidate = if ($Matches[4]) {
    [System.Numerics.BigInteger]::Parse($Matches[4])
}
else {
    99
}

if ($major -gt 255 -or $minor -gt 255 -or $patch -gt 654 -or
    ($Matches[4] -and $candidate -gt 98)) {
    throw 'MSI releases require major/minor <= 255, patch <= 654, and rc.N in 1..98.'
}

"$major.$minor.$($patch * 100 + $candidate)"
