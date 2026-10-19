# Installs Outloud for the current user, without admin rights: copies it to
# %LOCALAPPDATA%\Programs\Outloud, adds it to the Start menu and to
# Settings > Apps > Installed apps, and starts it.
#
#   From GitHub:   irm https://github.com/thomasreichmann/outloud/releases/latest/download/install-windows.ps1 | iex
#   From the zip:  run install.cmd
#   Uninstall:     Settings > Apps > Installed apps > Outloud, or install.ps1 -Uninstall
param([switch]$Uninstall)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar slows downloads to a crawl
$version = '__VERSION__'
$releaseBase = '__RELEASE_BASE__'
$dest = Join-Path $env:LOCALAPPDATA 'Programs\Outloud'
$shortcut = Join-Path ([Environment]::GetFolderPath('Programs')) 'Outloud.lnk'
$uninstallKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\Outloud'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'

function Stop-Outloud {
    # Ask it to close first: a killed tray app leaves a dead icon behind.
    if (Get-Process Outloud -ErrorAction SilentlyContinue) {
        taskkill.exe /IM Outloud.exe | Out-Null
        for ($i = 0; $i -lt 30 -and (Get-Process Outloud -ErrorAction SilentlyContinue); $i++) {
            Start-Sleep -Milliseconds 100
        }
        Get-Process Outloud -ErrorAction SilentlyContinue | Stop-Process -Force
    }
}

if ($Uninstall) {
    Stop-Outloud
    Remove-ItemProperty -Path $runKey -Name Outloud -ErrorAction SilentlyContinue
    Remove-Item -Force $shortcut -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $uninstallKey -ErrorAction SilentlyContinue
    foreach ($dir in $dest, (Join-Path $env:APPDATA 'Outloud'), (Join-Path $env:LOCALAPPDATA 'Outloud')) {
        Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
    }
    Write-Host 'Outloud is uninstalled. To remove the receiver from a Mac, run this there:'
    Write-Host '    ~/Library/Application\ Support/Outloud/outloud-receiver uninstall'
    return
}

# The files come from beside this script, or, when it runs through
# "irm | iex", from the GitHub release it belongs to.
$src = $PSScriptRoot
$tmp = $null
if (-not $src -or -not (Test-Path (Join-Path $src 'Outloud.exe'))) {
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('outloud-' + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    $zip = Join-Path $tmp 'Outloud.zip'
    Write-Host "Downloading Outloud $version ..."
    Invoke-WebRequest -UseBasicParsing "$releaseBase/Outloud-$version-windows-x64.zip" -OutFile $zip
    Expand-Archive -Path $zip -DestinationPath $tmp
    $src = Join-Path $tmp 'Outloud'
}

Stop-Outloud
New-Item -ItemType Directory -Force -Path (Join-Path $dest 'mac') | Out-Null
if ((Resolve-Path $src).ProviderPath.TrimEnd('\') -ne $dest.TrimEnd('\')) {
    Copy-Item -Force (Join-Path $src '*.exe'), (Join-Path $src 'install.ps1'), (Join-Path $src 'LICENSE') $dest
    Copy-Item -Force (Join-Path $src 'mac\*') (Join-Path $dest 'mac')
}
if ($tmp) { Remove-Item -Recurse -Force $tmp }
# Files from a zip downloaded with a browser carry the "from the internet" mark.
Get-ChildItem -Recurse -File $dest | Unblock-File

$exe = Join-Path $dest 'Outloud.exe'
$link = (New-Object -ComObject WScript.Shell).CreateShortcut($shortcut)
$link.TargetPath = $exe
$link.WorkingDirectory = $dest
$link.Description = "Play this PC's sound on a Mac"
$link.Save()

New-Item -Force -Path $uninstallKey | Out-Null
$strings = @{
    DisplayName     = 'Outloud'
    DisplayVersion  = $version
    Publisher       = 'thomasreichmann'
    DisplayIcon     = $exe
    InstallLocation = $dest
    URLInfoAbout    = 'https://github.com/thomasreichmann/outloud'
    UninstallString = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$dest\install.ps1`" -Uninstall"
}
foreach ($name in $strings.Keys) {
    New-ItemProperty -Force -Path $uninstallKey -Name $name -Value $strings[$name] | Out-Null
}
$sizeKB = [int]((Get-ChildItem -Recurse -File $dest | Measure-Object Length -Sum).Sum / 1KB)
foreach ($pair in @(@('NoModify', 1), @('NoRepair', 1), @('EstimatedSize', $sizeKB))) {
    New-ItemProperty -Force -Path $uninstallKey -Name $pair[0] -Value $pair[1] -PropertyType DWord | Out-Null
}

Start-Process $exe
Write-Host "Installed Outloud $version to $dest"
Write-Host 'It is running: look for the speaker icon in the notification area. Its menu has "Set up a Mac...".'
