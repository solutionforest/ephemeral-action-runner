@echo off
setlocal

powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0run-epar.ps1" %*

exit /b %ERRORLEVEL%
