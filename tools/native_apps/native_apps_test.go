package native_apps

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"testing"

	coretools "github.com/veqri/veqri/core/tools"
)

type recordingRunner struct {
	binary string
	args   []string
	result CommandResult
	err    error
}

func (r *recordingRunner) Run(_ context.Context, binary string, args []string, _ int) (CommandResult, error) {
	r.binary = binary
	r.args = append([]string(nil), args...)
	return r.result, r.err
}

func lookup(values map[string]string) LookupFunc {
	return func(name string) (string, error) {
		if value := values[name]; value != "" {
			return value, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestMacOSUsesOpenWithStructuredArguments(t *testing.T) {
	runner := &recordingRunner{result: CommandResult{Stdout: "ok"}}
	executor, err := NewWithConfig(Config{
		Platform: "darwin", Lookup: lookup(map[string]string{"open": "/usr/bin/open", "shortcuts": "/usr/bin/shortcuts"}), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"operation":"launch_application","application_id":"Example App","arguments":["--profile","work"]}`)
	encoded, err := executor.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.binary != "/usr/bin/open" || !reflect.DeepEqual(runner.args, []string{"-a", "Example App", "--args", "--profile", "work"}) {
		t.Fatalf("command = %q %#v", runner.binary, runner.args)
	}
	var output Output
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Backend != "macos.open" || output.Stdout != "ok" {
		t.Fatalf("output = %#v", output)
	}
	definition := executor.Definition()
	if definition.Risk != coretools.RiskStateChanging || !definition.ApprovalRequired {
		t.Fatalf("definition = %#v", definition)
	}
}

func TestMacOSShortcutUsesOfficialCLI(t *testing.T) {
	runner := &recordingRunner{}
	executor, err := NewWithConfig(Config{
		Platform: "darwin", Lookup: lookup(map[string]string{"open": "/usr/bin/open", "shortcuts": "/usr/bin/shortcuts"}), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), json.RawMessage(`{"operation":"run_shortcut","shortcut_name":"Morning routine"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.binary != "/usr/bin/shortcuts" || !reflect.DeepEqual(runner.args, []string{"run", "Morning routine"}) {
		t.Fatalf("command = %q %#v", runner.binary, runner.args)
	}
}

func TestLinuxDetectsGTKAndDBusAndRejectsMacOnlyFeature(t *testing.T) {
	runner := &recordingRunner{}
	executor, err := NewWithConfig(Config{
		Platform: "linux", Lookup: lookup(map[string]string{"gtk-launch": "/usr/bin/gtk-launch", "gdbus": "/usr/bin/gdbus"}), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	features := executor.Features()
	if !features.LinuxGTKLaunch || !features.LinuxDBus || !features.ApplicationLaunch {
		t.Fatalf("features = %#v", features)
	}
	if _, err := executor.Execute(context.Background(), json.RawMessage(`{"operation":"launch_application","application_id":"org.example.App"}`), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.args, []string{"--", "org.example.App"}) {
		t.Fatalf("gtk-launch args = %#v", runner.args)
	}
	_, err = executor.Execute(context.Background(), json.RawMessage(`{"operation":"run_shortcut","shortcut_name":"x"}`), nil)
	if !errors.Is(err, ErrUnsupportedFeature) {
		t.Fatalf("shortcut error = %v", err)
	}
}

func TestWindowsUsesAppsFolderActivation(t *testing.T) {
	runner := &recordingRunner{}
	executor, err := NewWithConfig(Config{
		Platform: "windows", Lookup: lookup(map[string]string{"explorer.exe": `C:\Windows\explorer.exe`}), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	appID := "Microsoft.WindowsCalculator_8wekyb3d8bbwe!App"
	_, err = executor.Execute(context.Background(), json.RawMessage(`{"operation":"launch_application","application_id":"`+appID+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.binary != `C:\Windows\explorer.exe` || !reflect.DeepEqual(runner.args, []string{`shell:AppsFolder\` + appID}) {
		t.Fatalf("command = %q %#v", runner.binary, runner.args)
	}
}

func TestMissingPlatformFeatureReturnsExplicitUnsupportedError(t *testing.T) {
	executor, err := NewWithConfig(Config{Platform: "linux", Lookup: lookup(nil), Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), json.RawMessage(`{"operation":"launch_application","application_id":"org.example.App"}`), nil)
	if !errors.Is(err, ErrUnsupportedFeature) {
		t.Fatalf("Execute error = %v", err)
	}
}
