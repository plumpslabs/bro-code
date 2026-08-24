# BroCode Universal Installer for Windows PowerShell
# Usage:
#   irm https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.ps1 | iex
# or:
#   iwr -useb https://raw.githubusercontent.com/plumpslabs/bro-code/main/scripts/install.ps1 | iex

$ErrorActionPreference = 'Stop'

# Ensure TLS 1.2 or higher is enabled for older PowerShell 5.1 environments
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 -bor [Net.SecurityProtocolType]::Tls13

$Repo = "plumpslabs/bro-code"
$BinaryName = "brocode"
$DefaultTag = "v0.1.41"

# 1. Detect Architecture
$Arch = $env:PROCESSOR_ARCHITECTURE
switch -Regex ($Arch) {
    'AMD64'   { $TargetArch = 'amd64' }
    'ARM64'   { $TargetArch = 'arm64' }
    'x86|i386' { $TargetArch = '386' }
    default   { $TargetArch = 'amd64' }
}

# 2. Determine Install Directory ($HOME\.local\bin or $HOME\.brocode\bin)
$InstallDir = Join-Path $HOME ".local\bin"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

# 3. Query Latest Release from GitHub API
Write-Host "🔍 Finding latest release of BroCode..." -ForegroundColor Cyan
$LatestTag = $DefaultTag
try {
    $ReleaseApiUrl = "https://api.github.com/repos/$Repo/releases/latest"
    $ReleaseJson = Invoke-RestMethod -Uri $ReleaseApiUrl -UseBasicParsing -Headers @{ "User-Agent" = "BroCode-Installer" }
    if ($ReleaseJson.tag_name) {
        $LatestTag = $ReleaseJson.tag_name
    }
} catch {
    Write-Host "⚠️ Could not query GitHub Releases API, using fallback tag $LatestTag" -ForegroundColor Yellow
}

$Ver = $LatestTag.TrimStart('v')
$ArchiveName = "${BinaryName}_${Ver}_windows_${TargetArch}.zip"
$DownloadUrl = "https://github.com/$Repo/releases/download/$LatestTag/$ArchiveName"

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
$ZipPath = Join-Path $TempDir $ArchiveName

try {
    Write-Host "🚀 Installing BroCode $LatestTag for windows/$TargetArch into $InstallDir..." -ForegroundColor Cyan
    Write-Host "📥 Downloading $DownloadUrl..." -ForegroundColor Gray
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing

    # Verify Checksum if checksums.txt exists
    try {
        $ChecksumUrl = "https://github.com/$Repo/releases/download/$LatestTag/checksums.txt"
        $ChecksumFile = Join-Path $TempDir "checksums.txt"
        Invoke-WebRequest -Uri $ChecksumUrl -OutFile $ChecksumFile -UseBasicParsing -ErrorAction SilentlyContinue
        if (Test-Path $ChecksumFile) {
            $ActualHash = (Get-FileHash -Path $ZipPath -Algorithm SHA256).Hash.ToLower()
            $ChecksumLines = Get-Content $ChecksumFile
            $ExpectedLine = $ChecksumLines | Where-Object { $_ -match $ArchiveName }
            if ($ExpectedLine) {
                $ExpectedHash = ($ExpectedLine -split '\s+')[0].ToLower()
                if ($ActualHash -ne $ExpectedHash) {
                    throw "Checksum mismatch! Expected: $ExpectedHash, Actual: $ActualHash"
                }
                Write-Host "✅ Checksum verified ($ActualHash)" -ForegroundColor Green
            }
        }
    } catch {
        Write-Host "⚠️ Checksum verification skipped or unavailable" -ForegroundColor DarkGray
    }

    # Extract Archive
    Write-Host "📦 Extracting binary..." -ForegroundColor Gray
    Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force

    # Find the binary inside the extracted files
    $ExtractedExe = Get-ChildItem -Path $TempDir -Filter "${BinaryName}.exe" -Recurse -File | Select-Object -First 1
    if (-not $ExtractedExe) {
        $ExtractedExe = Get-ChildItem -Path $TempDir -Filter "${BinaryName}" -Recurse -File | Select-Object -First 1
    }

    if (-not $ExtractedExe) {
        throw "Could not find $BinaryName executable inside the extracted archive."
    }

    $TargetExePath = Join-Path $InstallDir "${BinaryName}.exe"
    Copy-Item -Path $ExtractedExe.FullName -Destination $TargetExePath -Force

    Write-Host "✅ Installed $TargetExePath successfully!" -ForegroundColor Green

    # 4. Check & Update User PATH Environment Variable
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathParts = $UserPath -split ';'
    if ($PathParts -notcontains $InstallDir) {
        Write-Host "🔧 Adding $InstallDir to User PATH..." -ForegroundColor Cyan
        $NewUserPath = "$UserPath;$InstallDir".Trim(';')
        [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
        $env:Path = "$env:Path;$InstallDir"
        Write-Host "✨ PATH updated permanently in User Environment." -ForegroundColor Green
    }

    # 5. Print Banner
    Write-Host ""
    Write-Host "┌┐ ┬─┐┌─┐╔═╗┌─┐┌┬┐┌─┐" -ForegroundColor Magenta
    Write-Host "├┴┐├┬┘│ │║  │ │ ││├┤ " -ForegroundColor Magenta
    Write-Host "└─┘┴└─└─┘╚═╝└─┘─┴┘└─┘" -ForegroundColor Magenta
    Write-Host "ship less, ship right" -ForegroundColor Gray
    Write-Host "BroCode $LatestTag installed successfully!" -ForegroundColor Green
    Write-Host ""
    Write-Host "👉 Run: brocode" -ForegroundColor Yellow
    Write-Host ""

} finally {
    # Clean up temp folder
    if (Test-Path $TempDir) {
        Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
