# Over the share mapped at the drive $Mount: a file written and read back, a
# directory made, the file listed, then both deleted.
param([string]$Mount)
$ErrorActionPreference = 'Stop'

'hello from windows' | Out-File -FilePath "$Mount\file.txt" -Encoding ascii -NoNewline
$content = Get-Content "$Mount\file.txt" -Raw
if ($content.Trim() -ne 'hello from windows') { throw "file.txt reads back '$content'" }
Write-Host 'PASS: file create and read'

New-Item -ItemType Directory -Path "$Mount\dir" | Out-Null
if (!(Test-Path "$Mount\dir" -PathType Container)) { throw 'dir is not a directory' }
Write-Host 'PASS: directory create'

if ((Get-ChildItem "$Mount\").Name -notcontains 'file.txt') { throw 'file.txt is not listed' }
Write-Host 'PASS: directory listing'

Remove-Item "$Mount\file.txt"
Remove-Item "$Mount\dir" -Recurse
if ((Test-Path "$Mount\file.txt") -or (Test-Path "$Mount\dir")) { throw 'file.txt or dir is still there' }
Write-Host 'PASS: delete'
