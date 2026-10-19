package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func TestProtectionRevalidatesDiscoveredPairing(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		oldParameters          []string
		currentParameters      []string
		oldDuplicateConfig     bool
		currentDuplicateConfig bool
		wantFailure            bool
	}{
		{name: "added sidecar", currentParameters: []string{"json"}},
		{name: "removed sidecar", oldParameters: []string{"yaml"}},
		{name: "switched format", oldParameters: []string{"yaml"}, currentParameters: []string{"json"}},
		{name: "newly ambiguous sidecars", oldParameters: []string{"yaml"}, currentParameters: []string{"yaml", "json"}, wantFailure: true},
		{name: "sidecar ambiguity removed", oldParameters: []string{"yaml", "json"}, currentParameters: []string{"json"}},
		{name: "newly ambiguous configurations", currentDuplicateConfig: true, wantFailure: true},
		{name: "configuration ambiguity removed", oldDuplicateConfig: true, currentParameters: []string{"yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const controlText = "resources: []\n"
			before := writeInput(t, dir, "10-before.yaml", controlText)
			config := writeInput(t, dir, "20-web.yaml", "metadata: {dscd: {operation: set}}\nresources: []\n")
			after := writeInput(t, dir, "30-after.yaml", controlText)
			for _, format := range tc.oldParameters {
				writeInput(t, dir, "20-web.parameters."+format, "old: parameters\n")
			}
			duplicate := filepath.Join(dir, "20-web.json")
			if tc.oldDuplicateConfig {
				writeInput(t, dir, "20-web.json", `{"resources":[]}`)
			}
			candidates, err := discover(dir)
			if err != nil {
				t.Fatal(err)
			}
			wantCandidates := 3
			if tc.oldDuplicateConfig {
				wantCandidates++
			}
			if len(candidates) != wantCandidates {
				t.Fatalf("discovered %d candidates, want %d", len(candidates), wantCandidates)
			}

			for _, format := range tc.oldParameters {
				if err := os.Remove(filepath.Join(dir, "20-web.parameters."+format)); err != nil {
					t.Fatal(err)
				}
			}

			const parameterText = `{"current":"parameters"}`
			for _, format := range tc.currentParameters {
				writeInput(t, dir, "20-web.parameters."+format, parameterText)
			}
			if tc.oldDuplicateConfig {
				if err := os.Remove(duplicate); err != nil {
					t.Fatal(err)
				}
			}
			if tc.currentDuplicateConfig {
				writeInput(t, dir, "20-web.json", `{"resources":[]}`)
			}
			const currentText = "metadata: {dscd: {operation: test}}\nresources: []\n"
			replacement := writeInput(t, dir, ".replacement", currentText)
			if err := os.Rename(replacement, config); err != nil {
				t.Fatal(err)
			}
			current := dsc.Input{
				Configuration:     config,
				ConfigurationText: currentText,
				Operation:         dsc.OperationTest,
			}
			if len(tc.currentParameters) == 1 {
				current.Parameters = filepath.Join(dir, "20-web.parameters."+tc.currentParameters[0])
				current.ParametersText = parameterText
			}
			wantInputs := []dsc.Input{
				{Configuration: before, ConfigurationText: controlText, Operation: dsc.OperationSet},
			}
			if !tc.wantFailure {
				wantInputs = append(wantInputs, current)
			}
			wantInputs = append(wantInputs, dsc.Input{
				Configuration: after, ConfigurationText: controlText, Operation: dsc.OperationSet,
			})
			var inputs []dsc.Input
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				inputs = append(inputs, in)
				return dsc.Result{Outcome: "succeeded", Operation: &in.Operation}
			}}
			writer := &fakeWriter{}
			if err := New(dir, time.Second, client.start, writer, logger()).execute(context.Background(), candidates); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inputs, wantInputs) {
				t.Fatalf("submitted inputs = %+v, want %+v", inputs, wantInputs)
			}
			results := writer.snapshot()
			if len(results) != 3 {
				t.Fatalf("published %d results, want 3: %+v", len(results), results)
			}
			for i, name := range []string{"10-before.yaml", "20-web.yaml", "30-after.yaml"} {
				if results[i].Configuration != name {
					t.Errorf("result %d configuration = %q, want %q", i, results[i].Configuration, name)
				}
			}
			result := results[1]
			if tc.wantFailure {
				if result.Error == nil || result.Error.Kind != "input" || result.Operation != nil || result.InputHash != "" {
					t.Fatalf("new ambiguity did not publish an input failure: %+v", result)
				}
			} else {
				wantParameters := ""
				if current.Parameters != "" {
					wantParameters = filepath.Base(current.Parameters)
				}
				if result.Error != nil || result.InputHash != inputHash(current) ||
					result.Parameters != wantParameters || result.Operation == nil || *result.Operation != current.Operation {
					t.Fatalf("result does not describe current pair: %+v", result)
				}
			}
			for _, result := range []dsc.Result{results[0], results[2]} {
				if result.Error != nil || result.Outcome != "succeeded" {
					t.Errorf("independent configuration failed: %+v", result)
				}
			}
		})
	}
}

func TestProtectionRevalidatesTargetedHintEligibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes inputChanges
		wantRun bool
	}{
		{name: "stale sidecar create or write", changes: inputChanges{"web.parameters.json": false}},
		{name: "explicit sidecar removal", changes: inputChanges{"web.parameters.json": true}, wantRun: true},
		{name: "configuration change", changes: inputChanges{"web.yaml": false}, wantRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const text = "metadata: {dscd: {operation: test}}\nresources: []\n"
			config := writeInput(t, dir, "web.yaml", text)
			parameters := writeInput(t, dir, "web.parameters.json", `{"old":"parameters"}`)
			candidates, err := changedCandidates(dir, tc.changes)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("targeted discovery = %v, %v", candidates, err)
			}
			if candidates[0].input.Parameters != parameters {
				t.Fatalf("initial candidate parameters = %q, want %q", candidates[0].input.Parameters, parameters)
			}
			if err := os.Remove(parameters); err != nil {
				t.Fatal(err)
			}
			wantInput := dsc.Input{
				Configuration:     config,
				ConfigurationText: text,
				Operation:         dsc.OperationTest,
			}
			starts, calls := 0, 0
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls++
				if in != wantInput {
					t.Errorf("submitted input = %+v, want current parameterless input %+v", in, wantInput)
				}
				return dsc.Result{Outcome: "succeeded", Operation: &in.Operation}
			}}
			start := func(ctx context.Context) (DSC, error) {
				starts++
				return client.start(ctx)
			}
			writer := &fakeWriter{}
			if err := New(dir, time.Second, start, writer, logger()).execute(context.Background(), candidates); err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if tc.wantRun {
				wantCalls = 1
			}
			results := writer.snapshot()
			if starts != wantCalls || calls != wantCalls || len(results) != wantCalls {
				t.Fatalf("started %d sessions, executed %d times, published %d results; want %d each",
					starts, calls, len(results), wantCalls)
			}
			if tc.wantRun {
				result := results[0]
				if result.Error != nil || result.Configuration != "web.yaml" || result.Parameters != "" ||
					result.InputHash != inputHash(wantInput) || result.Operation == nil || *result.Operation != wantInput.Operation {
					t.Fatalf("result does not describe current parameterless input: %+v", result)
				}
			}
			if err := os.Remove(config); err != nil {
				t.Fatalf("configuration protection leaked after targeted eligibility check: %v", err)
			}
		})
	}
}
