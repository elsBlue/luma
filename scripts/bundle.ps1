$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root "dist"
$stage = Join-Path $dist "stage"
$tmp = Join-Path $dist "tmp"
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
New-Item -ItemType Directory -Force (Join-Path $stage "tools") | Out-Null
New-Item -ItemType Directory -Force $tmp | Out-Null

Copy-Item (Join-Path $dist "Luma.exe") (Join-Path $stage "Luma.exe")
Copy-Item (Join-Path $root "README.md") (Join-Path $stage "README.txt")
Copy-Item (Join-Path $root "LICENSE") (Join-Path $stage "LICENSE.txt")
Copy-Item (Join-Path $root "NOTICE") (Join-Path $stage "NOTICE.txt")
$licDir = Join-Path $stage "THIRD_PARTY_LICENSES"
New-Item -ItemType Directory -Force $licDir | Out-Null
Copy-Item (Join-Path $root "THIRD_PARTY_LICENSES\*") $licDir

$tools = Join-Path $stage "tools"
$serverUrl = "https://github.com/Genymobile/scrcpy/releases/download/v5.0.1/scrcpy-server-v5.0.1"
$adbUrl = "https://dl.google.com/android/repository/platform-tools-latest-windows.zip"
$ffUrl = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-lgpl.zip"

Write-Host "phone service"
Invoke-WebRequest -Uri $serverUrl -OutFile (Join-Path $tools "luma-server") -UseBasicParsing
Write-Host "usb bridge"
Invoke-WebRequest -Uri $adbUrl -OutFile (Join-Path $tmp "adb.zip") -UseBasicParsing
Write-Host "decoder"
Invoke-WebRequest -Uri $ffUrl -OutFile (Join-Path $tmp "ffmpeg.zip") -UseBasicParsing

Expand-Archive (Join-Path $tmp "adb.zip") -DestinationPath (Join-Path $tmp "adb")
Expand-Archive (Join-Path $tmp "ffmpeg.zip") -DestinationPath (Join-Path $tmp "ff")

$adb = Get-ChildItem -Recurse (Join-Path $tmp "adb") -Filter adb.exe | Select-Object -First 1
if (-not $adb) { throw "adb.exe missing from platform-tools" }
Get-ChildItem $adb.DirectoryName | Where-Object { $_.Name -eq "adb.exe" -or $_.Extension -eq ".dll" } | Copy-Item -Destination $tools

$ff = Get-ChildItem -Recurse (Join-Path $tmp "ff") -Filter ffmpeg.exe | Select-Object -First 1
if (-not $ff) { throw "ffmpeg.exe missing" }
Get-ChildItem $ff.DirectoryName | Where-Object { $_.Name -eq "ffmpeg.exe" -or $_.Extension -eq ".dll" } | Copy-Item -Destination $tools

$zip = Join-Path $dist "Luma-Windows.zip"
if (Test-Path $zip) { Remove-Item $zip }
Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $zip
Write-Host "wrote $zip"
