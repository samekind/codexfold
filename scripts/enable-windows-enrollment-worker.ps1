# Kept as the existing entry point; enrollment now uses a persistent SCM service.
param(
    [string]$CodexHome,
    [ValidatePattern('^[D-Zd-z]$')][string]$MountDrive = 'V',
    [switch]$Apply
)
& (Join-Path $PSScriptRoot 'enable-windows-enrollment-service.ps1') -CodexHome $CodexHome -MountDrive $MountDrive -Apply:$Apply
