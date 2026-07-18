param(
    [string]$Version = $env:MAMARI_VERSION,
    [string]$InstallDir = $(if ($env:MAMARI_INSTALL_DIR) {
        $env:MAMARI_INSTALL_DIR
    } else {
        Join-Path $env:LOCALAPPDATA "Programs\Mamari\bin"
    }),
    [switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"

$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
switch ($architecture) {
    "X64" { $arch = "amd64" }
    "Arm64" { $arch = "arm64" }
    default { throw "mamari installer: unsupported architecture: $architecture" }
}

$repository = if ($env:MAMARI_REPOSITORY) {
    $env:MAMARI_REPOSITORY
} else {
    "waelhoury/mamari"
}
$asset = "mamari_windows_$arch.zip"
if ($env:MAMARI_DOWNLOAD_BASE) {
    $base = $env:MAMARI_DOWNLOAD_BASE.TrimEnd("/")
} elseif ($Version) {
    $base = "https://github.com/$repository/releases/download/$Version"
} else {
    $base = "https://github.com/$repository/releases/latest/download"
}

function Get-ReleaseFile {
    param([string]$Name, [string]$Destination)
    if ($base -match "^https?://") {
        Invoke-WebRequest -Uri "$base/$Name" -OutFile $Destination
    } else {
        Copy-Item -Path (Join-Path $base $Name) -Destination $Destination
    }
}

$temp = Join-Path ([System.IO.Path]::GetTempPath()) ("mamari-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $temp | Out-Null
try {
    $archivePath = Join-Path $temp $asset
    $checksumsPath = Join-Path $temp "checksums.txt"
    Write-Host "Downloading $asset..."
    Get-ReleaseFile -Name $asset -Destination $archivePath
    Get-ReleaseFile -Name "checksums.txt" -Destination $checksumsPath

    $escapedAsset = [regex]::Escape($asset)
    $checksumLine = Get-Content $checksumsPath |
        Where-Object { $_ -match "^\s*([0-9a-fA-F]{64})\s+\*?$escapedAsset\s*$" } |
        Select-Object -First 1
    if (-not $checksumLine) {
        throw "mamari installer: $asset is missing from checksums.txt"
    }
    $expected = [regex]::Match($checksumLine, "([0-9a-fA-F]{64})").Groups[1].Value
    $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash
    if ($actual -ne $expected) {
        throw "mamari installer: checksum verification failed for $asset"
    }

    $unpack = Join-Path $temp "unpack"
    Expand-Archive -Path $archivePath -DestinationPath $unpack
    $binary = Join-Path $unpack "mamari.exe"
    if (-not (Test-Path $binary -PathType Leaf)) {
        throw "mamari installer: archive does not contain mamari.exe"
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $destination = Join-Path $InstallDir "mamari.exe"
    Copy-Item -Force $binary $destination

    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ";" | Where-Object { $_ })
        if ($entries -notcontains $InstallDir) {
            $newPath = (@($entries) + $InstallDir) -join ";"
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        }
        if (($env:Path -split ";") -notcontains $InstallDir) {
            $env:Path = "$env:Path;$InstallDir"
        }
    }

    Write-Host "Installed mamari to $destination"
    Write-Host "Next: cd to a codebase and run 'mamari init --mcp <client>'"
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $temp
}
