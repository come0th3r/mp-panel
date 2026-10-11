[CmdletBinding()]
param(
  [Parameter(Mandatory=$true)][string]$Address,
  [Parameter(Mandatory=$true)][string]$User,
  [int]$Port = 22,
  [string]$IdentityFile,
  [string]$InstallDirectory = '/opt/mp-panel',
  [string]$EnvironmentFile,
  [string]$KnownHostsFile
)
$ErrorActionPreference = 'Stop'
if ($Address -notmatch '^[A-Za-z0-9.:-]+$' -or $Address.StartsWith('-')) { throw 'Invalid SSH address' }
if ($User -notmatch '^[a-zA-Z_][a-zA-Z0-9_-]*$') { throw 'Invalid SSH user' }
if ($Port -lt 1 -or $Port -gt 65535) { throw 'Invalid SSH port' }
if ($InstallDirectory -notmatch '^/[a-zA-Z0-9_./-]+$' -or $InstallDirectory -match '(^|/)\.\.(/|$)') { throw 'Use an absolute dedicated install directory without spaces or ..' }
$projectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (!$EnvironmentFile) { $EnvironmentFile = Join-Path $projectRoot '.env' }
$resolvedEnv = (Resolve-Path -LiteralPath $EnvironmentFile).Path
$envContent = Get-Content -LiteralPath $resolvedEnv -Raw
foreach ($key in @('POSTGRES_PASSWORD','MP_PUBLIC_URL','MP_AGENT_PUBLIC_URL','MP_DOWNLOAD_USER_AGENT')) {
  if ($envContent -notmatch "(?m)^$key=.+$") { throw "Missing $key in .env" }
}
if ($envContent -match 'example\.com|your-real-domain\.example') { throw 'Replace example public URLs and User-Agent contact in .env before deployment' }
$sshOptions = @('-p', "$Port", '-o', 'StrictHostKeyChecking=yes', '-o', 'BatchMode=yes')
$scpOptions = @('-P', "$Port", '-o', 'StrictHostKeyChecking=yes', '-o', 'BatchMode=yes')
if ($IdentityFile) { $keyPath = (Resolve-Path -LiteralPath $IdentityFile).Path; $sshOptions += @('-i', $keyPath); $scpOptions += @('-i', $keyPath) }
if ($KnownHostsFile) { $knownPath = (Resolve-Path -LiteralPath $KnownHostsFile).Path; $sshOptions += @('-o', "UserKnownHostsFile=$knownPath"); $scpOptions += @('-o', "UserKnownHostsFile=$knownPath") }
$target = "${User}@${Address}"
if ($Address.Contains(':')) { $scpTarget = "${User}@[$Address]" } else { $scpTarget = $target }
$taskId = [guid]::NewGuid().ToString('N')
$bundle = Join-Path ([IO.Path]::GetTempPath()) "mp-panel-$taskId.tar.gz"
$remoteBundle = "/tmp/mp-panel-$taskId.tar.gz"
$remoteEnv = "/tmp/mp-panel-$taskId.env"
try {
  & ssh @sshOptions $target 'true'
  if ($LASTEXITCODE -ne 0) { throw 'SSH key authentication or known_hosts validation failed. Verify the fingerprint and establish SSH trust first.' }
  & ssh @sshOptions $target "umask 077; : > '$remoteEnv'"
  if ($LASTEXITCODE -ne 0) { throw 'Could not prepare protected environment staging file' }
  $sourceEntries = @('.dockerignore', '.env.example', 'README.md', 'Dockerfile', 'compose.yaml', 'go.mod', 'go.sum', 'cmd', 'internal', 'apps/web', 'db', 'deploy', 'scripts', 'docs/DEPLOYMENT.md', 'docs/GATEWAY.md', 'docs/API_CONTRACT.md')
  & tar -czf $bundle --exclude='node_modules' --exclude='apps/web/dist' --exclude='*.tsbuildinfo' --exclude='.env' --exclude='*.key' --exclude='*.pem' -C $projectRoot @sourceEntries
  if ($LASTEXITCODE -ne 0) { throw 'Could not package project sources' }
  & scp @scpOptions $bundle "${scpTarget}:$remoteBundle"
  if ($LASTEXITCODE -ne 0) { throw 'Source upload failed' }
  & scp @scpOptions $resolvedEnv "${scpTarget}:$remoteEnv"
  if ($LASTEXITCODE -ne 0) { throw 'Environment upload failed' }
  # Source contains no credentials. .env is never printed or inserted into a command.
  $script = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'install.sh')).Replace("`r`n", "`n")
  $command = "chmod 600 '$remoteEnv'; if [ `"`$(id -u)`" -eq 0 ]; then exec sh -s -- '$remoteBundle' '$remoteEnv' '$InstallDirectory'; else exec sudo -n sh -s -- '$remoteBundle' '$remoteEnv' '$InstallDirectory'; fi"
  $script | & ssh @sshOptions $target $command
  if ($LASTEXITCODE -ne 0) { throw 'Remote installation failed; inspect the installer output above' }
} finally {
  # Exact known temporary paths; never recursively removes the project or deployment.
  & ssh @sshOptions $target "rm -f -- '$remoteBundle' '$remoteEnv'" 2>$null
  if (Test-Path -LiteralPath $bundle) { Remove-Item -LiteralPath $bundle -Force }
}
