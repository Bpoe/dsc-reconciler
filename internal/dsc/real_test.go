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
		{".yaml", "parameters:\n  message: Hello from parameters\n", "Hello from parameters"},
		{".json", `{"parameters":{"message":"Hello from parameters"}}`, "Hello from parameters"},
	}
	for _, document := range documents {
		for _, parameter := range parameters {
			t.Run(document.ext+"/parameters"+parameter.ext, func(t *testing.T) {
				dir := t.TempDir()
				in := Input{Configuration: filepath.Join(dir, "echo with spaces"+document.ext)}
				if err := os.WriteFile(in.Configuration, []byte(document.content), 0600); err != nil {
					t.Fatal(err)
				}
				if parameter.ext != "" {
					in.Parameters = filepath.Join(dir, "echo with spaces.parameters"+parameter.ext)
					if err := os.WriteFile(in.Parameters, []byte(parameter.content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				result := client.Execute(context.Background(), in)
				if result.Outcome != "succeeded" {
					t.Fatalf("real DSC failed: %+v; stderr: %s", result.Error, result.Stderr)
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
