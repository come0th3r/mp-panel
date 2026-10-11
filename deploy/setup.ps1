[CmdletBinding()]
param(
  [Parameter(Mandatory=$true)][string]$Address,
  [Parameter(Mandatory=$true)][string]$User,
  [int]$Port = 22,
  [string]$IdentityFile,
  [string]$InstallDirectory = '/opt/mp-panel',
  [string]$Version = 'v0.3.3',
  [string]$Repository = 'come0th3r/mp-panel'
)
$ErrorActionPreference = 'Stop'
if ($Address -notmatch '^[A-Za-z0-9][A-Za-z0-9.:-]*$') { throw 'Invalid SSH address.' }
if ($User -notmatch '^[a-zA-Z_][a-zA-Z0-9_-]*$') { throw 'Invalid SSH user.' }
if ($Port -lt 1 -or $Port -gt 65535) { throw 'Invalid SSH port.' }
if ($InstallDirectory -notmatch '^/[A-Za-z0-9_./-]+$' -or $InstallDirectory -match '(^|/)\.\.(/|$)') { throw 'Use an absolute dedicated installation directory.' }
if ($Repository -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$') { throw 'Invalid GitHub repository.' }
if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?$') { throw 'Invalid release version.' }
if (!(Get-Command ssh -ErrorAction SilentlyContinue)) { throw 'Install the Windows OpenSSH Client first.' }

# SSH prompts for the host key and password. Changed host keys are rejected.
$sshOptions = @('-t', '-p', "$Port", '-o', 'StrictHostKeyChecking=ask')
if ($IdentityFile) { $sshOptions += @('-i', (Resolve-Path -LiteralPath $IdentityFile).Path) }
$command = @'
set -eu
task_dir=$(mktemp -d /tmp/mp-launch.XXXXXXXX)
trap 'rm -f -- "$task_dir/setup.sh"; rmdir -- "$task_dir"' 0
if ! command -v curl >/dev/null 2>&1; then
  if [ "$(id -u)" -eq 0 ]; then
    apt-get update && apt-get install -y ca-certificates curl
  else
    sudo apt-get update && sudo apt-get install -y ca-certificates curl
  fi
fi
curl --proto '=https' --tlsv1.2 -fL 'https://github.com/@REPOSITORY@/releases/download/@VERSION@/setup.sh' -o "$task_dir/setup.sh"
if [ "$(id -u)" -eq 0 ]; then
  bash "$task_dir/setup.sh" --version '@VERSION@' --repository '@REPOSITORY@' --directory '@DIRECTORY@'
else
  sudo bash "$task_dir/setup.sh" --version '@VERSION@' --repository '@REPOSITORY@' --directory '@DIRECTORY@'
fi
'@
$command = $command.Replace('@REPOSITORY@', $Repository).Replace('@VERSION@', $Version).Replace('@DIRECTORY@', $InstallDirectory).Replace("`r`n", "`n")
# Keep shell quoting intact across Windows PowerShell and PowerShell 7.
$encodedCommand = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($command))
$remoteCommand = "printf '%s' '$encodedCommand' | base64 -d | sh"
Write-Host 'Connecting over SSH. Verify the host fingerprint before accepting it.'
& ssh @sshOptions "${User}@${Address}" $remoteCommand
if ($LASTEXITCODE -ne 0) { throw 'Installation failed. Review the SSH and installer output above.' }
