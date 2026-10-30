# Tessera CLI installer — Windows PowerShell.
#
# Downloads a tessera-cli release asset over HTTPS, verifies its SHA-256
# against the release's published SHA256SUMS, and installs it into a
# user-writable directory. It never needs Administrator, and it refuses to
# install when the checksum is missing or does not match.
#
#   iwr -useb https://tessera.storage/install.ps1 | iex
#   & ([scriptblock]::Create((iwr -useb https://tessera.storage/install.ps1))) v1.11.4   # a specific version
#
# PATH: installs to %LOCALAPPDATA%\Tessera\bin and adds that directory to your
# user PATH (new terminals pick it up). Set TESSERA_NO_MODIFY_PATH=1 to skip.
#
# Network: installing needs HTTPS to github.com. Using Tessera afterwards needs
# HTTPS to tessera.storage plus outbound TCP to the storage hosts (usually
# port 9984; some hosts announce other ports).
#
# Environment overrides (mainly for tests, forks and air-gapped mirrors):
#   TESSERA_INSTALL_DIR       install directory
#   TESSERA_NO_MODIFY_PATH=1  never change the user PATH
#   TESSERA_RELEASE_REPO      GitHub owner/repo to download from instead of
#                             TesseraStorage/tessera-cli (prints a warning)
#   TESSERA_RELEASE_BASE_URL  base URL holding the asset + SHA256SUMS
param([string]$Version = 'latest')
$ErrorActionPreference = 'Stop'

$official = 'TesseraStorage/tessera-cli'
$repo = if ($env:TESSERA_RELEASE_REPO) { $env:TESSERA_RELEASE_REPO } else { $official }
$defaultBase = if ($Version -eq 'latest') { "https://github.com/$repo/releases/latest/download" } else { "https://github.com/$repo/releases/download/$Version" }
$base = if ($env:TESSERA_RELEASE_BASE_URL) { $env:TESSERA_RELEASE_BASE_URL } else { $defaultBase }
$dest = if ($env:TESSERA_INSTALL_DIR) { $env:TESSERA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Tessera\bin' }
$asset = 'tessera-windows-amd64.exe'

function Die($m) { Write-Error "install: $m"; exit 1 }

if ($repo -ne $official) {
    Write-Warning "TESSERA_RELEASE_REPO is set: downloading from github.com/$repo, not the official $official. Only continue if you trust that repository."
}
if ($env:TESSERA_RELEASE_BASE_URL) {
    Write-Warning "TESSERA_RELEASE_BASE_URL is set: downloading from $base."
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("tessera-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "[1/4] downloading $asset ($Version)"
    Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile (Join-Path $tmp $asset)

    Write-Host "[2/4] downloading SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing "$base/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')

    Write-Host "[3/4] verifying checksum"
    $line = Get-Content (Join-Path $tmp 'SHA256SUMS') |
        Where-Object { $_ -match "\s\*?$([regex]::Escape($asset))$" } |
        Select-Object -First 1
    if (-not $line) { Die "no checksum for $asset in SHA256SUMS - refusing to install" }
    $expected = ($line -split '\s+')[0].ToLower()
    $actual = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) {
        Die "checksum mismatch for $asset (expected $expected, got $actual) - refusing to install"
    }
    Write-Host "      ok: $asset $actual"

    Write-Host "[4/4] installing to $dest\tessera.exe"
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item (Join-Path $tmp $asset) (Join-Path $dest 'tessera.exe') -Force

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $onPath = ($userPath -split ';') -contains $dest
    if (-not $onPath) {
        if ($env:TESSERA_NO_MODIFY_PATH -eq '1') {
            Write-Host "      $dest is not on your PATH (TESSERA_NO_MODIFY_PATH=1, left unchanged)."
        } else {
            [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
            Write-Host "      Added $dest to your user PATH (open a new terminal)."
        }
        Write-Host "      For this terminal:  `$env:Path += `";$dest`""
    }
    & (Join-Path $dest 'tessera.exe') version 2>$null
    Write-Host ""
    Write-Host "Next: tessera signup   (needs outbound TCP to storage hosts, usually port 9984)"
    Write-Host "Done."
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
