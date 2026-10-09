# 构建 mcloudmount.exe（PowerShell）
# 构建为无控制台窗口的 GUI 程序：双击即打开面板，日志见 %APPDATA%\mCloudMount\mcloudmount.log
# 用法：在项目根目录执行  .\build.ps1
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

python -m PyInstaller `
  --clean --noconfirm --onefile --noconsole `
  --name mcloudmount `
  --paths "$root\src" `
  --add-data "$root\web;web" `
  --collect-all wsgidav `
  --collect-all cheroot `
  --collect-all defusedxml `
  --collect-all Crypto `
  --collect-submodules pystray `
  --hidden-import PIL.Image `
  --hidden-import PIL.ImageDraw `
  "$root\scripts\entry.py"

if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host ""
Write-Host "Build complete: $root\dist\mcloudmount.exe" -ForegroundColor Green
