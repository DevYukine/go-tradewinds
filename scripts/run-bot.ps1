param(
    [Parameter(Mandatory = $true)]
    [string]$Binary,
    [Parameter(Mandatory = $true)]
    [string]$Version,
    [Parameter(Mandatory = $true)]
    [string]$RunDir
)

$ErrorActionPreference = 'Stop'
$Binary = (Resolve-Path -LiteralPath $Binary).ProviderPath
$Host.UI.RawUI.WindowTitle = "Tradewinds $Version - live trading"
Set-Location -LiteralPath $RunDir
$env:LOG_LEVEL = 'info'
$env:LOG_COLOR = 'always'
# NO_COLOR overrides LOG_COLOR; a launching shell must not silently strip colors.
Remove-Item Env:NO_COLOR -ErrorAction SilentlyContinue
$logPath = Join-Path (Split-Path -Parent $Binary) "bot-$Version.log"
Write-Host "Tradewinds $Version. Ctrl+C stops trading safely."
Write-Host "Console copy: $logPath"

& cmd.exe /d /c "`"$Binary`" run --execute 2>&1" | Tee-Object -FilePath $logPath -Append
$botExitCode = $LASTEXITCODE
if ($botExitCode -ne 0) {
    Write-Host "Bot exited with code $botExitCode. Inspect the log before restarting." -ForegroundColor Red
    [void](Read-Host 'Press Enter to close this window')
}
exit $botExitCode
