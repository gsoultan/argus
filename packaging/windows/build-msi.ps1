# Builds the argus-agent Windows installer from an argus-agent.exe.
#
#   ./build-msi.ps1 -Exe argus-agent.exe -Version 1.4.2 -Arch amd64 -Out argus-agent_1.4.2_windows_amd64.msi
#
# Needs the WiX 5 command line and its Util extension, which -InstallWix adds
# (a .NET tool, so the .NET SDK first). The release builds from the
# argus-agent.exe in its own zip, so the installer and an update from the
# console put the same file in place.
param(
    [Parameter(Mandatory)] [string] $Exe,
    [Parameter(Mandatory)] [string] $Version,
    [ValidateSet('amd64', 'arm64')] [string] $Arch = 'amd64',
    [Parameter(Mandatory)] [string] $Out,
    [switch] $InstallWix
)
$ErrorActionPreference = 'Stop'
$wixVersion = '5.0.2'

if ($InstallWix) {
    dotnet tool install --global wix --version $wixVersion
    if ($LASTEXITCODE -ne 0) { throw 'wix could not be installed' }
    $env:PATH = "$env:PATH;$env:USERPROFILE\.dotnet\tools"
    wix extension add --global "WixToolset.Util.wixext/$wixVersion"
    if ($LASTEXITCODE -ne 0) { throw 'the WiX Util extension could not be added' }
}

# Windows Installer versions are three numbers: 1.4.2-rc.1 installs as 1.4.2.
if ($Version -notmatch '^v?(\d+)\.(\d+)\.(\d+)') { throw "not a version: $Version" }
$productVersion = '{0}.{1}.{2}' -f $Matches[1], $Matches[2], $Matches[3]
$wixArch = @{ amd64 = 'x64'; arm64 = 'arm64' }[$Arch]

$exePath = (Resolve-Path $Exe).Path
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
wix build -arch $wixArch -ext WixToolset.Util.wixext `
    -d "ProductVersion=$productVersion" -d "AgentExe=$exePath" `
    -o $Out (Join-Path $here 'argus-agent.wxs')
if ($LASTEXITCODE -ne 0) { throw 'wix build failed' }
# The .wixpdb is for debugging the installer, not for shipping.
Remove-Item -ErrorAction SilentlyContinue ([IO.Path]::ChangeExtension($Out, '.wixpdb'))
Write-Host "built $Out ($productVersion, $wixArch)"
