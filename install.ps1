# Install pidgeon (and its short alias pdgn) on Windows.
#
#   .\install.ps1               build from this checkout
#   .\install.ps1 -AddToPath    also add the install folder to your user PATH
#
# Destination: $env:INSTALL_DIR, else $env:GOBIN, else (go env GOPATH)\bin.
# Requires Go 1.26+ from https://go.dev/dl/.
param([switch]$AddToPath)
$ErrorActionPreference = "Stop"

$Module = "github.com/Indigo6415/pidgeon"
$Root = $PSScriptRoot

# Read the command names from internal/brand/brand.go (the single source).
$Name = "pidgeon"; $Alias = "pdgn"
$Brand = Join-Path $Root "internal\brand\brand.go"
if (Test-Path $Brand) {
    $text = Get-Content $Brand -Raw
    if ($text -match 'Command = "([^"]+)"') { $Name = $Matches[1] }
    if ($text -match 'Alias = "([^"]+)"') { $Alias = $Matches[1] }
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go is required: install it from https://go.dev/dl/ and re-run."
}

# Pick the destination directory.
if ($env:INSTALL_DIR) { $Dest = $env:INSTALL_DIR }
elseif ((go env GOBIN)) { $Dest = (go env GOBIN) }
else { $Dest = Join-Path ((go env GOPATH).Split(";")[0]) "bin" }
New-Item -ItemType Directory -Force -Path $Dest | Out-Null

$env:CGO_ENABLED = "0"
$Exe = Join-Path $Dest "$Name.exe"
if (Test-Path (Join-Path $Root "go.mod")) {
    $Version = (git -C $Root describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = "dev" }
    Write-Host "building $Name $Version..."
    Push-Location $Root
    try { go build -trimpath -ldflags "-s -w -X $Module/internal/brand.Version=$Version" -o $Exe "./cmd/$Name" }
    finally { Pop-Location }
} else {
    Write-Host "installing $Name from $Module@latest..."
    $env:GOBIN = $Dest
    go install -trimpath -ldflags "-s -w" "$Module/cmd/$Name@latest"
}
if ($LASTEXITCODE -ne 0) { throw "build failed" }

# Windows symlinks need admin rights, so the alias is a copy.
Copy-Item $Exe (Join-Path $Dest "$Alias.exe") -Force
Write-Host "installed $Exe (and $Alias.exe)"

# Check PATH and optionally add the folder for the current user.
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$OnPath = ($env:Path -split ";") -contains $Dest
if (-not $OnPath) {
    if ($AddToPath) {
        [Environment]::SetEnvironmentVariable("Path", "$UserPath;$Dest", "User")
        Write-Host "added $Dest to your user PATH; open a new terminal to use '$Name'."
    } else {
        Write-Host ""
        Write-Host "$Dest is not on your PATH. Re-run with -AddToPath, or add it in"
        Write-Host "Settings > System > About > Advanced system settings > Environment Variables."
    }
} else {
    Write-Host "run '$Name' (or '$Alias') in any folder to share it."
}
