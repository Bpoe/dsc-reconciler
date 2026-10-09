package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func TestConfigurationOperation(t *testing.T) {
	for _, test := range []struct {
		name, json, yaml string
		want             dsc.Operation
	}{
		{"no metadata", `{"resources":[]}`, "resources: []\n", dsc.OperationSet},
		{"empty document object", `{}`, "{}\n", dsc.OperationSet},
		{"empty metadata", `{"metadata":{}}`, "metadata: {}\n", dsc.OperationSet},
		{"empty dscd", `{"metadata":{"dscd":{}}}`, "metadata:\n  dscd: {}\n", dsc.OperationSet},
		{"explicit set", `{"metadata":{"dscd":{"operation":"set"}}}`, "metadata:\n  dscd:\n    operation: set\n", dsc.OperationSet},
		{"explicit test", `{"metadata":{"dscd":{"operation":"test"}}}`, "metadata:\n  dscd:\n    operation: test\n", dsc.OperationTest},
		{"unknown namespace", `{"metadata":{"anotherTool":{"arbitrary":["a",1,true,null,{}]}}}`, "metadata:\n  anotherTool:\n    arbitrary: [a, 1, true, null, {}]\n", dsc.OperationSet},
		{"unknown scalar properties", `{"metadata":{"future":null,"customTool":false,"dscd":{"number":1e9999,"string":"hello","boolean":true,"null":null}}}`, "metadata:\n  future: null\n  customTool: false\n  dscd:\n    number: 1e9999\n    string: hello\n    boolean: true\n    null: null\n", dsc.OperationSet},
		{"unknown dscd property", `{"metadata":{"dscd":{"foobar":"hello","futureSetting":{"enabled":true,"values":["one","two"]}}}}`, "metadata:\n  dscd:\n    foobar: hello\n    futureSetting:\n      enabled: true\n      values: [one, two]\n", dsc.OperationSet},
		{"unknown top level", `{"future":[{},null,false,3,"text"]}`, "future: [{}, null, false, 3, text]\n", dsc.OperationSet},
		{"case sensitive metadata", `{"Metadata":{"dscd":{"operation":"test"}}}`, "Metadata:\n  dscd:\n    operation: test\n", dsc.OperationSet},
		{"case sensitive namespace", `{"metadata":{"DSCD":{"operation":"test"}}}`, "metadata:\n  DSCD:\n    operation: test\n", dsc.OperationSet},
		{"case sensitive operation", `{"metadata":{"dscd":{"Operation":"test","OPERATION":null}}}`, "metadata:\n  dscd:\n    Operation: test\n    OPERATION: null\n", dsc.OperationSet},
		{"exact operation", `{"metadata":{"dscd":{"Operation":"set","operation":"test","OPERATION":null}}}`, "metadata:\n  dscd:\n    Operation: set\n    operation: test\n    OPERATION: null\n", dsc.OperationTest},
		{"Unicode escapes in unknown properties", `{"metadata":{"dscd":{"operation":"test","future":"\ud83d\ude00"}}}`, "metadata:\n  dscd:\n    operation: test\n    future: \"\\U0001F600\"\n", dsc.OperationTest},
		{"combined unknown properties", `{
  "$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
  "metadata": {
    "Microsoft.DSC": {"securityContext": "elevated"},
    "customTool": {"arbitrary": [null, false, 42, "secret", {"nested": []}]},
    "dscd": {"operation": "test", "foobar": "hello", "futureSetting": {"enabled": true, "values": ["one", "two"]}}
  },
  "future": [null, {"anything": true}],
  "parameters": {"message": {"type": "string"}},
  "resources": [{"name": "Example", "type": "Microsoft.DSC.Debug/Echo", "properties": {"output": "[parameters('message')]"}}]
}`, `$schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
metadata:
  Microsoft.DSC:
    securityContext: elevated
  customTool:
    arbitrary: [null, false, 42, secret, {nested: []}]
  dscd:
    operation: test
    foobar: hello
    futureSetting:
      enabled: true
      values: [one, two]
future: [null, {anything: true}]
parameters:
  message:
    type: string
resources:
  - name: Example
    type: Microsoft.DSC.Debug/Echo
    properties:
      output: "[parameters('message')]"
`, dsc.OperationTest},
	} {
		for ext, text := range map[string]string{".json": test.json, ".yaml": test.yaml, ".json.yaml": test.json} {
			t.Run(test.name+ext, func(t *testing.T) {
				got, err := configurationOperation("web"+ext, text)
				if err != nil || got != test.want {
					t.Fatalf("operation = %q (%v), want %q", got, err, test.want)
				}
			})
		}
	}
}

func TestYAMLMetadataAliasesAndMerges(t *testing.T) {
	for _, text := range []string{
		"policy: &policy {operation: test}\nmetadata: {dscd: *policy}\n",
		"policy: &policy {operation: test}\nmetadata: {dscd: {<<: *policy}}\n",
		"policy: &policy {operation: set}\nmetadata: {dscd: {<<: *policy, operation: test}}\n",
		"policy: &policy {dscd: {operation: test}}\nmetadata: {<<: [*policy]}\n",
		"policy: &policy {metadata: {dscd: {operation: test}}}\n<<: *policy\n",
		"policy: &policy test\nmetadata: {dscd: {operation: *policy}}\n",
	} {
		t.Run(text, func(t *testing.T) {
			op, err := configurationOperation("web.yaml", text)
			if err != nil || op != dsc.OperationTest {
				t.Fatalf("YAML policy ignored: %q (%v)", op, err)
			}
		})
	}
}

func TestEmbeddedMetadataAndParametersSubmittedUnchanged(t *testing.T) {
	for ext, text := range map[string]string{
		".json": `{"metadata":{"Microsoft.DSC":{"securityContext":"elevated"},"dscd":{"operation":"test","future":[null,1,true,{},[]]}},"parameters":{"message":{"type":"string"}},"resources":[{"name":"Example","type":"Microsoft.DSC.Debug/Echo","properties":{"output":"[parameters('message')]"}}]}`,
		".yaml": "# Preserve comments, formatting, expressions and unrelated metadata.\nmetadata:\n  Microsoft.DSC: {securityContext: elevated}\n  dscd:\n    operation: test\n    future: [null, 1, true, {}, []]\nparameters:\n  message: {type: string}\nresources:\n  - name: Example\n    type: Microsoft.DSC.Debug/Echo\n    properties:\n      output: \"[parameters('message')]\"\n",
	} {
		for _, params := range []struct{ name, text string }{
			{"", ""}, {"web.parameters.yaml", "message: Hello\n"}, {"web.parameters.json", `{"message":"Hello"}`},
		} {
			t.Run(ext+"/"+params.name, func(t *testing.T) {
				dir := t.TempDir()
				want := dsc.Input{
					Configuration:     writeInput(t, dir, "web"+ext, text),
					ConfigurationText: text, Operation: dsc.OperationTest,
				}
				if params.name != "" {
					want.Parameters = writeInput(t, dir, params.name, params.text)
					want.ParametersText = params.text
				}
				calls := 0
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					calls++
					if in != want {
						t.Fatalf("captured input = %+v, want %+v", in, want)
					}
					return dsc.Result{Configuration: "web" + ext, Operation: &in.Operation, Outcome: "succeeded"}
				}}
				writer := &fakeWriter{}
				if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || len(writer.results) != 1 || writer.results[0].InputHash != inputHash(want) {
					t.Fatalf("calls=%d results=%+v", calls, writer.results)
				}
			})
		}
	}
}

func TestEmbeddedMetadataRediscoveryAndHashIdentity(t *testing.T) {
	for _, ext := range []string{".json", ".yaml"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			var logs bytes.Buffer
			var calls []dsc.Input
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls = append(calls, in)
				return dsc.Result{Configuration: filepath.Base(in.Configuration), Operation: &in.Operation, Outcome: "succeeded"}
			}}
			writer := &fakeWriter{}
			r := New(dir, time.Second, client.start, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
			hashes := make(map[string]string)
			for _, test := range []struct {
				json, yaml string
				op         dsc.Operation
			}{
				{`{"resources":[]}`, "resources: []\n", dsc.OperationSet},
				{`{"metadata":{"dscd":{"operation":"test"}},"resources":[]}`, "metadata:\n  dscd:\n    operation: test\nresources: []\n", dsc.OperationTest},
				{`{"metadata":{"dscd":{"operation":"set"}},"resources":[]}`, "metadata:\n  dscd:\n    operation: set\nresources: []\n", dsc.OperationSet},
				{`{ "metadata": { "dscd": { "operation": "set" } }, "resources": [] }`, "# Formatting changes are still input changes.\nmetadata: {dscd: {operation: set}}\nresources: []\n", dsc.OperationSet},
				{`{"metadata":{"dscd":{"operation":"set","future":"PRIVATE-METADATA"}},"resources":[]}`, "metadata:\n  dscd:\n    operation: set\n    future: PRIVATE-METADATA\nresources: []\n", dsc.OperationSet},
				{`{"resources":[]}`, "resources: []\n", dsc.OperationSet},
			} {
				text := test.json
				if ext == ".yaml" {
					text = test.yaml
				}
				writeInput(t, dir, "web"+ext, text)
				if err := r.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				result := writer.results[len(writer.results)-1]
				in := calls[len(calls)-1]
				if in.Operation != test.op || in.ConfigurationText != text ||
					result.Operation == nil || *result.Operation != test.op || result.InputHash != inputHash(in) {
					t.Fatalf("operation/snapshot mismatch: %+v, input=%+v", result, in)
				}
				for previous, hash := range hashes {
					if (result.InputHash == hash) != (previous == text) {
						t.Fatal("hash did not identify the exact submitted configuration bytes")
					}
				}
				hashes[text] = result.InputHash
			}
			if len(calls) != 6 || strings.Contains(logs.String(), "PRIVATE-METADATA") ||
				!strings.Contains(logs.String(), `"operation":"test"`) {
				t.Fatalf("missing calls/operation or leaked metadata: calls=%d logs=%s", len(calls), logs.String())
			}
		})
	}
}

func assertConfigurationFailure(t *testing.T, dir, name string) {
	t.Helper()
	input(t, dir, "z-last.json")
	var calls []string
	var logs bytes.Buffer
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls = append(calls, filepath.Base(in.Configuration))
		return dsc.Result{Configuration: filepath.Base(in.Configuration), Operation: &in.Operation, Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	if err := New(dir, time.Second, client.start, writer, slog.New(slog.NewJSONHandler(&logs, nil))).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"z-last.json"}) || len(writer.results) != 2 {
		t.Fatalf("invalid input executed or stopped pass: calls=%v, results=%+v", calls, writer.results)
	}
	failed := writer.results[0]
	if failed.Configuration != name || failed.Outcome != "failed" || failed.Error == nil || failed.Error.Kind != "input" ||
		(!strings.Contains(failed.Error.Message, name) && (failed.Parameters == "" || !strings.Contains(failed.Error.Message, failed.Parameters))) || failed.InputHash != "" ||
		failed.Operation != nil || failed.DSCResult != nil {
		t.Fatalf("input failure: %+v", failed)
	}
	if strings.Contains(logs.String(), "PRIVATE-METADATA") || strings.Contains(failed.Error.Message, "PRIVATE-METADATA") {
		t.Fatal("input contents leaked into diagnostics")
	}
}

func TestInvalidEmbeddedMetadataFailsOnlyConfiguration(t *testing.T) {
	for _, content := range []string{
		"", "{", `[]`, `null`, `"PRIVATE-METADATA"`, `{"resources":[]} {}`,
		`{"metadata":null}`, `{"metadata":[]}`, `{"metadata":"PRIVATE-METADATA"}`,
		`{"metadata":{"dscd":null}}`, `{"metadata":{"dscd":[]}}`, `{"metadata":{"dscd":true}}`,
		`{"metadata":{"dscd":{"operation":"apply"}}}`,
		`{"metadata":{"dscd":{"operation":"PRIVATE-METADATA"}}}`,
		`{"metadata":{"dscd":{"operation":""}}}`,
		`{"metadata":{"dscd":{"operation":null}}}`,
		`{"metadata":{"dscd":{"operation":false}}}`,
		`{"metadata":{"dscd":{"operation":1}}}`,
		`{"metadata":{"dscd":{"operation":[]}}}`,
		`{"metadata":{"dscd":{"operation":{}}}}`,
		`{"metadata":{"dscd":{"operation":"Test"}}}`,
		`{"metadata":{"dscd":{"operation":" test "}}}`,
		`{"metadata":{"dscd":{"operation":null,"Operation":"test"}}}`,
		`{"metadata":{"dscd":{"operation":"test","operation":"set"}}}`,
		`{"metadata":{"dscd":{"operation":"test"},"dscd":{}}}`,
		`{"metadata":{"dscd":{"operation":"test"}},"metadata":{}}`,
		"{\"metadata\":\"\xff\"}",
	} {
		for _, ext := range []string{".json", ".yaml"} {
			t.Run(ext+"/"+content, func(t *testing.T) {
				dir := t.TempDir()
				writeInput(t, dir, "web"+ext, content)
				assertConfigurationFailure(t, dir, "web"+ext)
			})
		}
	}
	for _, text := range []string{
		"# empty\n", "metadata: [PRIVATE-METADATA", "metadata:\n  dscd: *PRIVATE-METADATA\n",
		"metadata: {}\n---\nmetadata: {dscd: {operation: test}}\n",
		"metadata: {}\n---\n", "metadata: {}\n---\n[PRIVATE-METADATA",
		"metadata:\n  dscd:\n    operation:\n", "metadata:\n  dscd:\n    operation: true\n",
		"metadata:\n  dscd:\n    operation: 1\n", "metadata:\n  dscd:\n    operation: [test]\n",
		"metadata:\n  dscd:\n    operation: Test\n", "metadata:\n  dscd:\n    operation: ' test '\n",
		"metadata:\n  dscd:\n    operation: test\n    operation: set\n",
		"metadata:\n  dscd:\n    operation: !!binary dGVzdA==\n",
		"metadata: &loop {<<: *loop}\n",
	} {
		t.Run(text, func(t *testing.T) {
			dir := t.TempDir()
			writeInput(t, dir, "web.yaml", text)
			assertConfigurationFailure(t, dir, "web.yaml")
		})
	}
	t.Run("JSON cannot use YAML syntax", func(t *testing.T) {
		dir := t.TempDir()
		writeInput(t, dir, "web.json", "metadata: {dscd: {operation: test}}\n")
		assertConfigurationFailure(t, dir, "web.json")
	})
}

func TestFormerCompanionFilesAreOrdinaryConfigurations(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	writeInput(t, dir, "web.dscd.json", `{"operation":"test"}`)
	writeInput(t, dir, "orphan.dscd.json", `{"metadata":{"dscd":{"operation":"test"}},"resources":[]}`)
	want := []dsc.Operation{dsc.OperationTest, dsc.OperationSet, dsc.OperationSet}
	var calls []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		if in.Operation != want[len(calls)] {
			t.Fatalf("companion-file policy still applied: %+v", in)
		}
		calls = append(calls, filepath.Base(in.Configuration))
		return dsc.Result{Configuration: filepath.Base(in.Configuration), Operation: &in.Operation, Outcome: "succeeded"}
	}}
	if err := New(dir, time.Second, client.start, &fakeWriter{}, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"orphan.dscd.json", "web.dscd.json", "web.yaml"}) {
		t.Fatalf("former companion files received special treatment: %v", calls)
	}
}

func TestInvalidMetadataOnlyPassDoesNotStartServer(t *testing.T) {
	dir := t.TempDir()
	writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: apply}}\n")
	writer := &fakeWriter{}
	start := func(context.Context) (DSC, error) {
		t.Fatal("started server for invalid metadata")
		return nil, nil
	}
	if err := New(dir, time.Second, start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writer.results) != 1 || writer.results[0].Error == nil {
		t.Fatalf("missing failure result: %+v", writer.results)
	}
}

func TestEmbeddedMetadataSharesInputSizeLimit(t *testing.T) {
	dir := t.TempDir()
	const prefix = `{"metadata":{"dscd":{"operation":"test","ignored":"`
	const suffix = `"}},"resources":[]}`
	text := prefix + strings.Repeat("x", maxInputBytes-len(prefix)-len(suffix)) + suffix
	writeInput(t, dir, "web.json", text)
	calls := 0
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls++
		if in.Operation != dsc.OperationTest || in.ConfigurationText != text {
			t.Fatal("limit-sized snapshot changed")
		}
		return dsc.Result{Configuration: "web.json", Operation: &in.Operation, Outcome: "succeeded"}
	}}
	if err := New(dir, time.Second, client.start, &fakeWriter{}, logger()).Pass(context.Background()); err != nil || calls != 1 {
		t.Fatalf("exact input limit rejected: calls=%d err=%v", calls, err)
	}
	writeInput(t, dir, "web.parameters.json", " ")
	assertConfigurationFailure(t, dir, "web.json")
	if err := os.Remove(filepath.Join(dir, "web.parameters.json")); err != nil {
		t.Fatal(err)
	}
	writeInput(t, dir, "web.json", text+" ")
	assertConfigurationFailure(t, dir, "web.json")
}

func TestOperationHashWithoutParameters(t *testing.T) {
	in := dsc.Input{ConfigurationText: "opaque input", Operation: dsc.OperationSet}
	setHash := inputHash(in)
	in.Operation = dsc.OperationTest
	if inputHash(in) == setHash {
		t.Fatal("operation ignored for inputs without parameters")
	}
}

func TestMetadataStartupFailureRetainsOperation(t *testing.T) {
	dir := t.TempDir()
	writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: test}}\n")
	writer := &fakeWriter{}
	start := func(context.Context) (DSC, error) { return nil, os.ErrNotExist }
	if err := New(dir, time.Second, start, writer, logger()).Pass(context.Background()); err == nil {
		t.Fatal("missing startup error")
	}
	if len(writer.results) != 1 {
		t.Fatalf("results=%+v", writer.results)
	}
	result := writer.results[0]
	data, err := json.Marshal(result)
	if err != nil || result.Error == nil || result.Error.Kind != "start" ||
		!bytes.Contains(data, []byte(`"operation":"test"`)) || result.InputHash == "" {
		t.Fatalf("startup result: %s (%v)", data, err)
	}
}
