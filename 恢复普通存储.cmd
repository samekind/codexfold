@echo off
setlocal
"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -File "%~dp0scripts\launch-windows-local.ps1" -Action Restore
exit /b %errorlevel%
