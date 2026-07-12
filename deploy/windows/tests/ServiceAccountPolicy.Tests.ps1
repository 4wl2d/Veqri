#requires -Version 5.1

BeforeAll {
  Import-Module "$PSScriptRoot/../ServiceAccountPolicy.psm1" -Force

  function New-TestCredential {
    param([string]$Name)
    $password = ConvertTo-SecureString "not-a-production-password" -AsPlainText -Force
    return [System.Management.Automation.PSCredential]::new($Name, $password)
  }

  function New-SafeAssessment {
    return [pscustomobject]@{
      CanonicalName = "TESTHOST\veqri-service"
      AccountSid = "S-1-5-21-1000-1000-1000-1100"
      GroupSids = [string[]]@("S-1-1-0", "S-1-5-32-545")
      IsAdministrator = $false
    }
  }
}

Describe "Veqri Windows service-account policy" {
  It "preserves explicit LocalSystem rejection before token inspection" {
    $script:InspectorCalled = $false
    $inspector = {
      param($credential)
      $script:InspectorCalled = $true
      New-SafeAssessment
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential "NT AUTHORITY\SYSTEM") `
        -TokenInspector $inspector
    } | Should -Throw "*Built-in*"
    $script:InspectorCalled | Should -BeFalse
  }

  It "rejects a directly detected administrator token" {
    $inspector = {
      param($credential)
      $result = New-SafeAssessment
      $result.IsAdministrator = $true
      return $result
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential ".\veqri-service") `
        -TokenInspector $inspector
    } | Should -Throw "*local Administrators*"
  }

  It "rejects nested administrator membership found in authorization group SIDs" {
    $inspector = {
      param($credential)
      $result = New-SafeAssessment
      # IsInRole can be false for filtered tokens. The raw token group list
      # must still reject a nested local-Administrators SID.
      $result.IsAdministrator = $false
      $result.GroupSids = [string[]]@("S-1-1-0", "S-1-5-32-544")
      return $result
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential "DOMAIN\veqri-service") `
        -TokenInspector $inspector
    } | Should -Throw "*local Administrators*"
  }

  It "rejects a built-in service SID even when the supplied name is disguised" {
    $inspector = {
      param($credential)
      $result = New-SafeAssessment
      $result.AccountSid = "S-1-5-18"
      return $result
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential "DOMAIN\alias") `
        -TokenInspector $inspector
    } | Should -Throw "*built-in*"
  }

  It "rejects a renamed built-in Administrator account by RID" {
    $inspector = {
      param($credential)
      $result = New-SafeAssessment
      $result.AccountSid = "S-1-5-21-1000-1000-1000-500"
      return $result
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential "DOMAIN\renamed-account") `
        -TokenInspector $inspector
    } | Should -Throw "*built-in*"
  }

  It "fails closed when token verification is unavailable" {
    $inspector = {
      param($credential)
      throw "authorization API unavailable"
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential ".\veqri-service") `
        -TokenInspector $inspector
    } | Should -Throw "*Installation is denied*"
  }

  It "fails closed when authorization groups are incomplete" {
    $inspector = {
      param($credential)
      return [pscustomobject]@{
        CanonicalName = "TESTHOST\veqri-service"
        AccountSid = "S-1-5-21-1000-1000-1000-1100"
        GroupSids = $null
        IsAdministrator = $false
      }
    }
    {
      Assert-VeqriDedicatedNonAdminServiceAccount `
        -ServiceCredential (New-TestCredential ".\veqri-service") `
        -TokenInspector $inspector
    } | Should -Throw "*Installation is denied*"
  }

  It "accepts one complete non-administrator token assessment" {
    $inspector = {
      param($credential)
      New-SafeAssessment
    }
    $result = Assert-VeqriDedicatedNonAdminServiceAccount `
      -ServiceCredential (New-TestCredential ".\veqri-service") `
      -TokenInspector $inspector

    $result.CanonicalName | Should -Be "TESTHOST\veqri-service"
    $result.IsAdministrator | Should -BeFalse
  }
}
