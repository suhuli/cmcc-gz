# 在 Windows 上构建。用法：  .\build.ps1   或   .\build.ps1 -Version 1.0.0
param([string]$Version = "")
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (-not $Version) {
  $Version = (git describe --tags --always --dirty 2>$null)
  if (-not $Version) { $Version = "dev" }
}
$Version = $Version.TrimStart("v")
$ldflags = "-s -w -X main.version=$Version"
$env:CGO_ENABLED = "0"

if (Get-Command go-winres -ErrorAction SilentlyContinue) {
  go run ./tools/genicon winres
  $num = [regex]::Match($Version, '^\d+(\.\d+){0,3}').Value
  if ($num) {
    go-winres make --in winres/winres.json --out cmd/mcloudmount/rsrc --arch amd64,arm64 --product-version $num --file-version $num
  } else {
    go-winres make --in winres/winres.json --out cmd/mcloudmount/rsrc --arch amd64,arm64
  }
}

go vet ./...
if ($LASTEXITCODE) { exit $LASTEXITCODE }
go test ./...
if ($LASTEXITCODE) { exit $LASTEXITCODE }

if (Test-Path dist) { Remove-Item -Recurse -Force dist }
foreach ($arch in "amd64", "arm64") {
  $out = "dist/mcloudmount-windows-$arch"
  New-Item -ItemType Directory -Force $out | Out-Null
  $env:GOOS = "windows"; $env:GOARCH = $arch
  go build -trimpath -ldflags "$ldflags -H windowsgui" -o "$out/mcloudmount.exe" ./cmd/mcloudmount
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  go build -trimpath -ldflags "$ldflags" -o "$out/mcloudmount-cli.exe" ./cmd/mcloudmount
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  Copy-Item README.md, LICENSE $out
  Compress-Archive -Force -Path $out -DestinationPath "dist/mcloudmount-$Version-windows-$arch.zip"
}
Remove-Item Env:GOOS, Env:GOARCH
Get-ChildItem dist
