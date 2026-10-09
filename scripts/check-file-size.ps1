#!/usr/bin/env pwsh
# PowerShell twin of scripts/check-file-size.sh for hosts without WSL/bash.
# Keeps the same ceiling, EXEMPT list and exit semantics.
$ErrorActionPreference = 'Stop'
$ceiling = 700
$exempt = @(
  'internal/qoder/client.go',
  'internal/grok/native_chat.go',
  'internal/workbuddy/auth.go',
  'internal/handler/handler_helpers.go',
  'internal/qoder/stream.go',
  'internal/loadbalancer/loadbalancer.go',
  'internal/grok/handler.go',
  'internal/api/api_ops.go',
  'internal/grok/quality_hold.go',
  'internal/opsagg/opsagg.go'
)
function Get-LineCount([string]$p) {
  $b = [System.IO.File]::ReadAllBytes($p)
  return ($b | Where-Object { $_ -eq 10 }).Count
}
$status = 0
foreach ($p in (git ls-files '*.go')) {
  if ($p -like '*_test.go') { continue }
  if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { continue }
  $n = Get-LineCount $p
  if ($n -le $ceiling) { continue }
  if ($exempt -contains $p) { continue }
  "$p : $n lines (ceiling $ceiling)"
  $status = 1
}
foreach ($p in $exempt) {
  if (-not (Test-Path -LiteralPath $p)) { "$p : exempted but missing"; $status = 1; continue }
  $n = Get-LineCount $p
  if ($n -le $ceiling) { "$p : $n lines, no longer needs an exemption - remove it"; $status = 1 }
}
if ($status -eq 0) { "every non-test Go file is at or under $ceiling lines" }
exit $status
