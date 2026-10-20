[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [string] $OutputDirectory = 'dist'
)
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'MSI builds require Windows and PowerShell 7.' }
if ($Version -cnotmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z') {
    throw 'Expected vMAJOR.MINOR.PATCH.'
}
$parts = $Version.Substring(1).Split('.')
if ([bigint]$parts[0] -gt 255 -or [bigint]$parts[1] -gt 255 -or [bigint]$parts[2] -gt 65535) {
    throw 'MSI version requires major/minor <= 255 and patch <= 65535.'
}
$productVersion = $Version.Substring(1)
$output = [IO.Path]::GetFullPath($OutputDirectory)
$stage = Join-Path ([IO.Path]::GetTempPath()) ('dscregsyncd-build-' + [guid]::NewGuid().ToString('N'))
$previousCGO = $env:CGO_ENABLED
try {
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    $binary = Join-Path $stage 'dscregsyncd.exe'
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags "-X main.version=$Version" -o $binary .\cmd\dscregsyncd
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
    dotnet build "$PSScriptRoot\msi\dscregsyncd.wixproj" --configuration Release --nologo `
        "-p:ProductVersion=$productVersion" "-p:BinaryPath=$binary" `
        "-p:BaseIntermediateOutputPath=$stage\obj\" "-p:IntermediateOutputPath=$stage\obj\" `
        "-p:MSBuildProjectExtensionsPath=$stage\obj\" "-p:OutputPath=$stage\output\"
    if ($LASTEXITCODE -ne 0) { throw 'WiX build failed.' }
    New-Item -ItemType Directory -Path $output -Force | Out-Null
    Copy-Item -LiteralPath "$stage\output\dscregsyncd-$productVersion-windows-amd64.msi" -Destination $output
}
finally {
    $env:CGO_ENABLED = $previousCGO
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}
