<#
.SYNOPSIS
  Builds static Linux executables of oCameraAgent (runs on musl and glibc alike).

.DESCRIPTION
  The agent is pure Go (CGO_ENABLED=0), so the result is a fully static binary with
  no libc dependency: it runs on musl (Alpine, OpenWrt, Buildroot) as well as glibc.

.EXAMPLE
  .\build-musl.ps1                          # amd64 + arm (v7) + arm64 + mipsle
  .\build-musl.ps1 -Arch amd64              # x64 only
  .\build-musl.ps1 -Arch arm -GoArm 5       # ARMv5 camera
  .\build-musl.ps1 -Update                  # update dependencies first (go get -u + tidy)
  .\build-musl.ps1 -Upx                     # also compress with upx if available
#>
[CmdletBinding()]
param(
    [ValidateSet('amd64', '386', 'arm', 'arm64', 'mipsle', 'mips')]
    [string[]] $Arch = @('amd64', 'arm', 'arm64', 'mipsle'),

    [ValidateSet('5', '6', '7')]
    [string] $GoArm = '7',

    [string] $OutDir = 'dist',

    [switch] $Update,
    [switch] $Upx
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'go not found in PATH'
}
Write-Host "Using $(go version)"

if ($Update) {
    Write-Host 'Updating dependencies...'
    go get -u ./...
    if ($LASTEXITCODE -ne 0) { throw 'go get -u failed' }
    go mod tidy
    if ($LASTEXITCODE -ne 0) { throw 'go mod tidy failed' }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$upxCmd = $null
if ($Upx) {
    $upxCmd = Get-Command upx -ErrorAction SilentlyContinue
    if (-not $upxCmd) { Write-Warning 'upx not found, skipping compression' }
}

# Save and restore the caller's environment.
$saved = @{}
foreach ($v in 'GOOS', 'GOARCH', 'GOARM', 'GOMIPS', 'CGO_ENABLED') {
    $saved[$v] = [Environment]::GetEnvironmentVariable($v, 'Process')
}

try {
    foreach ($a in $Arch) {
        $env:GOOS = 'linux'
        $env:GOARCH = $a
        $env:CGO_ENABLED = '0'
        Remove-Item Env:GOARM, Env:GOMIPS -ErrorAction SilentlyContinue

        $name = "oCameraAgent-linux-$a"
        if ($a -eq 'arm') {
            $env:GOARM = $GoArm
            $name = "oCameraAgent-linux-armv$GoArm"
        }
        if ($a -eq 'mipsle' -or $a -eq 'mips') {
            $env:GOMIPS = 'softfloat'   # most camera SoCs have no FPU
        }

        $out = Join-Path $OutDir $name
        Write-Host "Building $name ..."
        go build -trimpath -buildvcs=false -ldflags='-s -w' -o $out .
        if ($LASTEXITCODE -ne 0) { throw "build failed for $a" }

        if ($upxCmd) {
            & $upxCmd.Source --best --lzma -q $out | Out-Null
        }

        $size = [math]::Round((Get-Item $out).Length / 1MB, 2)
        Write-Host ("  -> {0} ({1} MB)" -f $out, $size)
    }
}
finally {
    foreach ($v in $saved.Keys) {
        [Environment]::SetEnvironmentVariable($v, $saved[$v], 'Process')
    }
}

Write-Host 'Done.'
