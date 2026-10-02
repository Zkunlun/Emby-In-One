# Phase 7 static checks only. Run from this repository root.
# No go test/build/vet/race is invoked; regex inventory supplements source review.
$ErrorActionPreference = 'Stop'
$all = @(git ls-files --cached --others --exclude-standard -- '*.go' | Sort-Object -Unique)
if ($LASTEXITCODE -ne 0) { throw 'git Go manifest failed' }
$prod = @($all | Where-Object { $_ -notlike '*_test.go' })
$symbols = @('recordSessionToWatchStore','heartbeatPlaybackLease','stopPlaybackLease','finalizeStoppedPlayback','forwardNoContent')
$rows = @()
foreach ($p in $prod) {
  $lines = [System.IO.File]::ReadAllLines((Join-Path (Get-Location) $p))
  for ($i=0; $i -lt $lines.Length; $i++) {
    if ($lines[$i] -match '^\s*(//|func\s)') { continue }
    foreach ($symbol in $symbols) {
      if ($lines[$i] -match ('\.'+[regex]::Escape($symbol)+'\s*\(')) {
        $rows += [pscustomobject]@{symbol=$symbol;path=$p;line=$i+1;text=$lines[$i].Trim()}
      }
    }
  }
}
$expected = @{recordSessionToWatchStore=4;heartbeatPlaybackLease=2;stopPlaybackLease=1;finalizeStoppedPlayback=3;forwardNoContent=7}
foreach ($symbol in $symbols) {
  $count = @($rows | Where-Object { $_.symbol -eq $symbol }).Count
  if ($count -ne $expected[$symbol]) { throw "Unexpected $symbol callsites: $count" }
}
$direct = @()
foreach ($p in $prod) {
  Select-String -Path $p -Pattern 'WatchStore\.RecordProgress\(|PlaybackLimiter\.(Heartbeat|Stop)\(|\.CountForServer\(' | ForEach-Object {
    $direct += [pscustomobject]@{path=$p;line=$_.LineNumber;text=$_.Line.Trim()}
  }
}
if (@($direct | Where-Object { $_.path -notin @('internal/backend/session_userdata.go','internal/backend/media_stream.go') }).Count -ne 0 -or $direct.Count -ne 4 -or @($direct | Where-Object { $_.text.Contains('.CountForServer(') }).Count -ne 0) { throw 'Unexpected direct local-state mutation/count callsite' }
$changed = @(git diff --name-only -- '*.go') + @(git ls-files --others --exclude-standard -- '*.go')
$changed = @($changed | Sort-Object -Unique)
$format = @(& gofmt -l @changed 2>&1)
if ($LASTEXITCODE -ne 0 -or $format.Count -ne 0) { throw ('gofmt parse/format failed: '+($format -join '; ')) }
$white = @(git diff --check 2>&1)
if ($LASTEXITCODE -ne 0 -or $white.Count -ne 0) { throw ('tracked whitespace failed: '+($white -join '; ')) }
$untracked = @(git ls-files --others --exclude-standard)
foreach ($p in $untracked) {
  $result = @(git -c core.whitespace=blank-at-eol,blank-at-eof,space-before-tab diff --no-index --check -- /dev/null $p 2>&1)
  $exitCode = $LASTEXITCODE
  if ($exitCode -notin @(0,1) -or $result.Count -ne 0) { throw ("untracked whitespace failed: "+$p+" "+($result -join '; ')) }
}
$phase = @($all | Where-Object { $_ -match '/phase7.*_test\.go$' })
$tests = @()
foreach ($p in $phase) {
  $text = [System.IO.File]::ReadAllText((Join-Path (Get-Location) $p))
  foreach ($m in [regex]::Matches($text, '(?m)^func (Test\w+)\(')) { $tests += [pscustomobject]@{path=$p;name=$m.Groups[1].Value} }
}
$duplicates = @($tests | Group-Object name | Where-Object Count -gt 1)
if ($duplicates.Count -ne 0) { throw 'Duplicate Phase7 test function names' }
$source = Get-Content -Raw internal/backend/session_userdata.go
$stopped = [regex]::Match($source,'(?ms)^func \(a \*App\) handleSessionPlayingStopped\(.*?^}').Value
if (!$stopped -or $stopped -match 'writeSessionUpstream(Error|Unavailable)') { throw 'Stopped uses fail-closed session writer' }
$forward = [regex]::Match($source,'(?ms)^func \(a \*App\) forwardNoContent\(.*?^}').Value
if ($forward -match 'ReadAll|readUpstreamJSONOrNoContent|Unmarshal') { throw 'No-content helper reads/parses upstream body' }
$references = @{}
foreach ($p in $phase) {
  $text = [System.IO.File]::ReadAllText((Join-Path (Get-Location) $p))
  foreach ($m in [regex]::Matches($text,'\b(phase[1-7][A-Z]\w*)\s*\(')) { $references[$m.Groups[1].Value] = $true }
}
$allText = ($all | ForEach-Object { [System.IO.File]::ReadAllText((Join-Path (Get-Location) $_)) }) -join "\n"
$missing = @($references.Keys | Where-Object { $allText -notmatch ('(?m)^(func|type)\s+'+[regex]::Escape($_)+'\b') })
if ($missing.Count -ne 0) { throw ('Missing helper declarations: '+($missing -join ', ')) }
[pscustomobject]@{result='PASS';go_manifest=$all.Count;production_files=$prod.Count;gofmt_changed_files=$changed.Count;untracked_whitespace_files=$untracked.Count;phase7_test_files=$phase.Count;phase7_test_functions=$tests.Count;helper_declarations_checked=$references.Count;callsites=$rows;direct_calls=$direct;phase7_manifest=$phase;checks=@('gofmt parse and formatting','tracked diff whitespace','all untracked whitespace','expected mechanical callsite counts','direct state mutation/count inventory','Stopped writer boundary','no-content body not consumed','Phase7 helper declarations','unique Phase7 test names');go_formal_verification='NOT RUN'} | ConvertTo-Json -Depth 6 -Compress

exit 0
