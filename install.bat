@echo off
rem Install pidgeon (and its short alias pdgn) on Windows.
rem
rem   install.bat               build from this checkout (double-click works too)
rem   install.bat -AddToPath    also add the install folder to your user PATH
rem
rem This is a wrapper around install.ps1, which does the real work. It exists so
rem the installer runs with a double-click and isn't blocked by PowerShell's
rem execution policy. Every argument is passed straight through.
setlocal

rem Bail out early with a clear message if install.ps1 isn't next to this file.
if not exist "%~dp0install.ps1" (
    echo install.ps1 not found next to %~nx0
    set "RC=1"
    goto :done
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
set "RC=%ERRORLEVEL%"

:done
rem When started from Explorer the window would close before the output can be
rem read, so wait for a key. cmdcmdline only contains this script's name then.
echo %cmdcmdline% | findstr /i /c:"%~nx0" >nul && pause
exit /b %RC%
