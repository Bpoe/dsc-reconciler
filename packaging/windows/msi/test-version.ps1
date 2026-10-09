[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$valid = @{
    'v0.0.0' = '0.0.99'
    'v0.0.1' = '0.0.199'
    'v255.255.654' = '255.255.65499'
}
foreach ($entry in $valid.GetEnumerator()) {
    $actual = & "$PSScriptRoot/version.ps1" -Version $entry.Key
    if ($actual -cne $entry.Value) {
        throw "Incorrect mapping for $($entry.Key): $actual"
    }
}

foreach ($invalid in @('0.0.1', 'V0.0.1', 'v00.1.1', 'v1.01.1', 'v1.1.01', 'v1.1.1-rc.1', 'v1.1.1-rc.01',
        'v1.1.1-rc.0', 'v1.1.1-rc.99', 'v1.1.1-beta.1', 'v1.1.1+build', 'v256.0.0',
        'v0.256.0', 'v0.0.655', 'v999999999999999999999999.0.0', "v1.1.1`n", '')) {
    $rejected = $false
    try {
        & "$PSScriptRoot/version.ps1" -Version $invalid | Out-Null
    }
    catch {
        $rejected = $true
    }

    if (-not $rejected) {
        throw "Accepted invalid version: $invalid"
    }
}

$previous = [version]'0.0.0'
foreach ($tag in @('v0.0.0', 'v0.0.1', 'v0.1.0', 'v1.0.0', 'v255.255.654')) {
    $current = [version](& "$PSScriptRoot/version.ps1" $tag)
    if ($current -le $previous) {
        throw "Versions do not increase at $tag"
    }

    $previous = $current
}

Write-Output 'MSI version mapping, ordering, and invalid boundaries passed.'
