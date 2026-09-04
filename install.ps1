# Tessera CLI installer — Windows (PowerShell).
#
# Usage:
#   powershell -c "irm https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.ps1 | iex"
#
# Or with a specific version:
#   powershell -c "irm https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.ps1 | iex" -Args v1.0.0

param([string]$Version = "latest")

$Repo = "TesseraStorage/tessera-cli"
$InstallDir = "$env:LOCALAPPDATA\tessera"
$Binary = "tessera-windows-amd64.exe"

$Url = "https://github.com/$Repo/releases/latest/download/$Binary"
if ($Version -ne "latest") {
  $Url = "https://github.com/$Repo/releases/download/$Version/$Binary"
}

Write-Host "→ Installing Tessera CLI $Version for Windows (amd64)"
Write-Host "  from: $Url"
Write-Host "  to:   $InstallDir\tessera.exe"
Write-Host ""

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Invoke-WebRequest -Uri $Url -OutFile "$InstallDir\tessera.exe"

# Add to PATH for the current user
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($UserPath -notlike "*$InstallDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
  $env:Path += ";$InstallDir"
  Write-Host "Added to PATH. Restart your terminal, then run:"
} else {
  Write-Host "Already in PATH. Run:"
}

Write-Host "  tessera login"