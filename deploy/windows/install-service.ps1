#requires -Version 5.1

[CmdletBinding(SupportsShouldProcess = $true)]
param(
  [Parameter(Mandatory = $true)]
  [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
  [string]$BinaryPath,

  [Parameter(Mandatory = $true)]
  [ValidateNotNull()]
  [System.Management.Automation.PSCredential]$ServiceCredential,

  [Parameter(Mandatory = $true)]
  [ValidateNotNullOrEmpty()]
  [string]$Workspace,

  [string]$DataDir = "$env:ProgramData\Veqri"
)

$ErrorActionPreference = "Stop"

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Run this script from an elevated PowerShell session."
}

$account = $ServiceCredential.UserName.Trim()
$forbiddenAccounts = @(
  "localsystem",
  ".\localsystem",
  "nt authority\localsystem",
  "nt authority\system",
  "system",
  ".\system"
)
if ($forbiddenAccounts -contains $account.ToLowerInvariant()) {
  throw "LocalSystem is not an acceptable Veqri service identity. Use a dedicated non-administrator account."
}

$accountPolicyModule = Join-Path -Path $PSScriptRoot -ChildPath "ServiceAccountPolicy.psm1"
if (-not (Test-Path -LiteralPath $accountPolicyModule -PathType Leaf)) {
  throw "Service-account policy module is unavailable. Installation is denied."
}
try {
  # Module import changes only this PowerShell process and still occurs before
  # ShouldProcess, so -WhatIf performs the real fail-closed preflight.
  Import-Module -Name $accountPolicyModule -Force -ErrorAction Stop
  $verifiedIdentity = Assert-VeqriDedicatedNonAdminServiceAccount -ServiceCredential $ServiceCredential
}
catch {
  throw "Service-account verification failed closed: $($_.Exception.Message)"
}
if ([string]::IsNullOrWhiteSpace([string]$verifiedIdentity.CanonicalName) -or
    [string]::IsNullOrWhiteSpace([string]$verifiedIdentity.AccountSid)) {
  throw "Service-account verification did not return a canonical identity and SID. Installation is denied."
}
$accountAclIdentity = "*$($verifiedIdentity.AccountSid)"

if (Get-Service -Name "VeqriCore" -ErrorAction SilentlyContinue) {
  throw "The VeqriCore service already exists. Remove or update it explicitly instead of overwriting it."
}
if (-not [System.IO.Path]::IsPathRooted($Workspace)) {
  throw "Workspace must be an absolute path."
}
if (-not [System.IO.Path]::IsPathRooted($DataDir)) {
  throw "DataDir must be an absolute path."
}

$resolvedBinary = (Resolve-Path -LiteralPath $BinaryPath).Path
$workspacePath = [System.IO.Path]::GetFullPath($Workspace)
$dataDirPath = [System.IO.Path]::GetFullPath($DataDir)

function Get-VeqriDataTreeEntries {
  param([Parameter(Mandatory = $true)][System.IO.DirectoryInfo]$Root)

  $entries = [System.Collections.Generic.List[System.IO.FileSystemInfo]]::new()
  $pending = [System.Collections.Generic.Queue[System.IO.DirectoryInfo]]::new()
  $pending.Enqueue($Root)
  while ($pending.Count -gt 0) {
    $directory = $pending.Dequeue()
    foreach ($entry in @(Get-ChildItem -LiteralPath $directory.FullName -Force -ErrorAction Stop)) {
      if (($entry.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "DataDir contains a reparse point and cannot be hardened safely: $($entry.FullName)"
      }
      $entries.Add($entry)
      if ($entry.PSIsContainer) {
        $pending.Enqueue([System.IO.DirectoryInfo]$entry)
      }
    }
  }
  return $entries.ToArray()
}

function Assert-VeqriPathAncestorsNotReparse {
  param([Parameter(Mandatory = $true)][string]$Path)

  $probePath = [System.IO.Path]::GetFullPath($Path)
  while (-not (Test-Path -LiteralPath $probePath)) {
    $parent = [System.IO.Directory]::GetParent($probePath)
    if ($null -eq $parent) {
      throw "No existing ancestor is available for DataDir preflight."
    }
    $probePath = $parent.FullName
  }
  $probe = Get-Item -LiteralPath $probePath -Force -ErrorAction Stop
  while ($null -ne $probe) {
    if (($probe.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
      throw "DataDir or one of its existing ancestors is a reparse point: $($probe.FullName)"
    }
    $probe = $probe.Parent
  }
}

$null = Assert-VeqriPathAncestorsNotReparse -Path $dataDirPath
$binaryCommand = '"' + $resolvedBinary + '"'
if ($PSCmdlet.ShouldProcess("VeqriCore", "Create Windows service as $account")) {
  New-Item -ItemType Directory -Force -Path $workspacePath | Out-Null
  New-Item -ItemType Directory -Force -Path $dataDirPath | Out-Null

  # The data directory contains the admin token, device credentials, audit
  # records, and backups. Refuse reparse points so recursive ACL repair can
  # never escape DataDir, replace (not merely add to) the root DACL, reset
  # every existing descendant to inherit it, then verify the effective SID
  # set before creating the service.
  $dataRoot = Get-Item -LiteralPath $dataDirPath -Force -ErrorAction Stop
  $ancestor = $dataRoot
  while ($null -ne $ancestor) {
    if (($ancestor.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
      throw "DataDir or one of its ancestors is a reparse point and cannot be hardened safely: $($ancestor.FullName)"
    }
    $ancestor = $ancestor.Parent
  }
  $existingDataEntries = @(Get-VeqriDataTreeEntries -Root $dataRoot)

  $serviceSid = [System.Security.Principal.SecurityIdentifier]::new($verifiedIdentity.AccountSid)
  $systemSid = [System.Security.Principal.SecurityIdentifier]::new("S-1-5-18")
  $administratorsSid = [System.Security.Principal.SecurityIdentifier]::new("S-1-5-32-544")
  $allowedDataSids = @($serviceSid.Value, $systemSid.Value, $administratorsSid.Value)
  $expectedDataRights = @{}
  $expectedDataRights[$serviceSid.Value] = [System.Security.AccessControl.FileSystemRights]::Modify
  $expectedDataRights[$systemSid.Value] = [System.Security.AccessControl.FileSystemRights]::FullControl
  $expectedDataRights[$administratorsSid.Value] = [System.Security.AccessControl.FileSystemRights]::FullControl
  $inheritanceFlags = [System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor `
    [System.Security.AccessControl.InheritanceFlags]::ObjectInherit
  $propagationFlags = [System.Security.AccessControl.PropagationFlags]::None
  $allowType = [System.Security.AccessControl.AccessControlType]::Allow

  & icacls.exe $dataDirPath /setowner "*S-1-5-32-544" /T /L /Q | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "Could not set local Administrators as owner of the Veqri data directory tree."
  }

  $dataAcl = Get-Acl -LiteralPath $dataDirPath -ErrorAction Stop
  $dataAcl.SetAccessRuleProtection($true, $false)
  foreach ($existingRule in @($dataAcl.Access)) {
    [void]$dataAcl.RemoveAccessRuleSpecific($existingRule)
  }
  [void]$dataAcl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new(
    $serviceSid,
    [System.Security.AccessControl.FileSystemRights]::Modify,
    $inheritanceFlags,
    $propagationFlags,
    $allowType
  ))
  foreach ($fullControlSid in @($systemSid, $administratorsSid)) {
    [void]$dataAcl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new(
      $fullControlSid,
      [System.Security.AccessControl.FileSystemRights]::FullControl,
      $inheritanceFlags,
      $propagationFlags,
      $allowType
    ))
  }
  Set-Acl -LiteralPath $dataDirPath -AclObject $dataAcl -ErrorAction Stop

  if ($existingDataEntries.Count -gt 0) {
    & icacls.exe "$dataDirPath\*" /reset /T /L /Q | Out-Null
    if ($LASTEXITCODE -ne 0) {
      throw "Could not reset existing Veqri data files to the restricted inherited ACL."
    }
  }

  $verifiedRoot = Get-Item -LiteralPath $dataDirPath -Force -ErrorAction Stop
  $verifiedPaths = @($verifiedRoot) + @(Get-VeqriDataTreeEntries -Root $verifiedRoot)
  foreach ($verifiedPath in $verifiedPaths) {
    $verifiedAcl = Get-Acl -LiteralPath $verifiedPath.FullName -ErrorAction Stop
    $verifiedRules = @($verifiedAcl.GetAccessRules(
      $true,
      $true,
      [System.Security.Principal.SecurityIdentifier]
    ))
    $verifiedOwnerSid = $verifiedAcl.GetOwner(
      [System.Security.Principal.SecurityIdentifier]
    ).Value
    if ($verifiedOwnerSid -ne $administratorsSid.Value) {
      throw "Restricted DataDir owner verification failed for $($verifiedPath.FullName)."
    }
    $unexpectedRule = $verifiedRules | Where-Object {
      ($allowedDataSids -notcontains $_.IdentityReference.Value) -or
      ($_.AccessControlType -ne $allowType)
    } | Select-Object -First 1
    if ($null -ne $unexpectedRule) {
      throw "Restricted DataDir ACL verification found an unexpected access rule on $($verifiedPath.FullName)."
    }
    foreach ($expectedSid in $allowedDataSids) {
      $matchingRules = @($verifiedRules | Where-Object {
        $_.IdentityReference.Value -eq $expectedSid
      })
      if ($matchingRules.Count -ne 1 -or
          $matchingRules[0].FileSystemRights -ne $expectedDataRights[$expectedSid]) {
        throw "Restricted DataDir rights verification failed for SID $expectedSid on $($verifiedPath.FullName)."
      }
    }
    if ($verifiedPath.FullName -eq $dataDirPath) {
      if (-not $verifiedAcl.AreAccessRulesProtected -or $verifiedRules.Count -ne 3 -or
          ($verifiedRules | Where-Object { $_.IsInherited }).Count -ne 0 -or
          ($verifiedRules | Where-Object {
            $_.InheritanceFlags -ne $inheritanceFlags -or
            $_.PropagationFlags -ne $propagationFlags
          }).Count -ne 0) {
        throw "Restricted DataDir root ACL verification failed."
      }
    }
    elseif ($verifiedAcl.AreAccessRulesProtected -or
            ($verifiedRules | Where-Object { -not $_.IsInherited }).Count -ne 0) {
      throw "Restricted DataDir child ACL verification failed for $($verifiedPath.FullName)."
    }
  }

  & icacls.exe $workspacePath /grant:r "${accountAclIdentity}:(OI)(CI)M" | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "Could not grant the service account Modify access to $workspacePath."
  }

  New-Service `
    -Name "VeqriCore" `
    -BinaryPathName $binaryCommand `
    -Credential $ServiceCredential `
    -DisplayName "Veqri Core" `
    -Description "Local-first Veqri orchestration daemon" `
    -StartupType Automatic | Out-Null

  $serviceEnvironment = @(
    "VEQRI_ADDR=127.0.0.1:7342",
    "VEQRI_DATA_DIR=$dataDirPath",
    "VEQRI_DATABASE=$dataDirPath\veqri.db",
    "VEQRI_WORKSPACES=$workspacePath"
  )
  New-ItemProperty `
    -Path "HKLM:\SYSTEM\CurrentControlSet\Services\VeqriCore" `
    -Name "Environment" `
    -PropertyType MultiString `
    -Value $serviceEnvironment `
    -Force | Out-Null

  Write-Host "Created VeqriCore as $account (verified as $($verifiedIdentity.CanonicalName)). Workspace: $workspacePath"
  Write-Host "Review the service account rights, then start it with: Start-Service VeqriCore"
}
