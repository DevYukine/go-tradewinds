param(
    [ValidatePattern('^[0-9a-fA-F]{7,40}(-dirty)?$')]
    [string]$Version,
    [string]$RunDir,
    [string]$DeliveryDir,
    [switch]$AllowDirty
)

$ErrorActionPreference = 'Stop'
$logPath = $null
$stagedBinary = $null
$shutdownStarted = $false
$relaunchCommand = $null
$isRollback = -not [string]::IsNullOrWhiteSpace($Version)

function Get-BinaryIdentity {
    param([string]$BinaryPath)
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $binaryInfo = @(& go version -m $BinaryPath 2>&1)
        if ($LASTEXITCODE -ne 0) {
            return $null
        }
    } catch {
        return $null
    } finally {
        $ErrorActionPreference = $previousPreference
    }
    $buildRevision = $null
    $buildModified = $null
    foreach ($entry in $binaryInfo) {
        if ([string]$entry -match '^\s*build\s+vcs\.revision=([0-9a-fA-F]{40})\s*$') {
            $buildRevision = $Matches[1]
        } elseif ([string]$entry -match '^\s*build\s+vcs\.modified=(true|false)\s*$') {
            $buildModified = $Matches[1] -eq 'true'
        }
    }
    return [PSCustomObject]@{ Revision = $buildRevision; Modified = $buildModified }
}

function ConvertTo-PowerShellLiteral {
    param([string]$Value)
    # The tokenizer also treats typographic single quotes as delimiters.
    $escaped = [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($Value)
    return "'" + $escaped + "'"
}

function Sync-StagedBinaries {
    param([string]$Directory)
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        return
    }
    $runningPaths = @(Get-Process -Name 'tradewinds*' -ErrorAction SilentlyContinue | ForEach-Object {
        if ($_.Path) { [IO.Path]::GetFullPath($_.Path) }
    })
    $staged = @(Get-ChildItem -LiteralPath $Directory -Filter 'tradewinds-*-stage-*.exe' -File | ForEach-Object {
        if ($_.Name -match '^tradewinds-(?<version>[0-9a-fA-F]{7,40}(?:-dirty)?)-stage-[0-9a-fA-F]{32}\.exe$') {
            [PSCustomObject]@{ Version = $Matches['version']; File = $_ }
        }
    })
    foreach ($group in ($staged | Group-Object Version)) {
        $prefix = $group.Name -replace '-dirty$', ''
        $canonical = Join-Path $Directory "tradewinds-$($group.Name).exe"
        $canonicalFile = Get-Item -LiteralPath $canonical -ErrorAction SilentlyContinue
        $canonicalIdentity = $null
        if ($canonicalFile) {
            $canonicalIdentity = Get-BinaryIdentity -BinaryPath $canonical
        }
        $canonicalVerified = $canonicalIdentity -and $canonicalIdentity.Revision -and
            $null -ne $canonicalIdentity.Modified -and
            $canonicalIdentity.Revision.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
        $newest = $null
        foreach ($candidate in ($group.Group | Sort-Object { $_.File.LastWriteTimeUtc } -Descending)) {
            $candidateIdentity = Get-BinaryIdentity -BinaryPath $candidate.File.FullName
            if ($candidateIdentity -and $candidateIdentity.Revision -and $null -ne $candidateIdentity.Modified -and
                $candidateIdentity.Revision.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
                $newest = $candidate.File
                break
            }
        }
        $keepStaged = $null
        if ($newest -and (-not $canonicalVerified -or $newest.LastWriteTimeUtc -gt $canonicalFile.LastWriteTimeUtc)) {
            if ($runningPaths -contains $canonical -or $runningPaths -contains $newest.FullName) {
                $keepStaged = $newest.FullName
            } else {
                Move-Item -LiteralPath $newest.FullName -Destination $canonical -Force
                $canonicalVerified = $true
                Write-Host "Promoted verified staged executable to $canonical"
            }
        }
        if (-not $newest -and -not $canonicalVerified) {
            continue
        }
        foreach ($candidate in $group.Group) {
            $path = $candidate.File.FullName
            if ($path -eq $keepStaged -or $runningPaths -contains $path -or -not (Test-Path -LiteralPath $path -PathType Leaf)) {
                continue
            }
            try {
                Remove-Item -LiteralPath $path -Force
            } catch {
                Write-Warning "Could not remove stale staged executable: $path"
            }
        }
    }
}

try {
    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    $previousNativePreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $worktrees = @(& git -C $repoRoot worktree list --porcelain)
        $gitExit = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousNativePreference
    }
    if ($gitExit -ne 0) {
        throw 'Cannot resolve the main Git worktree.'
    }
    $mainEntry = $worktrees | Where-Object { $_.StartsWith('worktree ') } | Select-Object -First 1
    if (-not $mainEntry) {
        throw 'Git did not report a main worktree.'
    }
    $mainWorktree = [IO.Path]::GetFullPath($mainEntry.Substring(9))

    if (-not $RunDir) {
        foreach ($candidate in @($repoRoot, $mainWorktree)) {
            if (Test-Path -LiteralPath (Join-Path $candidate '.env') -PathType Leaf) {
                $RunDir = $candidate
                break
            }
        }
    }
    if (-not $RunDir) {
        throw 'No .env was found in the repository root or main worktree. Supply -RunDir.'
    }
    $RunDir = [IO.Path]::GetFullPath($RunDir)
    $envPath = Join-Path $RunDir '.env'
    if (-not (Test-Path -LiteralPath $envPath -PathType Leaf)) {
        throw 'The run directory must contain .env.'
    }
    if (-not $DeliveryDir) {
        $DeliveryDir = "${mainWorktree}-runtime-delivery"
    }
    $DeliveryDir = [IO.Path]::GetFullPath($DeliveryDir)
    $deliveryPrefix = $DeliveryDir.TrimEnd([char[]]'\/') + [IO.Path]::DirectorySeparatorChar

    # Read only HTTP_ADDR; never import or print the rest of .env.
    $httpAddr = '127.0.0.1:8585'
    foreach ($line in [IO.File]::ReadLines($envPath)) {
        if ($line -cmatch '^\s*(?:export\s+)?HTTP_ADDR\s*=\s*(?:"([^"]*)"|''([^'']*)''|([^\s#]*))\s*(?:#.*)?$') {
            $value = $Matches[1]
            if (-not $value) { $value = $Matches[2] }
            if (-not $value) { $value = $Matches[3] }
            if ($value) { $httpAddr = $value }
            break
        }
    }
    if ($httpAddr -notmatch '^(?<host>\[[0-9a-fA-F:]+\]|[^:/\s?#@]*):(?<port>[0-9]{1,5})$') {
        throw 'HTTP_ADDR must contain a host and port.'
    }
    $listenHost = $Matches['host']
    $listenPort = [int]$Matches['port']
    if ($listenPort -lt 1 -or $listenPort -gt 65535) {
        throw 'HTTP_ADDR must use a port between 1 and 65535.'
    }
    if ($listenHost -eq '' -or $listenHost -eq '0.0.0.0') {
        $listenHost = '127.0.0.1'
    } elseif ($listenHost -eq '[::]') {
        $listenHost = '[::1]'
    }
    $baseUrl = "http://${listenHost}:$listenPort"

    if (-not $isRollback) {
        $previousNativePreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'Continue'
            $revision = & git -C $repoRoot rev-parse --verify HEAD
            if ($LASTEXITCODE -ne 0 -or $revision -notmatch '^[0-9a-fA-F]{40}$') {
                throw 'Cannot resolve HEAD.'
            }
            if (-not $AllowDirty) {
                $trackedChanges = @(& git -C $repoRoot diff --name-only $revision -- cmd/ internal/ go.mod go.sum)
                if ($LASTEXITCODE -ne 0) {
                    throw 'Cannot list uncommitted Go changes.'
                }
                if ($trackedChanges.Count -gt 0) {
                    Write-Host 'Tracked Go changes must be committed or explicitly deployed with -AllowDirty:' -ForegroundColor Red
                    $trackedChanges | ForEach-Object { Write-Host "  $_" }
                    throw 'Refusing to deploy while tracked Go changes are uncommitted.'
                }
                $untrackedChanges = @(& git -C $repoRoot ls-files --others --exclude-standard -- cmd/ internal/ go.mod go.sum)
                if ($LASTEXITCODE -ne 0) {
                    throw 'Cannot list untracked Go changes.'
                }
                if ($untrackedChanges.Count -gt 0) {
                    Write-Host 'Untracked Go files will NOT be deployed; the default build uses committed HEAD:' -ForegroundColor Yellow
                    $untrackedChanges | ForEach-Object { Write-Host "  $_" }
                }
            }
        } finally {
            $ErrorActionPreference = $previousNativePreference
        }
        $Version = $revision.Substring(0, 7)
        if ($AllowDirty) {
            $Version += '-dirty'
        }
        [void](New-Item -ItemType Directory -Path $DeliveryDir -Force)
        Sync-StagedBinaries -Directory $DeliveryDir
        $binary = Join-Path $DeliveryDir "tradewinds-$Version.exe"
        $logPath = Join-Path $DeliveryDir "bot-$Version.log"
        $buildNeeded = $AllowDirty -or -not (Test-Path -LiteralPath $binary -PathType Leaf)
        if (-not $buildNeeded) {
            $cachedIdentity = Get-BinaryIdentity -BinaryPath $binary
            if ($cachedIdentity -and $cachedIdentity.Revision -eq $revision -and $cachedIdentity.Modified -eq $false) {
                Write-Host "Reusing existing clean HEAD binary: $binary"
            } else {
                Write-Host 'The cached binary is not a verified clean HEAD build; rebuilding from detached HEAD.' -ForegroundColor Yellow
                $buildNeeded = $true
            }
        }
        if ($buildNeeded) {
            # Stage dirty replacements too: the selected old executable may still be open.
            $stagedBinary = Join-Path $DeliveryDir "tradewinds-$Version-stage-$([Guid]::NewGuid().ToString('N')).exe"
            $buildRoot = $repoRoot
            $buildWorktree = $null
            $previousGoWork = $env:GOWORK
            $previousGoFlags = $env:GOFLAGS
            $previousBuildPreference = $ErrorActionPreference
            $buildHadError = $false
            try {
                $ErrorActionPreference = 'Continue'
                if (-not $AllowDirty) {
                    $buildCandidate = Join-Path ([IO.Path]::GetTempPath()) "tradewinds-build-$([Guid]::NewGuid().ToString('N'))"
                    $disabledHooks = Join-Path ([IO.Path]::GetTempPath()) "tradewinds-no-hooks-$([Guid]::NewGuid().ToString('N'))"
                    & git -c "core.hooksPath=$disabledHooks" -C $repoRoot worktree add --quiet --detach $buildCandidate $revision
                    if ($LASTEXITCODE -ne 0) {
                        throw "Cannot create the temporary HEAD build worktree. Check for a partial worktree at $buildCandidate"
                    }
                    $buildWorktree = $buildCandidate
                    $buildRoot = $buildWorktree
                }
                Write-Host "Building Tradewinds $Version before stopping any process."
                $env:GOWORK = 'off'
                $env:GOFLAGS = '-mod=readonly'
                & go -C $buildRoot build -mod=readonly -trimpath -o $stagedBinary ./cmd/tradewinds
                if ($LASTEXITCODE -ne 0) {
                    throw 'Go build failed; the running bot was not touched.'
                }
            } catch {
                $buildHadError = $true
                throw
            } finally {
                $ErrorActionPreference = $previousBuildPreference
                $env:GOWORK = $previousGoWork
                $env:GOFLAGS = $previousGoFlags
                if ($buildWorktree) {
                    $previousCleanupPreference = $ErrorActionPreference
                    try {
                        $ErrorActionPreference = 'Continue'
                        $worktreeRemoved = $false
                        for ($attempt = 0; $attempt -lt 2; $attempt++) {
                            & git -C $repoRoot worktree remove -f -f $buildWorktree
                            if ($LASTEXITCODE -eq 0) {
                                $worktreeRemoved = $true
                                break
                            }
                            if ($attempt -eq 0) {
                                Start-Sleep -Milliseconds 200
                            }
                        }
                        & git -C $repoRoot worktree prune
                        if (-not $worktreeRemoved -or $LASTEXITCODE -ne 0 -or (Test-Path -LiteralPath $buildWorktree)) {
                            throw "Temporary build worktree cleanup failed; possible leak at $buildWorktree. Inspect git worktree list and remove only that temporary path."
                        }
                    } catch {
                        if ($buildHadError) {
                            Write-Warning $_.Exception.Message
                        } else {
                            throw
                        }
                    } finally {
                        $ErrorActionPreference = $previousCleanupPreference
                    }
                }
            }
        }
    } else {
        Sync-StagedBinaries -Directory $DeliveryDir
        $binary = Join-Path $DeliveryDir "tradewinds-$Version.exe"
        $logPath = Join-Path $DeliveryDir "bot-$Version.log"
        if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
            throw "The requested existing binary does not exist: $binary"
        }
        Write-Host "Using existing version $Version without building."
    }

    $inspectionPath = $binary
    if ($stagedBinary) {
        $inspectionPath = $stagedBinary
    }
    $identity = Get-BinaryIdentity -BinaryPath $inspectionPath
    if (-not $identity) {
        throw 'Cannot inspect the selected Go binary with go version -m; the running bot was not touched.'
    }
    $buildRevision = $identity.Revision
    $buildModified = $identity.Modified
    $revisionPrefix = $Version -replace '-dirty$', ''
    if ($buildRevision -and -not $buildRevision.StartsWith($revisionPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'The binary VCS revision does not match its requested version; the running bot was not touched.'
    }
    $hasVcsIdentity = $buildRevision -and $null -ne $buildModified
    if (-not $isRollback -and (-not $hasVcsIdentity -or $buildRevision -ne $revision)) {
        throw 'A new HEAD deployment must have matching VCS revision and modified metadata; the running bot was not touched.'
    }
    if (-not $isRollback -and -not $AllowDirty -and $buildModified -ne $false) {
        throw 'A default HEAD deployment requires vcs.modified=false; the running bot was not touched.'
    }
    if ($isRollback -and $buildModified -eq $true) {
        Write-Host 'Warning: binary built from a modified tree; explicit -Version selected this artifact.' -ForegroundColor Yellow
    }
    if ($hasVcsIdentity) {
        Write-Host "Binary metadata: revision=$buildRevision, modified=$buildModified"
    } else {
        Write-Host 'Warning: rollback binary revision unverifiable; only process-path/readiness confirmation will be possible.' -ForegroundColor Yellow
    }

    if ($stagedBinary) {
        $runningTarget = @(Get-Process -Name 'tradewinds*' -ErrorAction SilentlyContinue | Where-Object {
            $_.Path -and [IO.Path]::GetFullPath($_.Path).Equals($binary, [StringComparison]::OrdinalIgnoreCase)
        })
        if ($runningTarget.Count -gt 0) {
            $binary = $stagedBinary
            Write-Host "The target executable is running; the verified replacement will launch from $binary"
        } else {
            Move-Item -LiteralPath $stagedBinary -Destination $binary -Force
            $stagedBinary = $null
        }
    }

    $runner = Join-Path $PSScriptRoot 'run-bot.ps1'
    if (-not (Test-Path -LiteralPath $runner -PathType Leaf)) {
        throw 'scripts/run-bot.ps1 is missing.'
    }
    $powershell = Join-Path $PSHOME 'powershell.exe'
    if (-not (Test-Path -LiteralPath $powershell -PathType Leaf)) {
        $powershell = (Get-Command powershell.exe -ErrorAction Stop).Source
    }
    $relaunchCommand = @(
        '&', (ConvertTo-PowerShellLiteral $powershell),
        '-NoProfile', '-ExecutionPolicy', "'Bypass'",
        '-File', (ConvertTo-PowerShellLiteral $runner),
        '-Binary', (ConvertTo-PowerShellLiteral $binary),
        '-Version', (ConvertTo-PowerShellLiteral $Version),
        '-RunDir', (ConvertTo-PowerShellLiteral $RunDir)
    ) -join ' '

    # Explorer owns the launch, so the console does not inherit this caller's job.
    $shortcutPath = Join-Path $DeliveryDir "Tradewinds-$Version.lnk"
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($shortcutPath)
    $shortcut.TargetPath = $powershell
    $shortcut.Arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$runner`" -Binary `"$binary`" -Version `"$Version`" -RunDir `"$RunDir`""
    $shortcut.WorkingDirectory = $RunDir
    $shortcut.WindowStyle = 1
    $shortcut.Description = "Tradewinds $Version"
    $shortcut.Save()

    # Only dedicated run-bot launchers for this delivery directory may be closed.
    $launchers = @(Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe' OR Name = 'pwsh.exe'" | Where-Object {
        if ($_.ProcessId -eq $PID -or $_.CommandLine -notmatch '(?i)(?:^|\s)-File\s+(?:"(?<script>[^"]+)"|(?<script>\S+))') {
            return $false
        }
        if ([IO.Path]::GetFileName($Matches['script']) -ne 'run-bot.ps1') {
            return $false
        }
        if ($_.CommandLine -notmatch '(?i)(?:^|\s)-Binary\s+(?:"(?<binary>[^"]+)"|(?<binary>\S+))') {
            return $false
        }
        $launcherBinary = [IO.Path]::GetFullPath($Matches['binary'])
        return $launcherBinary.StartsWith($deliveryPrefix, [StringComparison]::OrdinalIgnoreCase) -and [IO.Path]::GetFileName($launcherBinary) -like 'tradewinds*.exe'
    })
    $bots = @(Get-Process -Name 'tradewinds*' -ErrorAction SilentlyContinue | Where-Object {
        $_.Path -and [IO.Path]::GetFullPath($_.Path).StartsWith($deliveryPrefix, [StringComparison]::OrdinalIgnoreCase)
    })
    $allowedConsoleIds = @($bots | ForEach-Object { $_.Id }) + @($launchers | ForEach-Object { $_.ProcessId })
    foreach ($bot in $bots) {
        $botInfo = Get-CimInstance Win32_Process -Filter "ProcessId = $($bot.Id)"
        if ($botInfo) {
            $parent = Get-CimInstance Win32_Process -Filter "ProcessId = $($botInfo.ParentProcessId)"
            if ($parent -and $parent.Name -eq 'cmd.exe' -and $launchers.ProcessId -contains $parent.ParentProcessId) {
                $allowedConsoleIds += $parent.ProcessId
            }
        }
    }
    $allowedConsoleIds = @($allowedConsoleIds | Sort-Object -Unique) -join ', '
    $stopDeadline = [DateTime]::UtcNow.AddSeconds(30)
    $shutdownStarted = $true
    foreach ($bot in $bots) {
        Write-Host "Stopping $($bot.ProcessName) (PID $($bot.Id))."
        # Keep the process handle bound to this instance while waiting for exit.
        [void]$bot.Handle
        $signalScript = @"
Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class TradewindsConsoleSignal {
    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern bool FreeConsole();
    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern bool AttachConsole(uint processId);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetConsoleCtrlHandler(IntPtr handler, bool add);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint GetConsoleProcessList([Out] uint[] processes, uint count);
    [DllImport("kernel32.dll")]
    private static extern uint GetCurrentProcessId();
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GenerateConsoleCtrlEvent(uint controlEvent, uint processGroupId);

    public static bool Interrupt(uint target, uint[] allowed) {
        FreeConsole();
        if (!AttachConsole(target)) {
            return false;
        }
        try {
            if (!SetConsoleCtrlHandler(IntPtr.Zero, true)) {
                return false;
            }
            uint[] processes = new uint[64];
            uint count = GetConsoleProcessList(processes, (uint)processes.Length);
            if (count == 0 || count > int.MaxValue) {
                return false;
            }
            if (count > processes.Length) {
                processes = new uint[(int)count];
                count = GetConsoleProcessList(processes, (uint)processes.Length);
                if (count == 0 || count > processes.Length) {
                    return false;
                }
            }
            uint self = GetCurrentProcessId();
            for (int i = 0; i < count; i++) {
                if (processes[i] != self && Array.IndexOf(allowed, processes[i]) < 0) {
                    return false;
                }
            }
            if (!GenerateConsoleCtrlEvent(0, 0)) {
                return false;
            }
            System.Threading.Thread.Sleep(500);
            return true;
        } finally {
            FreeConsole();
        }
    }
}
'@
if (-not [TradewindsConsoleSignal]::Interrupt($($bot.Id), [uint32[]]@($allowedConsoleIds))) { exit 1 }
"@
        $encodedSignal = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($signalScript))
        [void](Start-Process -FilePath $powershell -WindowStyle Hidden -ArgumentList "-NoProfile -ExecutionPolicy Bypass -EncodedCommand $encodedSignal" -PassThru)
    }
    foreach ($bot in $bots) {
        $remaining = [Math]::Max(0, [int]($stopDeadline - [DateTime]::UtcNow).TotalMilliseconds)
        if (-not $bot.WaitForExit($remaining)) {
            Write-Host "Ctrl+C was unavailable, unsafe, or did not stop $($bot.ProcessName) within 30 seconds; forcing only PID $($bot.Id)." -ForegroundColor Yellow
            Stop-Process -InputObject $bot -Force -ErrorAction Stop
            if (-not $bot.WaitForExit(5000)) {
                throw "The old bot (PID $($bot.Id)) did not exit. No replacement was launched."
            }
        }
    }
    foreach ($launcher in $launchers) {
        $current = Get-CimInstance Win32_Process -Filter "ProcessId = $($launcher.ProcessId)"
        if ($current -and $current.CreationDate -eq $launcher.CreationDate -and $current.CommandLine -eq $launcher.CommandLine) {
            Stop-Process -Id $launcher.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }

    Write-Host "Starting Tradewinds $Version. HTTP_ADDR=$httpAddr"
    Write-Host "Run directory: $RunDir"
    Write-Host "Console copy: $logPath"
    $launchedAt = [DateTime]::UtcNow
    [void](Start-Process -FilePath explorer.exe -ArgumentList "`"$shortcutPath`"" -PassThru)

    $readyDeadline = [DateTime]::UtcNow.AddSeconds(60)
    $lastFailure = 'The readiness and warnings endpoints did not become available.'
    while ([DateTime]::UtcNow -lt $readyDeadline) {
        try {
            $ready = Invoke-WebRequest -Uri "$baseUrl/readyz" -UseBasicParsing -TimeoutSec 2
            if ([int]$ready.StatusCode -eq 200) {
                $running = @(Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($binary)) -ErrorAction SilentlyContinue | Where-Object {
                    $_.Path -and [IO.Path]::GetFullPath($_.Path).Equals($binary, [StringComparison]::OrdinalIgnoreCase) -and $_.StartTime.ToUniversalTime() -ge $launchedAt
                })
                if ($running.Count -eq 0) {
                    $lastFailure = 'No newly launched process has the selected executable path.'
                } else {
                    $report = $null
                    $legacyEndpoint = $false
                    try {
                        $report = Invoke-RestMethod -Uri "$baseUrl/api/warnings" -TimeoutSec 2
                    } catch {
                        $legacyEndpoint = $null -ne $_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq 404
                        if (-not $isRollback -or ($hasVcsIdentity -and -not $legacyEndpoint)) {
                            throw
                        }
                    }
                    $reportedRevision = [string]$report.build.revision
                    $verified = $reportedRevision.StartsWith($revisionPrefix, [StringComparison]::OrdinalIgnoreCase) -and $hasVcsIdentity -and $report.build.modified -eq $buildModified
                    $unverifiable = $isRollback -and ($legacyEndpoint -or -not $hasVcsIdentity -or ($report -and -not $reportedRevision))
                    if ($reportedRevision -and -not $reportedRevision.StartsWith($revisionPrefix, [StringComparison]::OrdinalIgnoreCase)) {
                        $unverifiable = $false
                    }
                    if (($verified -or $unverifiable) -and [DateTime]::UtcNow -le $readyDeadline) {
                        $activeCodes = 'unavailable'
                        if ($report) {
                            $codes = @($report.accounts | ForEach-Object { $_.warnings } | ForEach-Object { $_.code } | Sort-Object -Unique)
                            $activeCodes = 'none'
                            if ($codes.Count -gt 0) {
                                $activeCodes = $codes -join ', '
                            }
                        }
                        if ($verified) {
                            Write-Host "Ready: build $reportedRevision. Active warning codes: $activeCodes"
                        } else {
                            Write-Host "Running, revision unverifiable: readiness and executable path match $binary. Active warning codes: $activeCodes" -ForegroundColor Yellow
                        }
                        $unassessed = @($report.accounts | Where-Object { $_.available -eq $false -or ($_.suppressed -and $_.suppressed.Count -gt 0) })
                        if ($unassessed.Count -gt 0) {
                            Write-Host 'Some account conditions are unavailable or suppressed; absence of active codes is not an all-clear.' -ForegroundColor Yellow
                        }
                        exit 0
                    }
                    $lastFailure = 'The warnings endpoint did not confirm the selected binary identity.'
                }
            }
        } catch {
            $lastFailure = 'The readiness or warnings request failed.'
        }
        Start-Sleep -Milliseconds 500
    }
    throw "Restart was not confirmed within 60 seconds. $lastFailure Inspect the log, fix the cause, then restart or roll back with -Version."
} catch {
    Write-Host "Restart failed: $($_.Exception.Message)" -ForegroundColor Red
    if ($stagedBinary -and (Test-Path -LiteralPath $stagedBinary -PathType Leaf)) {
        if ($shutdownStarted) {
            Write-Host "Verified replacement retained at $stagedBinary" -ForegroundColor Yellow
        } else {
            Remove-Item -LiteralPath $stagedBinary -Force
        }
    }
    if ($shutdownStarted -and $relaunchCommand) {
        Write-Host 'After checking current bot processes, relaunch the verified replacement with:' -ForegroundColor Yellow
        Write-Host $relaunchCommand
    }
    if ($logPath -and (Test-Path -LiteralPath $logPath -PathType Leaf)) {
        Write-Host "Protected log: $logPath"
        Write-Host 'Structured Zap entries from the last 30 log lines:'
        $withheld = $false
        foreach ($entry in (Get-Content -LiteralPath $logPath -Tail 30)) {
            $plain = [regex]::Replace($entry, '\x1b\[[0-9;]*m', '')
            if ($plain -match '[\x00-\x08\x0b-\x1f\x7f]' -or $plain -notmatch '^\[[0-9]{2}:[0-9]{2}:[0-9]{2}\](?: \[[^\]\r\n]+\]){1,2} \[(?:DEBUG|INFO|WARN|ERROR|DPANIC|PANIC|FATAL)\] [^\r\n\t]*\t(?<context>\{.*\})$') {
                $withheld = $true
                continue
            }
            try {
                [void]($Matches['context'] | ConvertFrom-Json -ErrorAction Stop)
                Write-Host $plain
            } catch {
                $withheld = $true
            }
        }
        if ($withheld) {
            Write-Host 'Nonstructured output was withheld because it may contain credentials. Inspect the protected log locally.'
        }
    } else {
        Write-Host 'No log is available for the requested version.'
    }
    exit 1
}
