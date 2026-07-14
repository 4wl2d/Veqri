package integration_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAndroidGeneratedProtocolTypesStayInsideJSONCodec(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	androidSources := filepath.Join(repositoryRoot, "apps", "android", "app", "src", "main", "java")
	codecPath := filepath.Join(
		androidSources, "com", "veqri", "android", "network", "DeviceJsonCodec.kt",
	)

	seenCodecImport := false
	err := filepath.WalkDir(androidSources, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".kt" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(contents), "ai.veqri.protocol.v1") {
			return nil
		}
		if path != codecPath {
			t.Errorf("generated protocol type escaped the handwritten codec boundary: %s", path)
			return nil
		}
		seenCodecImport = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenCodecImport {
		t.Fatal("Android JSON codec does not use generated protocol messages")
	}
}
