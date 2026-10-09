<#
.SYNOPSIS
  Runs cc-connect as a background service on Windows Server (Task Scheduler).

.DESCRIPTION
  Registers a scheduled task that starts cc-connect at boot under a fixed,
  non-administrator service account, without anyone logging on. A small
  runner script restarts cc-connect after a crash, and cc-connect's own log
  rotation (CC_LOG_FILE / CC_LOG_MAX_SIZE / CC_LOG_MAX_BACKUPS) is enabled.

  Unlike `cc-connect daemon install` (which starts at user logon), this is
  meant for unattended servers such as the WeChat Customer Service (wecom_kf)
  deployment. See docs/wecom-kf-windows.md.

  Run from an elevated PowerShell. The service account needs the
  "Log on as a batch job" right.

.EXAMPLE
  .\install-cc-connect-service.ps1 -ExePath D:\cc-connect\cc-connect.exe `
      -ConfigPath D:\cc-connect\config.toml -ExtraPath 'C:\Users\ccservice\AppData\Roaming\npm'

.EXAMPLE
  .\install-cc-connect-service.ps1 -Uninstall
#>
[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(Mandatory = $true, ParameterSetName = 'Install')]
    [string]$ExePath,

    [Parameter(Mandatory = $true, ParameterSetName = 'Install')]
    [string]$ConfigPath,

    [Parameter(ParameterSetName = 'Install')]
    [System.Management.Automation.PSCredential]$Credential,

    [Parameter(ParameterSetName = 'Install')]
    [string]$LogFile,

    [Parameter(ParameterSetName = 'Install')]
    [ValidateRange(1, 1024)]
    [int]$LogMaxSizeMB = 10,

    [Parameter(ParameterSetName = 'Install')]
    [ValidateRange(0, 100)]
    [int]$LogMaxBackups = 10,

    # Directories prepended to PATH (e.g. where codex.cmd / node.exe live for the service account).
    [Parameter(ParameterSetName = 'Install')]
    [string[]]$ExtraPath = @(),

    [string]$TaskName = 'cc-connect',

    [Parameter(Mandatory = $true, ParameterSetName = 'Uninstall')]
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

function Assert-Administrator {
    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this script from an elevated (Administrator) PowerShell.'
    }
}

function ConvertTo-PSLiteral([string]$Value) {
    return "'" + ($Value -replace "'", "''") + "'"
}

function Remove-ServiceTask {
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '')]
    param([string]$Name)
    $task = Get-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue
    if ($null -eq $task) { return $null }
    if ($task.State -eq 'Running') { Stop-ScheduledTask -TaskName $Name }
    $runner = $null
    foreach ($action in $task.Actions) {
        if ($action.Arguments -match '-File "([^"]+)"') { $runner = $Matches[1] }
    }
    Unregister-ScheduledTask -TaskName $Name -Confirm:$false
    return $runner
}

Assert-Administrator

if ($Uninstall) {
    $runner = Remove-ServiceTask $TaskName
    if ($runner -and (Test-Path -LiteralPath $runner)) { Remove-Item -LiteralPath $runner }
    Write-Output "Scheduled task '$TaskName' removed."
    return
}

$ExePath = (Resolve-Path -LiteralPath $ExePath).Path
$ConfigPath = (Resolve-Path -LiteralPath $ConfigPath).Path
$configDir = Split-Path -Parent $ConfigPath
if (-not $LogFile) { $LogFile = Join-Path $configDir 'logs\cc-connect.log' }
$logDir = Split-Path -Parent $LogFile
if (-not $Credential) {
    $Credential = Get-Credential -Message 'Service account that runs cc-connect and Codex (not an administrator)'
}
$user = $Credential.UserName

New-Item -ItemType Directory -Force -Path $logDir | Out-Null
# The service account writes logs; it only needs to read the binary and config.
& icacls.exe $logDir /grant "${user}:(OI)(CI)M" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "icacls failed to grant $user access to $logDir" }

$runnerPath = Join-Path $configDir 'cc-connect-service.ps1'
$lines = @(
    "`$ErrorActionPreference = 'Continue'",
    "`$env:CC_LOG_FILE = $(ConvertTo-PSLiteral $LogFile)",
    "`$env:CC_LOG_MAX_SIZE = '${LogMaxSizeMB}MB'",
    "`$env:CC_LOG_MAX_BACKUPS = '$LogMaxBackups'"
)
if ($ExtraPath.Count -gt 0) {
    $lines += "`$env:PATH = $(ConvertTo-PSLiteral (($ExtraPath -join ';') + ';')) + `$env:PATH"
}
$lines += @(
    "Set-Location -LiteralPath $(ConvertTo-PSLiteral $configDir)",
    'while ($true) {',
    "  & $(ConvertTo-PSLiteral $ExePath) -config $(ConvertTo-PSLiteral $ConfigPath)",
    '  if ($LASTEXITCODE -eq 0) { exit 0 }',
    '  Start-Sleep -Seconds 10',
    '}'
)
Set-Content -LiteralPath $runnerPath -Value ($lines -join "`r`n") -Encoding UTF8

$null = Remove-ServiceTask $TaskName
$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$runnerPath`"" `
    -WorkingDirectory $configDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings `
    -User $user -Password $Credential.GetNetworkCredential().Password -RunLevel Limited `
    -Description 'cc-connect (started at boot, restarts on failure)' | Out-Null
Start-ScheduledTask -TaskName $TaskName

Write-Output "Scheduled task '$TaskName' registered for $user and started."
Write-Output "Runner : $runnerPath"
Write-Output "Logs   : $LogFile"
Write-Output "Status : Get-ScheduledTask -TaskName $TaskName | Select-Object State"
