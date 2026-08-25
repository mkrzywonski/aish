<#
.SYNOPSIS
  Install aishwin.exe, the Windows half of the aish-driven Windows shell feature.

.DESCRIPTION
  By default this downloads the prebuilt aishwin.exe from the latest GitHub
  release. Pass -Source to build from source instead (installing Go via
  winget if it's missing). Either way the binary (and its required
  side-by-side aishwin.exe.manifest) lands in -InstallDir, which is then
  added to your User PATH if it isn't already there.

  aishwin needs a Linux peer (cmd/aishwnd) reachable via WSL or ssh — this
  script only installs the Windows half; run install.sh inside WSL (or on
  the target Linux host) for the other side.

.PARAMETER Source
  Build from source instead of downloading a prebuilt binary.

.PARAMETER InstallDir
  Where aishwin.exe (and its manifest) are installed. Default:
  $env:LOCALAPPDATA\aishwin

.PARAMETER Version
  A specific release tag instead of "latest".

.PARAMETER Repo
  GitHub "owner/repo" to install from. Default: mkrzywonski/aish

.PARAMETER NoPathUpdate
  Don't modify the User PATH environment variable; just print what's needed.

.EXAMPLE
  iwr https://raw.githubusercontent.com/mkrzywonski/aish/main/install.ps1 | iex

.EXAMPLE
  .\install.ps1 -Source
#>
[CmdletBinding()]
param(
	[switch]$Source,
	[string]$InstallDir = "$env:LOCALAPPDATA\aishwin",
	[string]$Version = "latest",
	[string]$Repo = "mkrzywonski/aish",
	[switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"

function Write-Log($msg) { Write-Host $msg }
function Die($msg) { Write-Error "install.ps1: $msg"; exit 1 }

function Get-ReleaseAssetUrl {
	param([string]$AssetName)
	if ($Version -eq "latest") {
		return "https://github.com/$Repo/releases/latest/download/$AssetName"
	}
	return "https://github.com/$Repo/releases/download/$Version/$AssetName"
}

function Install-Prebuilt {
	$assetName = "aishwin_windows_amd64.zip"
	$url = Get-ReleaseAssetUrl $assetName
	$tmpDir = Join-Path $env:TEMP "aishwin-install-$([guid]::NewGuid())"
	New-Item -ItemType Directory -Path $tmpDir | Out-Null
	try {
		$zipPath = Join-Path $tmpDir $assetName
		Write-Log "downloading $url"
		try {
			Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing
		} catch {
			Write-Log "no prebuilt aishwin for windows/amd64 at $Version ($($_.Exception.Message))"
			return $false
		}
		Expand-Archive -Path $zipPath -DestinationPath $tmpDir -Force
		New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
		Copy-Item (Join-Path $tmpDir "aishwin.exe") (Join-Path $InstallDir "aishwin.exe") -Force
		Copy-Item (Join-Path $tmpDir "aishwin.exe.manifest") (Join-Path $InstallDir "aishwin.exe.manifest") -Force
		return $true
	} finally {
		Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
	}
}

# Ensure-Go makes sure `go` is on PATH for this session, installing it via
# winget if missing -- aishwin has no cgo, so the Go toolchain alone is all a
# from-source install needs (see Install-FromSource for the one required flag).
function Ensure-Go {
	if (Get-Command go -ErrorAction SilentlyContinue) {
		Write-Log "using existing $(go version)"
		return
	}
	if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
		Die "Go isn't installed and winget isn't available; install Go manually from https://go.dev/dl/ and rerun"
	}
	Write-Log "installing Go via winget"
	winget install --id GoLang.Go -e --accept-source-agreements --accept-package-agreements
	# winget updates machine PATH via the registry, but this process's own
	# $env:PATH won't see it until a new shell -- patch it in directly so the
	# rest of this script can call `go` right away.
	$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
	$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
	$env:Path = "$machinePath;$userPath"
	if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
		Die "Go was installed but isn't on PATH yet; open a new PowerShell window and rerun this script"
	}
}

# Get-RepoDir makes a checkout with this project's go.mod available,
# reusing the current directory if install.ps1 is already being run from
# inside one, else cloning a fresh copy.
function Get-RepoDir {
	$goMod = Join-Path (Get-Location) "go.mod"
	if ((Test-Path $goMod) -and (Select-String -Path $goMod -Pattern '^module ai-ssh$' -Quiet)) {
		return (Get-Location).Path
	}
	if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
		Die "git is required to build from source"
	}
	$dir = Join-Path $env:TEMP "aish-src-$([guid]::NewGuid())"
	Write-Log "cloning https://github.com/$Repo.git to $dir"
	git clone --depth 1 "https://github.com/$Repo.git" $dir
	return $dir
}

function Install-FromSource {
	Ensure-Go
	$repoDir = Get-RepoDir
	New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
	Write-Log "building aishwin"
	Push-Location $repoDir
	try {
		# -H=windowsgui links for the GUI subsystem so a shell that launches
		# aishwin gets its prompt back immediately instead of being held until
		# the window closes. It is link-time only -- nothing in the source can
		# set it -- so every build command has to carry it.
		go build -ldflags "-H=windowsgui" -o (Join-Path $InstallDir "aishwin.exe") ./cmd/aishwin
	} finally {
		Pop-Location
	}
	Copy-Item (Join-Path $repoDir "aishwin.exe.manifest") (Join-Path $InstallDir "aishwin.exe.manifest") -Force
}

function Add-InstallDirToUserPath {
	$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
	$entries = $userPath -split ";" | Where-Object { $_ -ne "" }
	if ($entries -contains $InstallDir) {
		return
	}
	if ($NoPathUpdate) {
		Write-Log "$InstallDir isn't on your User PATH; add it yourself, or rerun without -NoPathUpdate"
		return
	}
	$newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
	[Environment]::SetEnvironmentVariable("Path", $newPath, "User")
	$env:Path = "$env:Path;$InstallDir"
	Write-Log "added $InstallDir to your User PATH (open a new terminal to pick it up there)"
}

# Main
if ($Source) {
	Install-FromSource
} else {
	if (-not (Install-Prebuilt)) {
		Write-Log "falling back to building from source"
		Install-FromSource
	}
}

Add-InstallDirToUserPath

Write-Log "installed $InstallDir\aishwin.exe"
Write-Log ""
Write-Log "aishwin needs the Linux half (aishwnd) reachable via WSL or ssh."
Write-Log "Install it there with install.sh from the same repo, e.g. inside WSL:"
Write-Log "  curl -fsSL https://raw.githubusercontent.com/$Repo/main/install.sh | bash -s -- --components aishwnd"
