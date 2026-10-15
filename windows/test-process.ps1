# Exercise actual short-lived processes and redirected output on PS 5.1 and 7.
$ErrorActionPreference='Stop'
$ast=[System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'v6only.ps1'),[ref]$null,[ref]$null)
$fn=$ast.Find({$args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Invoke-BoundedProcess'},$false)
. ([scriptblock]::Create($fn.Extent.Text))
$temp=Join-Path ([IO.Path]::GetTempPath()) ('v6only-process-'+[guid]::NewGuid())
New-Item -ItemType Directory $temp|Out-Null
$exe=Join-Path $PSHOME 'powershell.exe'
if(!(Test-Path $exe)){$exe=Join-Path $PSHOME 'pwsh'}
$out=Join-Path $temp 'out';$err=Join-Path $temp 'err'
try {
 for($i=0;$i -lt 3;$i++){
  $code=Invoke-BoundedProcess $exe '-NoProfile -Command "[Console]::WriteLine(123);exit 0"' $out $err
  if($null -eq $code -or $code -ne 0 -or (Get-Content $out -Raw).Trim() -ne '123'){throw 'Fast successful process was misclassified'}
 }
 $code=Invoke-BoundedProcess $exe '-NoProfile -Command "[Console]::Error.WriteLine(456);exit 7"' $out $err
 if($code -ne 7 -or (Get-Content $err -Raw).Trim() -ne '456'){throw 'Nonzero exit or stderr was lost'}
 $failed=$false
 try{Invoke-BoundedProcess $exe '-NoProfile -Command "Start-Sleep 10"' $out $err 200|Out-Null}catch{$failed=$_.Exception.Message -match 'timed out'}
 if(!$failed){throw 'Process deadline was not enforced'}
 Write-Host 'PASS: short-lived success, exit status, stderr, and bounded process timeout'
} finally {Remove-Item $temp -Recurse -Force}
