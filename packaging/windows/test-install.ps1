[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $MsiPath,
    [string] $UpgradeMsiPath,
    [switch] $Disposable
)
$ErrorActionPreference = 'Stop'
if (-not $Disposable -or -not $IsWindows) { throw 'Use -Disposable on a disposable Windows host only.' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'An elevated disposable host is required.'
}
$data = Join-Path $env:ProgramData 'dsc'
if ((Test-Path -LiteralPath $data) -or (Get-Service dscregsyncd -ErrorAction SilentlyContinue) -or
    [Diagnostics.EventLog]::SourceExists('dscregsyncd')) {
    throw 'Refusing to touch existing DSC data, service or event source.'
}
$msi = (Resolve-Path -LiteralPath $MsiPath).Path
$upgrade = if ($UpgradeMsiPath) { (Resolve-Path -LiteralPath $UpgradeMsiPath).Path } else { $null }
function Get-PackageVersion([string] $Path) {
    $installer = New-Object -ComObject WindowsInstaller.Installer
    $database = $installer.OpenDatabase($Path, 0)
    $view = $database.OpenView('SELECT `Value` FROM `Property` WHERE `Property` = ''ProductVersion''')
    $view.Execute()
    $record = $view.Fetch()
    try { return 'v' + $record.StringData(1) }
    finally {
        $view.Close()
        foreach ($item in @($record, $view, $database, $installer)) {
            [Runtime.InteropServices.Marshal]::FinalReleaseComObject($item) | Out-Null
        }
    }
}
$expectedVersion = Get-PackageVersion $msi
$acl = [Security.AccessControl.DirectorySecurity]::new()
$acl.SetSecurityDescriptorSddlForm('O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)')
[IO.FileSystemAclExtensions]::Create([IO.DirectoryInfo]::new($data), $acl)
$inputDir = Join-Path $data 'config.d\dscregsync'
$resultsDir = Join-Path $data 'results.d\dscregsync'
New-Item -ItemType Directory -Path $inputDir, $resultsDir | Out-Null
$key = 'dscm_' + [guid]::NewGuid().ToString('N') + '.' + [guid]::NewGuid().ToString('N')
$files = @{
    (Join-Path $data 'dscregsyncd.json') = '{"registryUrl":"https://127.0.0.1:9","requestTimeoutSeconds":1}'
    (Join-Path $data 'dscregsyncd.key') = $key
    (Join-Path $inputDir 'smoke.json') = '{}'
    (Join-Path $inputDir 'smoke.parameters.json') = '{}'
    (Join-Path $inputDir '.smoke.revision') = '11111111-1111-4111-8111-111111111111'
    (Join-Path $inputDir 'machine.etag') = '"preserved"'
    (Join-Path $resultsDir 'smoke.json.result.json') = '{}'
}
foreach ($path in $files.Keys) { [IO.File]::WriteAllText($path, $files[$path]) }
$hashes = @{}
foreach ($path in $files.Keys) { $hashes[$path] = (Get-FileHash -LiteralPath $path).Hash }
function Assert-Preserved {
    foreach ($path in $files.Keys) {
        if (-not (Test-Path -LiteralPath $path) -or (Get-FileHash -LiteralPath $path).Hash -ne $hashes[$path]) {
            throw 'Installer or offline service changed provisioned data.'
        }
        $fileAcl = Get-Acl -LiteralPath $path
        foreach ($rule in $fileAcl.Access) {
            $sid = $rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
            if ($rule.AccessControlType -eq 'Allow' -and $sid -notin @('S-1-5-18', 'S-1-5-32-544')) {
                throw 'Secret/data ACL grants an unexpected identity.'
            }
        }
    }
}
function Invoke-Msi([string[]] $Arguments) {
    $process = Start-Process msiexec.exe -ArgumentList $Arguments -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) { throw "MSI failed with exit code $($process.ExitCode)." }
}
function Assert-Lifecycle {
    $binary = Join-Path $env:ProgramFiles 'dscregsyncd\dscregsyncd.exe'
    $binaryVersion = & $binary -version 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -or $binaryVersion.Trim() -cne "dscregsyncd $expectedVersion") {
        throw 'Installed executable does not match the MSI version.'
    }
    if ((Get-Service dscregsyncd).Status -ne 'Stopped') { throw 'Installer must leave the service stopped for provisioning.' }
    $registration = Get-CimInstance Win32_Service -Filter "Name='dscregsyncd'"
    if ($registration.StartMode -ne 'Auto' -or $registration.StartName -ne 'LocalSystem' -or
        $registration.PathName -notlike '*dscregsyncd.exe*') {
        throw 'Incorrect native service registration.'
    }
    $recovery = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\dscregsyncd'
    if ($recovery.FailureActionsOnNonCrashFailures -ne 1 -or -not $recovery.FailureActions) {
        throw 'Service recovery policy is missing.'
    }
    foreach ($attempt in 1..2) {
        Start-Service dscregsyncd
        $service = Get-Service dscregsyncd
        try {
            $service.WaitForStatus('Running', [timespan]::FromSeconds(30))
            Stop-Service dscregsyncd
            $service.WaitForStatus('Stopped', [timespan]::FromSeconds(40))
        }
        finally { $service.Dispose() }
    }
    Assert-Preserved
}
$installed = $false
$since = Get-Date
try {
    Invoke-Msi @('/i', "`"$msi`"", '/qn', '/norestart')
    $installed = $true
    Assert-Lifecycle
    Invoke-Msi @('/fa', "`"$msi`"", '/qn', '/norestart')
    Assert-Lifecycle
    if ($upgrade) {
        Invoke-Msi @('/i', "`"$upgrade`"", '/qn', '/norestart')
        $msi = $upgrade
        $expectedVersion = Get-PackageVersion $msi
        Assert-Lifecycle
    }
    $events = @(Get-WinEvent -FilterHashtable @{
        LogName = 'Application'; ProviderName = 'dscregsyncd'; StartTime = $since
    })
    if (-not ($events.Message -match 'daemon started') -or -not ($events.Message -match 'daemon stopped') -or
        ($events.Message -match [regex]::Escape($key))) {
        throw 'Lifecycle logging is missing or discloses credentials.'
    }
    Invoke-Msi @('/x', "`"$msi`"", '/qn', '/norestart')
    $installed = $false
    if ((Get-Service dscregsyncd -ErrorAction SilentlyContinue) -or
        [Diagnostics.EventLog]::SourceExists('dscregsyncd')) {
        throw 'Uninstall left service registration behind.'
    }
    Assert-Preserved
}
finally {
    if ($installed) { Invoke-Msi @('/x', "`"$msi`"", '/qn', '/norestart') }
    foreach ($path in $files.Keys) { Remove-Item -LiteralPath $path -Force }
    foreach ($directory in @($inputDir, $resultsDir, (Join-Path $data 'config.d'), (Join-Path $data 'results.d'), $data)) {
        Remove-Item -LiteralPath $directory
    }
}
