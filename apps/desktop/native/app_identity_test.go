package main

import (
	"bytes"
	"image"
	_ "image/png"
	"os"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

func TestApplicationOptionsUseCanonicalDesktopIdentityAndIcon(t *testing.T) {
	t.Parallel()

	application := newApplicationOptions(&Bridge{})
	if application.Linux == nil {
		t.Fatal("Linux options are nil")
	}
	if application.Linux.ProgramName != desktopBundleIdentifier {
		t.Fatalf("Linux ProgramName = %q, want %q", application.Linux.ProgramName, desktopBundleIdentifier)
	}
	if application.Linux.WebviewGpuPolicy != linux.WebviewGpuPolicyNever {
		t.Fatalf("Linux WebviewGpuPolicy = %v, want WebviewGpuPolicyNever", application.Linux.WebviewGpuPolicy)
	}
	if !bytes.Equal(application.Linux.Icon, appIcon) {
		t.Fatal("Linux icon does not use the canonical embedded app icon")
	}
	if application.Mac == nil || application.Mac.About == nil {
		t.Fatal("macOS About options are nil")
	}
	if !bytes.Equal(application.Mac.About.Icon, appIcon) {
		t.Fatal("macOS About icon does not use the canonical embedded app icon")
	}
}

func TestCanonicalAppIconIsPlatformReadyPNG(t *testing.T) {
	t.Parallel()

	configuration, format, err := image.DecodeConfig(bytes.NewReader(appIcon))
	if err != nil {
		t.Fatalf("decode app icon: %v", err)
	}
	if format != "png" {
		t.Fatalf("app icon format = %q, want png", format)
	}
	if configuration.Width != 1024 || configuration.Height != 1024 {
		t.Fatalf("app icon dimensions = %dx%d, want 1024x1024", configuration.Width, configuration.Height)
	}
}

func TestPlatformTemplatesUseCanonicalDesktopIdentity(t *testing.T) {
	t.Parallel()

	for _, filename := range []string{
		"build/darwin/Info.plist",
		"build/darwin/Info.dev.plist",
	} {
		contents := readIdentityAsset(t, filename)
		if !strings.Contains(contents, "<key>CFBundleIdentifier</key>\n        <string>"+desktopBundleIdentifier+"</string>") {
			t.Errorf("%s does not contain the exact desktop bundle identifier", filename)
		}
		if strings.Contains(contents, "com.wails") {
			t.Errorf("%s still contains the Wails placeholder identity", filename)
		}
		if !strings.Contains(contents, "<key>CFBundleVersion</key>\n        <string>{{.Info.ProductVersion}}</string>") {
			t.Errorf("%s does not source CFBundleVersion from numeric platform metadata", filename)
		}
	}

	windowsManifest := readIdentityAsset(t, "build/windows/wails.exe.manifest")
	if !strings.Contains(windowsManifest, `name="`+desktopBundleIdentifier+`"`) {
		t.Error("Windows manifest does not contain the exact desktop identity")
	}
	if !strings.Contains(windowsManifest, `version="{{.Info.ProductVersion}}.0"`) {
		t.Error("Windows manifest does not derive a four-part version from numeric platform metadata")
	}
	if strings.Contains(windowsManifest, "com.wails") {
		t.Error("Windows manifest still contains the Wails placeholder identity")
	}
}

func readIdentityAsset(t testing.TB, filename string) string {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return strings.ReplaceAll(string(contents), "\r\n", "\n")
}
