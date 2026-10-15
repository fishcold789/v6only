# A single HTTPS origin outage must not take the entire network offline.
$ErrorActionPreference='Stop'
$ast=[Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'v6only.ps1'),[ref]$null,[ref]$null)
$fn=$ast.Find({$args[0] -is [Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Test-PhysicalForwarding'},$false)
. ([scriptblock]::Create($fn.Extent.Text))
$CoreDir=$PSScriptRoot;$Marker=Join-Path ([IO.Path]::GetTempPath()) ('v6only-probe-'+[guid]::NewGuid()+'/active.flag')
New-Item -ItemType Directory (Split-Path $Marker)|Out-Null
function Write-Log($message){}
function Invoke-BoundedProcess($file,$arguments,$output,$errorOutput,$timeout){
 $script:Attempts+=,$arguments
 if($timeout -ne 12000){throw 'Missing bounded endpoint timeout'}
 if($script:TimeoutFirst -and $script:Attempts.Count -eq 1){throw 'Simulated timeout'}
 if($script:AllFail -or ($script:FailFirst -and $script:Attempts.Count -eq 1)){return 1}
 return 0
}
try {
 foreach($case in @('first-success','second-success','first-timeout','all-fail','fallback')){
  $script:Attempts=@();$script:FailFirst=$case -in @('second-success','fallback');$script:TimeoutFirst=$case -eq 'first-timeout';$script:AllFail=$case -eq 'all-fail'
  if($case -eq 'fallback'){New-Item -ItemType File (Join-Path (Split-Path $Marker) 'allow-ipv4-fallback.flag')|Out-Null}
  $failed=$false;try{Test-PhysicalForwarding @{Name='WLAN'} @('202.112.128.50') '2400:3200::1'}catch{$failed=$true}
  if($failed -ne ($case -eq 'all-fail')){throw "Incorrect availability result: $case"}
  $expected=if($case -eq 'first-success'){1}else{2}
  if($Attempts.Count -ne $expected){throw "Incorrect attempt count: $case"}
  foreach($args in $Attempts){if(($args -match '--allow-ipv4-fallback') -ne ($case -eq 'fallback')){throw 'Probe silently changed routing policy'}}
  if($Attempts.Count -eq 2 -and $Attempts[0] -eq $Attempts[1]){throw 'Origins are not independent'}
 }
 Write-Host 'PASS: primary success, secondary success, timeout recovery, all-origins failure and unchanged fallback policy'
}finally{Remove-Item (Split-Path $Marker) -Recurse -Force}
