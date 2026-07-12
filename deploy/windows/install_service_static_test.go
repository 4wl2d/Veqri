package windows_test

import (
	"os"
	"strings"
	"testing"
)

func readDeploymentSource(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func requireSourceFragments(t testing.TB, source string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(source, fragment) {
			t.Errorf("deployment source omitted %q", fragment)
		}
	}
}

func TestWindowsInstallerVerifiesAccountBeforeEveryMutation(t *testing.T) {
	installer := readDeploymentSource(t, "install-service.ps1")
	requireSourceFragments(t, installer,
		`"localsystem"`,
		`"nt authority\system"`,
		`LocalSystem is not an acceptable Veqri service identity`,
		`Import-Module -Name $accountPolicyModule -Force -ErrorAction Stop`,
		`Assert-VeqriDedicatedNonAdminServiceAccount -ServiceCredential $ServiceCredential`,
		`Service-account verification failed closed`,
		`$accountAclIdentity = "*$($verifiedIdentity.AccountSid)"`,
		`"${accountAclIdentity}:(OI)(CI)M"`,
	)

	verification := strings.Index(installer, "Assert-VeqriDedicatedNonAdminServiceAccount")
	if verification < 0 {
		t.Fatal("service-account verification call is missing")
	}
	for _, mutation := range []string{
		`$PSCmdlet.ShouldProcess`,
		`New-Item -ItemType Directory`,
		`icacls.exe`,
		`New-Service`,
		`New-ItemProperty`,
	} {
		position := strings.Index(installer, mutation)
		if position < 0 {
			t.Errorf("installer mutation marker %q is missing", mutation)
		} else if verification > position {
			t.Errorf("account verification occurs after mutation marker %q", mutation)
		}
	}
	if strings.Contains(installer, "-TokenInspector") {
		t.Fatal("production installer exposes the test token-inspector seam")
	}
}

func TestWindowsAccountPolicyUsesEffectiveServiceTokenAndFailsClosed(t *testing.T) {
	policy := readDeploymentSource(t, "ServiceAccountPolicy.psm1")
	requireSourceFragments(t, policy,
		`EntryPoint = "LogonUserW"`,
		`LOGON32_LOGON_SERVICE (5)`,
		`SecureStringToGlobalAllocUnicode`,
		`ZeroFreeGlobalAllocUnicode`,
		`System.Security.Principal.WindowsIdentity`,
		`$identity.Groups`,
		`WindowsBuiltInRole]::Administrator`,
		`S-1-5-32-544`,
		`S-1-5-18`,
		`Service-account authorization groups were unavailable. Installation is denied.`,
		`Unable to prove that`,
	)

	logon := strings.Index(policy, "NativeAccountMethods]::LogonUser")
	groupEnumeration := strings.Index(policy, "foreach ($group in $identity.Groups)")
	administratorDecision := strings.Index(policy, "$isAdministrator =")
	if logon < 0 || groupEnumeration < logon || administratorDecision < groupEnumeration {
		t.Fatal("native logon, nested token-group enumeration, and administrator decision are not ordered safely")
	}
}

func TestWindowsInstallerReplacesAndVerifiesDataDirectoryACL(t *testing.T) {
	installer := readDeploymentSource(t, "install-service.ps1")
	requireSourceFragments(t, installer,
		`FileAttributes]::ReparsePoint`,
		`$ancestor = $dataRoot`,
		`$ancestor = $ancestor.Parent`,
		`Get-VeqriDataTreeEntries -Root $dataRoot`,
		`$pending.Enqueue([System.IO.DirectoryInfo]$entry)`,
		`Assert-VeqriPathAncestorsNotReparse -Path $dataDirPath`,
		`/setowner "*S-1-5-32-544" /T /L /Q`,
		`$dataAcl.SetAccessRuleProtection($true, $false)`,
		`$dataAcl.RemoveAccessRuleSpecific($existingRule)`,
		`Set-Acl -LiteralPath $dataDirPath`,
		`/reset /T /L /Q`,
		`$verifiedAcl.GetAccessRules(`,
		`$verifiedAcl.GetOwner(`,
		`$verifiedOwnerSid -ne $administratorsSid.Value`,
		`$allowedDataSids -notcontains $_.IdentityReference.Value`,
		`$expectedDataRights[$serviceSid.Value] =`,
		`$matchingRules.Count -ne 1`,
		`$matchingRules[0].FileSystemRights -ne $expectedDataRights[$expectedSid]`,
		`$verifiedRules.Count -ne 3`,
		`$_.InheritanceFlags -ne $inheritanceFlags`,
		`$_.PropagationFlags -ne $propagationFlags`,
		`Where-Object { -not $_.IsInherited }`,
	)

	if strings.Contains(installer, `$dataDirPath /inheritance:r /grant:r`) {
		t.Fatal("installer still uses additive root grants that preserve unexpected explicit ACEs")
	}
	preflight := strings.Index(installer, `Assert-VeqriPathAncestorsNotReparse -Path $dataDirPath`)
	createDataDir := strings.Index(installer, `New-Item -ItemType Directory -Force -Path $dataDirPath`)
	if preflight < 0 || createDataDir < 0 || preflight > createDataDir {
		t.Fatal("DataDir ancestor reparse preflight must run before directory creation")
	}
}

func TestPowerShellPolicySuiteCoversNestedMembershipAndUnavailableVerification(t *testing.T) {
	tests := readDeploymentSource(t, "tests/ServiceAccountPolicy.Tests.ps1")
	requireSourceFragments(t, tests,
		`preserves explicit LocalSystem rejection`,
		`rejects nested administrator membership`,
		`S-1-5-32-544`,
		`fails closed when token verification is unavailable`,
		`fails closed when authorization groups are incomplete`,
		`accepts one complete non-administrator token assessment`,
	)
}
