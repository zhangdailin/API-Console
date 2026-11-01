param([switch]$OpenLoop,[switch]$Warmup,[switch]$RequireTarget,[ValidateSet('cline','workbuddy','grok')][string[]]$Providers=@('cline','workbuddy','grok'),[ValidateRange(1,60)][int]$Seconds=5,[ValidateRange(1,1000)][int]$GCPercent=100,[string]$MemoryLimit='512MiB')
# Measures current source. Archived overlays target the earlier source/test shape.
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$oldArch = $env:GOARCH
$oldLoad = $env:PROVIDER_LOCAL_LOAD
$oldSeconds = $env:PROVIDER_LOCAL_LOAD_SECONDS
$oldGC = $env:GOGC
$oldMemoryLimit = $env:GOMEMLIMIT
$oldWarmup = $env:PROVIDER_LOCAL_WARMUP
$oldRequireTarget = $env:PROVIDER_LOCAL_REQUIRE_TARGET
try {
 $env:GOARCH = 'amd64'
 $env:GOGC = "$GCPercent"
 $env:GOMEMLIMIT = $MemoryLimit
 $env:PROVIDER_LOCAL_LOAD_SECONDS = "$Seconds"
 if ($Warmup) { $env:PROVIDER_LOCAL_WARMUP='1' } else { Remove-Item Env:\PROVIDER_LOCAL_WARMUP -ErrorAction SilentlyContinue }
 if ($RequireTarget) { $env:PROVIDER_LOCAL_REQUIRE_TARGET='1' } else { Remove-Item Env:\PROVIDER_LOCAL_REQUIRE_TARGET -ErrorAction SilentlyContinue }
 if ($OpenLoop) { $env:PROVIDER_LOCAL_LOAD='1' } else { Remove-Item Env:\PROVIDER_LOCAL_LOAD -ErrorAction SilentlyContinue }
 Push-Location $projectRoot
 try {
  $argsList=@('test')
  $argsList += @($Providers | ForEach-Object { "./internal/$_" })
  $argsList += @('-run','^$','-bench','^BenchmarkProviderLocal$','-benchmem','-cpu=18')
  if ($OpenLoop) { $argsList += '-benchtime=1x' } else { $argsList += '-benchtime=2s' }
  & go @argsList
  if ($LASTEXITCODE -ne 0) { throw "Performance run failed: exit $LASTEXITCODE" }
 } finally { Pop-Location }
} finally {
 if ($null -eq $oldArch) { Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH=$oldArch }
 if ($null -eq $oldLoad) { Remove-Item Env:\PROVIDER_LOCAL_LOAD -ErrorAction SilentlyContinue } else { $env:PROVIDER_LOCAL_LOAD=$oldLoad }
 if ($null -eq $oldSeconds) { Remove-Item Env:\PROVIDER_LOCAL_LOAD_SECONDS -ErrorAction SilentlyContinue } else { $env:PROVIDER_LOCAL_LOAD_SECONDS=$oldSeconds }
 if ($null -eq $oldGC) { Remove-Item Env:\GOGC -ErrorAction SilentlyContinue } else { $env:GOGC=$oldGC }
 if ($null -eq $oldMemoryLimit) { Remove-Item Env:\GOMEMLIMIT -ErrorAction SilentlyContinue } else { $env:GOMEMLIMIT=$oldMemoryLimit }
 if ($null -eq $oldWarmup) { Remove-Item Env:\PROVIDER_LOCAL_WARMUP -ErrorAction SilentlyContinue } else { $env:PROVIDER_LOCAL_WARMUP=$oldWarmup }
 if ($null -eq $oldRequireTarget) { Remove-Item Env:\PROVIDER_LOCAL_REQUIRE_TARGET -ErrorAction SilentlyContinue } else { $env:PROVIDER_LOCAL_REQUIRE_TARGET=$oldRequireTarget }
}
