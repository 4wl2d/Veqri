#requires -Version 5.1

Set-StrictMode -Version Latest

$script:LocalAdministratorsSid = "S-1-5-32-544"
$script:BuiltInServiceSids = @("S-1-5-18", "S-1-5-19", "S-1-5-20")

function Assert-VeqriDedicatedAccountName {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]
    [string]$Account
  )

  $trimmed = $Account.Trim()
  if ([string]::IsNullOrWhiteSpace($trimmed)) {
    throw "The Veqri service account name is empty."
  }

  $normalized = $trimmed.ToLowerInvariant()
  $builtInAccounts = @(
    "localsystem",
    ".\localsystem",
    "nt authority\system",
    "nt authority\localsystem",
    "system",
    ".\system",
    "localservice",
    ".\localservice",
    "nt authority\localservice",
    "nt authority\local service",
    "networkservice",
    ".\networkservice",
    "nt authority\networkservice",
    "nt authority\network service"
  )
  if (($builtInAccounts -contains $normalized) -or $normalized.StartsWith("nt service\")) {
    throw "Built-in and virtual service identities are not acceptable. Use a dedicated non-administrator user account."
  }
  return $trimmed
}

function Resolve-VeqriLogonName {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]
    [string]$Account
  )

  $separator = $Account.IndexOf("\")
  if ($separator -ge 0) {
    if (($separator -eq 0) -or ($separator -eq ($Account.Length - 1)) -or
        ($separator -ne $Account.LastIndexOf("\"))) {
      throw "The service account must use DOMAIN\user, .\user, user@domain, or a local user name."
    }
    $domain = $Account.Substring(0, $separator)
    $user = $Account.Substring($separator + 1)
    if ($domain -eq ".") {
      if ([string]::IsNullOrWhiteSpace($env:COMPUTERNAME)) {
        throw "The local computer name is unavailable for service-account verification."
      }
      $domain = $env:COMPUTERNAME
    }
    return [pscustomobject]@{ User = $user; Domain = $domain }
  }

  if ($Account.Contains("@")) {
    return [pscustomobject]@{ User = $Account; Domain = $null }
  }
  if ([string]::IsNullOrWhiteSpace($env:COMPUTERNAME)) {
    throw "Use an explicit DOMAIN\user account because the local computer name is unavailable."
  }
  return [pscustomobject]@{ User = $Account; Domain = $env:COMPUTERNAME }
}

function Initialize-VeqriWindowsAccountNativeApi {
  [CmdletBinding()]
  param()

  if ($env:OS -ne "Windows_NT") {
    throw "Windows service-token verification is unavailable on this operating system."
  }
  if ($null -ne ("Veqri.Windows.NativeAccountMethods" -as [type])) {
    return
  }

  Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

namespace Veqri.Windows
{
    public static class NativeAccountMethods
    {
        [DllImport("advapi32.dll", EntryPoint = "LogonUserW", CharSet = CharSet.Unicode, SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        public static extern bool LogonUser(
            string userName,
            string domain,
            IntPtr password,
            int logonType,
            int logonProvider,
            out IntPtr token);

        [DllImport("kernel32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        public static extern bool CloseHandle(IntPtr handle);
    }
}
"@ -ErrorAction Stop
}

function Get-VeqriServiceAccountTokenAssessment {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]
    [System.Management.Automation.PSCredential]$ServiceCredential
  )

  Initialize-VeqriWindowsAccountNativeApi
  $account = Assert-VeqriDedicatedAccountName -Account $ServiceCredential.UserName
  $logonName = Resolve-VeqriLogonName -Account $account
  $passwordPointer = [IntPtr]::Zero
  $token = [IntPtr]::Zero
  $identity = $null

  try {
    $passwordPointer = [Runtime.InteropServices.Marshal]::SecureStringToGlobalAllocUnicode($ServiceCredential.Password)
    # LOGON32_LOGON_SERVICE (5) both verifies the supplied credential/service
    # logon right and asks LSA to build the effective authorization token. The
    # token group set includes nested domain/local authorization memberships.
    $loggedOn = [Veqri.Windows.NativeAccountMethods]::LogonUser(
      $logonName.User,
      $logonName.Domain,
      $passwordPointer,
      5,
      0,
      [ref]$token
    )
    if (-not $loggedOn) {
      $errorCode = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
      throw "LogonUserW(LOGON32_LOGON_SERVICE) failed with Win32 error $errorCode. Verify the credential and grant 'Log on as a service' before installation."
    }
    if ($token -eq [IntPtr]::Zero) {
      throw "Windows returned an empty service logon token."
    }

    $identity = [System.Security.Principal.WindowsIdentity]::new($token)
    if (($null -eq $identity.User) -or [string]::IsNullOrWhiteSpace($identity.Name)) {
      throw "Windows did not resolve the service account identity."
    }
    if ($null -eq $identity.Groups) {
      throw "Windows did not return authorization groups for the service account."
    }

    $groupSids = @()
    foreach ($group in $identity.Groups) {
      if (($null -eq $group) -or [string]::IsNullOrWhiteSpace($group.Value)) {
        throw "Windows returned an incomplete service-account authorization group."
      }
      $groupSids += $group.Value
    }
    if ($groupSids.Count -eq 0) {
      throw "Windows returned no authorization groups for the service account."
    }

    $windowsPrincipal = [Security.Principal.WindowsPrincipal]::new($identity)
    $isAdministrator = $windowsPrincipal.IsInRole(
      [Security.Principal.WindowsBuiltInRole]::Administrator
    ) -or ($groupSids -contains $script:LocalAdministratorsSid)

    return [pscustomobject]@{
      CanonicalName = $identity.Name
      AccountSid = $identity.User.Value
      GroupSids = [string[]]$groupSids
      IsAdministrator = [bool]$isAdministrator
    }
  }
  finally {
    if ($null -ne $identity) {
      $identity.Dispose()
    }
    if ($token -ne [IntPtr]::Zero) {
      [void][Veqri.Windows.NativeAccountMethods]::CloseHandle($token)
    }
    if ($passwordPointer -ne [IntPtr]::Zero) {
      [Runtime.InteropServices.Marshal]::ZeroFreeGlobalAllocUnicode($passwordPointer)
    }
  }
}

function Assert-VeqriDedicatedNonAdminServiceAccount {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]
    [System.Management.Automation.PSCredential]$ServiceCredential,

    # This seam is for cross-platform policy tests. The production installer
    # never supplies it and therefore always uses the native Windows token.
    [scriptblock]$TokenInspector
  )

  $requestedAccount = Assert-VeqriDedicatedAccountName -Account $ServiceCredential.UserName
  if ($null -eq $TokenInspector) {
    $TokenInspector = {
      param($credential)
      Get-VeqriServiceAccountTokenAssessment -ServiceCredential $credential
    }
  }

  try {
    $assessmentResults = @(& $TokenInspector $ServiceCredential)
  }
  catch {
    throw "Unable to prove that '$requestedAccount' is a dedicated non-administrator service account. Installation is denied: $($_.Exception.Message)"
  }
  if ($assessmentResults.Count -ne 1 -or $null -eq $assessmentResults[0]) {
    throw "Service-account verification returned no single authoritative token assessment. Installation is denied."
  }
  $assessment = $assessmentResults[0]
  $requiredProperties = @("CanonicalName", "AccountSid", "GroupSids", "IsAdministrator")
  foreach ($property in $requiredProperties) {
    if (-not ($assessment.PSObject.Properties.Name -contains $property)) {
      throw "Service-account verification omitted $property. Installation is denied."
    }
  }
  if ([string]::IsNullOrWhiteSpace([string]$assessment.CanonicalName) -or
      [string]::IsNullOrWhiteSpace([string]$assessment.AccountSid)) {
    throw "Service-account verification returned an incomplete identity. Installation is denied."
  }
  $accountSid = [string]$assessment.AccountSid
  if ($accountSid -notmatch "^S-[0-9]+(-[0-9]+)+$") {
    throw "Service-account verification returned an invalid account SID. Installation is denied."
  }
  if ($assessment.IsAdministrator -isnot [bool]) {
    throw "Service-account administrator membership was not authoritative. Installation is denied."
  }
  if ($null -eq $assessment.GroupSids) {
    throw "Service-account authorization groups were unavailable. Installation is denied."
  }

  $groupSids = @($assessment.GroupSids)
  if ($groupSids.Count -eq 0) {
    throw "Service-account authorization groups were empty. Installation is denied."
  }
  foreach ($groupSid in $groupSids) {
    if ([string]::IsNullOrWhiteSpace([string]$groupSid) -or
        ([string]$groupSid -notmatch "^S-[0-9]+(-[0-9]+)+$")) {
      throw "Service-account authorization groups were incomplete. Installation is denied."
    }
  }
  if (($script:BuiltInServiceSids -contains $accountSid) -or
      ($accountSid -match "-(500|501)$") -or
      [bool]$assessment.IsAdministrator -or
      ($groupSids -contains $script:LocalAdministratorsSid)) {
    throw "The resolved identity '$($assessment.CanonicalName)' is built-in or belongs to local Administrators. Use a dedicated non-administrator account."
  }

  return [pscustomobject]@{
    RequestedName = $requestedAccount
    CanonicalName = [string]$assessment.CanonicalName
    AccountSid = $accountSid
    IsAdministrator = $false
  }
}

Export-ModuleMember -Function Assert-VeqriDedicatedNonAdminServiceAccount
