# Deployment and operations

## Release scope and prerequisites

Packages target Linux amd64 and Windows x64. Install a compatible independent
`dscd` and DSC separately. Both daemons must use the same `config.d` and
`results.d` roots and compatible identities: packaged defaults are root on
Linux and LocalSystem on Windows. No service dependency forces `dscd` to start
first: synchronization, offline reconciliation and watcher recovery are
independent. Only one `dscregsyncd` may own a managed subtree.

The installer owns the executable, service registration, license and (Linux)
an example under `/usr/share/doc/dscregsyncd`. It never owns or overwrites a live
JSON config, machine key, managed inputs, revisions, ETag or results.
Uninstall, including Debian purge, **retains all provisioned data** and does not
undo DSC changes or revoke the Registry key. Revoke keys separately when retiring
a machine. Remove retained data only after stopping both daemons and deciding
whether offline reconciliation should continue.

The package scripts do not rewrite existing data-directory permissions. New
Linux service roots use 0700; Windows data roots must be provisioned with
restricted ACLs before credentials are copied. The daemon still creates new
managed entries privately, but cannot make an insecure administrator-owned
ancestor safe. Roots/ancestors must not be writable by untrusted users or
redirected through untrusted symlinks/reparse points.

## Linux

Install the `.deb` with `apt install ./dscregsyncd_VERSION_amd64.deb`, or the
`.rpm` with `dnf install ./dscregsyncd-VERSION-1.x86_64.rpm`.
Packages enable `dscregsyncd.service`. On an unprovisioned host systemd skips
startup because `/etc/dsc/dscregsyncd.json` is absent.

For initial provisioning, ensure neither destination already exists. Copy the
example to `/etc/dsc/dscregsyncd.json` as root, mode 0600, and set `registryUrl`
to the production HTTPS origin. Place the separately issued machine key at
`/etc/dsc/dscregsyncd.key`, root-owned mode 0600, using a secure source file or
secret-management tool, not shell history or command-line key values.
Do not put the key in JSON.

```sh
sudo systemctl start dscregsyncd
sudo systemctl status dscregsyncd --no-pager
sudo journalctl -u dscregsyncd --no-pager
/usr/bin/dscregsyncd -version
```

The unit uses umask 0077, `NoNewPrivileges`, a read-only system filesystem,
private temporary storage and protected home directories. Its only writable
system paths are `/etc/dsc/config.d` and `/var/lib/dsc/results.d`.
For custom roots, provision them privately and add a systemd drop-in with the
appropriate `ReadWritePaths`; `ProtectHome` must also be considered for
home-backed paths. Changing the service JSON path requires an `ExecStart`
override and an updated `ConditionPathExists`. Do not remove hardening globally
just to hide permission failures.

Upgrades try-restart an active daemon without starting an administrator-stopped
one. Removal stops/disables only this service. The 40-second systemd stop timeout
exceeds the daemon's 30-second cleanup deadline.

## Windows

Run the MSI elevated, for example:

```powershell
$process = Start-Process msiexec.exe -ArgumentList '/i "C:\packages\dscregsyncd-1.0.0-windows-amd64.msi" /qn /norestart' -Wait -PassThru
if ($process.ExitCode -notin @(0, 3010)) { throw 'MSI installation failed' }
```

It installs `%ProgramFiles%\dscregsyncd\dscregsyncd.exe`, registers the automatic
LocalSystem service and Application Event Log source `dscregsyncd`, and configures
five-second failure recovery. The executable directory is writable only by
SYSTEM/Administrators. **Install, repair and upgrade leave the service stopped**:
provision/verify data, then explicitly start it. Automatic startup applies on
subsequent boots. Recovery on handled nonzero SCM exits is configured, but Windows
may require the next system boot to activate that failure-policy flag.

Provision `%ProgramData%\dsc` before copying any credentials. For a **new**
directory, PowerShell 7 can create it with the ACL already restricted:

```powershell
$data = Join-Path $env:ProgramData 'dsc'
if (Test-Path -LiteralPath $data) { throw 'Inspect the existing shared DSC directory before provisioning' }
$acl = [Security.AccessControl.DirectorySecurity]::new()
$acl.SetSecurityDescriptorSddlForm('O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)')
[IO.FileSystemAclExtensions]::Create([IO.DirectoryInfo]::new($data), $acl)
```

The owner is Administrators, which an elevated administrator can assign without
enabling restore privileges to impersonate SYSTEM ownership. Access remains
restricted to SYSTEM and Administrators.

If `dscd` already created the directory, inspect its owner, ACL and ancestors;
use that protected directory instead of rerunning the creation example. Restrict
access to SYSTEM/Administrators (or deliberately configured compatible service
identities), not Users/Everyone. Verify inherited and explicit permissions on
existing children too. The MSI deliberately does not change this shared tree.

Create `dscregsyncd.json` there with at least
`{"registryUrl":"https://registry.example.internal"}`. Other Windows paths default
under this directory. Copy the separately provisioned machine key from a secure
file to `dscregsyncd.key`, inheriting the restricted parent ACL. Do not overwrite
existing credentials during provisioning; do not grant ordinary users access.

```powershell
Start-Service dscregsyncd
Get-Service dscregsyncd
Get-WinEvent -FilterHashtable @{ LogName='Application'; ProviderName='dscregsyncd' } -MaxEvents 20
& "$env:ProgramFiles\dscregsyncd\dscregsyncd.exe" -version
```

SCM stop/shutdown cancels polling, upload and watcher waits. Event ID 1 contains
structured JSON including its own `level`; the Event Log entry itself uses the
information category. An incomplete shutdown returns a nonzero service exit
code for recovery instead of falsely reporting clean cleanup.
MSI uninstall stops/removes this service and its event source, not `dscd` or
`%ProgramData%\dsc`. Repair/upgrade require an explicit `Start-Service` afterward.
Packages are currently unsigned; follow organizational artifact verification and
signing policy rather than treating checksums as publisher authentication.

## Key rotation, TLS and diagnosis

Issue the replacement machine key through Registry administration. While the old
key remains valid, prepare the replacement in a private file in the protected
key directory, stop this service, atomically replace the key file and restart.
Verify successful polling/upload through safe status logs and Registry history,
then revoke the old key. The key is read only at startup; changing it on disk
without restarting has no effect. Never place either key in a command argument,
routine logs, issue report or source control.

HTTPS uses OS trust roots and normal hostname/certificate validation. Provision
the internal CA into the service's OS trust store if necessary; there is no
certificate-verification bypass. HTTP is only an explicit loopback development
option. Redirects are refused.

Use status codes, retry delays and lifecycle events to diagnose failures.
401/403 indicate credential/role problems; 429/5xx back off without deleting
cached inputs. A 409 report identity conflict or permanent invalid report needs
investigation, not a fabricated revision/new upload ID. Filesystem failures
usually indicate unsafe entries, permissions or an incompatible reconciler.
Never attach full keys, templates, parameters, result files or resource stderr
to routine diagnostics: they can contain secrets.

Watcher recovery is notification-driven with a one-time scan after startup or
reattachment. A missing result directory is normal before first execution.
After confirmed out-of-band input loss, stop `dscregsyncd` and invalidate only
its `machine.etag` before restarting to request a complete collection; a 304
cannot repair arbitrary local deletion. Do not delete revisions or alter other
`config.d` subtrees as a troubleshooting shortcut.

## Builds, CI and acceptance evidence

From the repository root, with Go 1.27:

```sh
# nFPM v2.47.0 must be on PATH.
VERSION=v1.0.0 bash packaging/linux/build.sh dist
```

On Windows with PowerShell 7 and the .NET SDK, the project restores WiX 7.0.0:

```powershell
.\packaging\windows\build.ps1 -Version v1.0.0 -OutputDirectory dist
```

Only stable `vMAJOR.MINOR.PATCH` versions are accepted. MSI maps directly to
`MAJOR.MINOR.PATCH`, requiring major/minor <=255 and patch <=65535.
`-version` embeds the same tag (`dev` for ordinary developer builds).
DEB/RPM and MSI builds are unsigned. Tagged deployment workflows wait for
formatting, tests, vet, race/build checks, native peer roundtrips and package
lifecycle jobs, then publish packages and SHA-256 checksums.

`packaging/linux/test-install.sh` requires root and
`DSCREGSYNC_DISPOSABLE=1`; `packaging/windows/test-install.ps1` requires
elevation and `-Disposable`. **Use only disposable hosts.** Both refuse existing
installations, seed credentials/managed data, check offline service restarts,
repair/reinstall, optional upgrade and retention after uninstall. The Windows
test also checks restricted ACLs, recovery registration and redacted lifecycle
events. Linux CI checks DEB under real systemd and RPM under Fedora without
systemd; the RPM test is not evidence of native systemd service operation.

The opt-in Go test uses real binaries, a disposable SQLite Registry and real DSC:

```powershell
$env:DSCREGSYNC_INTEGRATION = '1'
$env:DSCREGD_BINARY = 'C:\test-tools\dscregd.exe'
$env:DSCD_BINARY = 'C:\test-tools\dscd.exe'
$env:DSC_BINARY = 'C:\test-tools\dsc\dsc.exe'
go test ./cmd/dscregsyncd -run '^TestRealServiceRoundtrip$' -count=1 -timeout 5m -v
```

On Linux, export the same variables pointing to Linux executables. Production
code never executes DSC: the test launches `dscd`, which executes a harmless
test-only DSC echo resource. A barrier holds real `dscd` protection during an
update; a transparent HTTP proxy delays an original upload until its desired
configuration is deleted. The test checks initial 200/304, changed parameters,
protection ordering, result generation, removed-revision acceptance, exact bytes,
duplicate 200 history behavior, committed-validator/startup-scan recovery and
distinct executions after reconciler restart. All processes are scoped to the
test (Windows Job Objects; Linux process groups); secrets/peer output are not
printed. This is foreground cross-service integration, **not an installed-service
or whole-machine reboot test**.

Pinned peer versions are Registry
`5a4ddb543afc103de3ad933922ed5f603f3ff656`, reconciler
`d6010bf737dbbbfd56d69d98d1fc20132022c8dd`, and DSC 3.3.0 with
verified upstream archive SHA-256 checksums. Registry's relative-path fix is
exercised by the real report roundtrip, not worked around by rewriting envelopes.
Its Windows device-stem naming discrepancy remains documented in the protocol.

Local evidence for this stage: Windows unit tests/vet, real Windows roundtrip
with both peers and DSC, MSI build/validation, Linux and Windows binary builds,
and DEB/RPM payload creation. Local race tests are blocked by the missing C
compiler; Linux runtime is unavailable. Package lifecycle scripts and the new
CI jobs have not been executed on this shared workstation.

Before declaring v1 ready, require green native Linux/Windows CI, exercise the
packages against the intended deployed Registry/reconciler releases and test a
real reboot on disposable Linux and Windows machines. After reboot verify both
services start automatically, cached inputs/key/ETag survive, the first poll
uses the committed validator, and new executions reach history without duplicate
unchanged snapshots. Record OS/build versions and safe status evidence; do not
claim a cross-build or a process restart proves reboot compatibility.
