@echo off
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0run-audit.ps1"
exit /b %ERRORLEVEL%
