<#
.SYNOPSIS
    Installs the MSP agent as a Windows service. Served by the server at /dl/install.ps1.

.DESCRIPTION
    Run from an elevated (Administrator) PowerShell:

        irm https://rmm.example.com/dl/install.ps1 -OutFile $env:TEMP\msp-install.ps1
        & $env:TEMP\msp-install.ps1 -Server https://rmm.example.com -Token et_...

    Downloads msp-agent-windows-amd64.exe and SHA256SUMS from <Server>/dl/, verifies
    the SHA-256 hash, places the binary at %ProgramData%\msp-agent\msp-agent.exe and
    runs `msp-agent.exe install`, which enrolls the device, writes
    %ProgramData%\msp-agent\agent.json and registers + starts the "MSP Agent" service.

.PARAMETER Server
    Base URL of the MSP server, e.g. https://rmm.example.com

.PARAMETER Token
    Enrollment token from the dashboard (Orgs > New enrollment token), e.g. et_...

.PARAMETER InsecureHttp
    Allow a plain http:// server URL that is not loopback (lab use only).
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [Parameter(Mandatory = $true)][string]$Token,
    [switch]$InsecureHttp
)

$ErrorActionPreference = 'Stop'

function Fail([string]$Message) {
    Write-Host "install.ps1: $Message" -ForegroundColor Red
    exit 1
}

# --- preconditions -----------------------------------------------------------
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Fail 'run from an elevated (Administrator) PowerShell: the agent registers a Windows service'
}

if ($Server -notmatch '^https?://') {
    Fail '-Server must start with https:// (or http:// for loopback / -InsecureHttp)'
}
$Server = $Server.TrimEnd('/')
if ($Token -notlike 'et_*') {
    Fail '-Token must be an enrollment token (et_...)'
}

$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -ne 'AMD64') {
    Write-Warning "only 64-bit x86 Windows is supported in this release (found $arch); the amd64 agent may run under emulation"
}

# Windows PowerShell 5.1 on older hosts defaults to TLS 1.0; the server requires TLS 1.2+.
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

# --- download ----------------------------------------------------------------
$dir = Join-Path $env:ProgramData 'msp-agent'
$exe = Join-Path $dir 'msp-agent.exe'
$download = Join-Path $dir 'msp-agent.exe.download'
$sums = Join-Path $dir 'SHA256SUMS'
$file = 'msp-agent-windows-amd64.exe'

New-Item -ItemType Directory -Force -Path $dir | Out-Null

Write-Host "downloading $Server/dl/$file"
try {
    Invoke-WebRequest -UseBasicParsing -Uri "$Server/dl/$file" -OutFile $download
    Invoke-WebRequest -UseBasicParsing -Uri "$Server/dl/SHA256SUMS" -OutFile $sums
} catch {
    Remove-Item -Force -ErrorAction SilentlyContinue $download
    Fail "download failed ($($_.Exception.Message)): agent binaries are not published: run scripts/build-agents.sh with MSP_AGENT_DIR set (or check the server URL)"
}

# --- verify ------------------------------------------------------------------
Write-Host 'verifying checksum'
$line = Get-Content $sums | Where-Object { $_ -match "\s\*?$([regex]::Escape($file))\s*$" } | Select-Object -First 1
if (-not $line) {
    Remove-Item -Force -ErrorAction SilentlyContinue $download
    Fail "$file is not listed in SHA256SUMS"
}
$expected = ($line.Trim() -split '\s+')[0].ToLowerInvariant()
$actual = (Get-FileHash -Algorithm SHA256 -Path $download).Hash.ToLowerInvariant()
if ($expected -ne $actual) {
    Remove-Item -Force -ErrorAction SilentlyContinue $download
    Fail "checksum mismatch for $file (expected $expected, got $actual); refusing to install"
}

# --- install binary ----------------------------------------------------------
# Stop a running service first so the binary can be replaced on upgrades;
# `msp-agent.exe install` restarts it afterwards.
$existing = Get-Service -Name 'msp-agent' -ErrorAction SilentlyContinue
if ($existing -and $existing.Status -ne 'Stopped') {
    Write-Host 'stopping the running msp-agent service'
    Stop-Service -Name 'msp-agent' -Force
    $existing.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
}
Move-Item -Force -Path $download -Destination $exe

# The directory will hold agent.json with the device token: SYSTEM and
# Administrators only. Best effort; a failure here does not stop the install.
try {
    & icacls $dir /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
} catch {
    Write-Warning "could not restrict permissions on ${dir}: $($_.Exception.Message)"
}

# --- enroll + register service ----------------------------------------------
$agentArgs = @('install', '--server', $Server, '--token', $Token)
if ($InsecureHttp) { $agentArgs += '--insecure-http' }

Write-Host "enrolling with $Server"
& $exe @agentArgs
if ($LASTEXITCODE -ne 0) {
    Fail "msp-agent.exe install failed with exit code $LASTEXITCODE"
}

# --- status ------------------------------------------------------------------
& $exe status
Get-Service -Name 'msp-agent' | Format-Table -AutoSize Name, Status, StartType
Write-Host 'enrolled; device appears on the Fleet page within 30 s'
Write-Host 'logs: Event Viewer > Windows Logs > Application, source msp-agent'
