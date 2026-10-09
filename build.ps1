@echo off
setlocal
set SCRIPT_DIR=%~dp0
python -m PyInstaller ^
  --clean ^
  --onefile ^
  --name mcloudmount ^
  --paths "%SCRIPT_DIR%src" ^
  --collect-all wsgidav ^
  --collect-all cheroot ^
  --collect-all defusedxml ^
  --collect-all Crypto ^
  "%SCRIPT_DIR%scripts\entry.py"

if %ERRORLEVEL% NEQ 0 exit /b %ERRORLEVEL%
echo.
echo Build complete: %SCRIPT_DIR%dist\mcloudmount.exe
