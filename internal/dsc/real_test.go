package dsc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This explicitly opt-in test uses only the Echo debugging resource, never host resources.
func TestRealDSCParameterFiles(t *testing.T) {
	path := os.Getenv("DSCD_TEST_DSC_PATH")
	if path == "" {
		t.Skip("set DSCD_TEST_DSC_PATH to opt in to the real DSC Echo test")
	}
	path, err := exec.LookPath(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(path, 30*time.Second)
	session, err := client.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	}()
	failed, err := session.Execute(context.Background(), Input{Configuration: "invalid.yaml", Operation: OperationSet, ConfigurationText: "not a configuration"})
	if err != nil || failed.Error == nil || failed.Error.Kind != "dsc" {
		t.Fatalf("expected an ordinary DSC error on the reusable session: %+v (%v)", failed.Error, err)
	}
	documents := []struct {
		ext, content string
	}{
		{".yaml", `$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
parameters:
  message:
    type: string
    defaultValue: Hello from dscd
resources:
  - name: Echo parameter
    type: Microsoft.DSC.Debug/Echo
    properties:
      output: "[parameters('message')]"
`},
		{".json", `{"$schema":"https://aka.ms/dsc/schemas/v3/bundled/config/document.json","parameters":{"message":{"type":"string","defaultValue":"Hello from dscd"}},"resources":[{"name":"Echo parameter","type":"Microsoft.DSC.Debug/Echo","properties":{"output":"[parameters('message')]"}}]}`},
	}
	parameters := []struct {
		ext, content, output string
	}{
		{"", "", "Hello from dscd"},
		{".yaml", "message: Hello from parameters\n", "Hello from parameters"},
		{".json", `{"message":"Hello from parameters"}`, "Hello from parameters"},
	}
	for _, document := range documents {
		for _, parameter := range parameters {
			t.Run(document.ext+"/parameters"+parameter.ext, func(t *testing.T) {
				dir := t.TempDir()
				in := Input{Configuration: filepath.Join(dir, "echo with spaces"+document.ext), Operation: OperationSet, ConfigurationText: document.content}
				if parameter.ext != "" {
					in.Parameters = filepath.Join(dir, "echo with spaces.parameters"+parameter.ext)
					in.ParametersText = parameter.content
				}
				result, err := session.Execute(context.Background(), in)
				if err != nil || result.Outcome != "succeeded" {
					t.Fatalf("real DSC failed: %+v; session: %v", result.Error, err)
				}
				if result.ExitCode != nil || result.Stderr != "" {
					t.Fatal("successful RPC synthesized per-operation process diagnostics")
				}
				var payload struct {
					Results []struct {
						Result struct {
							AfterState struct {
								Output string
							}
						}
					}
				}
				if err := json.Unmarshal(result.DSCResult, &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Results) != 1 || payload.Results[0].Result.AfterState.Output != parameter.output {
					t.Fatalf("unexpected Echo result: %s", result.DSCResult)
				}
				t.Logf("Echo returned %q", parameter.output)
			})
		}
	}
}
