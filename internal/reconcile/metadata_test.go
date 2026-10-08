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

func writeMetadata(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "web.dscd.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMetadataAssociation(t *testing.T) {
	for _, configuration := range []string{"web.yaml", "web.json"} {
		for _, parameters := range []string{"", "web.parameters.yaml", "web.parameters.json"} {
			t.Run(configuration+"/"+parameters, func(t *testing.T) {
				dir := t.TempDir()
				input(t, dir, configuration)
				if parameters != "" {
					input(t, dir, parameters)
				}
				path := writeMetadata(t, dir, `{"operation":"test"}`)
				input(t, dir, "orphan.dscd.json")
				found, err := discover(dir)
				if err != nil || len(found) != 1 || found[0].err != nil || found[0].metadataPath != path {
					t.Fatalf("metadata discovery: %+v (%v)", found, err)
				}
				want := dsc.Input{
					Configuration: filepath.Join(dir, configuration), ConfigurationText: "opaque document",
					Operation: dsc.OperationTest,
				}
				if parameters != "" {
					want.Parameters, want.ParametersText = filepath.Join(dir, parameters), "opaque document"
				}
				calls := 0
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					calls++
					if in != want {
						t.Fatalf("captured input = %+v, want %+v", in, want)
					}
					return dsc.Result{Configuration: configuration, Operation: &in.Operation, Outcome: "succeeded"}
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

func TestMetadataDefaultsAndHashIdentity(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	input(t, dir, "web.parameters.json")
	var logs bytes.Buffer
	var operations []dsc.Operation
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		operations = append(operations, in.Operation)
		return dsc.Result{Configuration: "web.yaml", Operation: &in.Operation, Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	r := New(dir, time.Second, client.start, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
	hashes := make(map[dsc.Operation]string)
	for _, test := range []struct {
		name, content string
		operation     dsc.Operation
	}{
		{"missing", "", dsc.OperationSet},
		{"empty object", `{}`, dsc.OperationSet},
		{"explicit set", `{"operation":"set"}`, dsc.OperationSet},
		{"explicit test", `{"operation":"test"}`, dsc.OperationTest},
		{"formatted test", "{\n  \"operation\": \"test\"\n}\n", dsc.OperationTest},
		{"unknown property", `{"operation":"test","future":{"secret":"PRIVATE-METADATA"}}`, dsc.OperationTest},
		{"only unknown property", `{"future":"PRIVATE-METADATA"}`, dsc.OperationSet},
		{"capitalized property", `{"Operation":"test"}`, dsc.OperationSet},
		{"uppercase property", `{"OPERATION":"test"}`, dsc.OperationSet},
		{"invalid capitalized property", `{"Operation":null}`, dsc.OperationSet},
		{"exact set before capitalized test", `{"operation":"set","Operation":"test"}`, dsc.OperationSet},
		{"exact set after capitalized test", `{"Operation":"test","operation":"set"}`, dsc.OperationSet},
		{"exact test before uppercase set", `{"operation":"test","OPERATION":"set"}`, dsc.OperationTest},
		{"exact test after uppercase set", `{"OPERATION":"set","operation":"test"}`, dsc.OperationTest},
		{"removed", "", dsc.OperationSet},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.content != "" {
				writeMetadata(t, dir, test.content)
			} else if test.name == "removed" {
				if err := os.Remove(filepath.Join(dir, "web.dscd.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := writer.results[len(writer.results)-1]
			if operations[len(operations)-1] != test.operation || result.Operation == nil || *result.Operation != test.operation {
				t.Fatalf("incorrect operation: %+v, calls=%v", result, operations)
			}
			if result.InputHash == "" {
				t.Fatal("missing hash")
			}
			if previous := hashes[test.operation]; previous != "" && result.InputHash != previous {
				t.Fatal("metadata formatting or ignored properties changed input identity")
			}
			hashes[test.operation] = result.InputHash
		})
	}
	if hashes[dsc.OperationSet] == hashes[dsc.OperationTest] {
		t.Fatal("operation does not contribute to hash")
	}
	if strings.Contains(logs.String(), "PRIVATE-METADATA") || !strings.Contains(logs.String(), `"operation":"test"`) {
		t.Fatalf("missing operation or leaked metadata in logs: %s", logs.String())
	}
}

func assertMetadataFailure(t *testing.T, dir string) {
	t.Helper()
	input(t, dir, "web.yaml")
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
		t.Fatalf("invalid metadata executed or stopped pass: calls=%v, results=%+v", calls, writer.results)
	}
	failed := writer.results[0]
	if failed.Outcome != "failed" || failed.Error == nil || failed.Error.Kind != "input" ||
		!strings.Contains(failed.Error.Message, "web.dscd.json") || failed.InputHash != "" ||
		failed.Operation != nil || failed.DSCResult != nil {
		t.Fatalf("metadata failure: %+v", failed)
	}
	if strings.Contains(logs.String(), "PRIVATE-METADATA") {
		t.Fatal("metadata contents leaked into logs")
	}
}

func TestInvalidMetadataFailsOnlyConfiguration(t *testing.T) {
	for _, content := range []string{
		"", "{", `{"operation":`, `{"operation":"test"} {}`, `[]`, `null`, `"test"`,
		`{"operation":"apply"}`, `{"operation":"PRIVATE-METADATA"}`, `{"operation":""}`,
		`{"operation":null}`, `{"operation":false}`, `{"operation":1}`, `{"operation":[]}`,
		`{"operation":{}}`, `{"operation":"Test"}`, `{"operation":" test "}`, "{\"x\":\"\xff\"}",
		`{"operation":null,"Operation":"test"}`, `{"OPERATION":"test","operation":null}`,
	} {
		t.Run(content, func(t *testing.T) {
			dir := t.TempDir()
			writeMetadata(t, dir, content)
			assertMetadataFailure(t, dir)
		})
	}
}

func TestNonregularMetadataFailsOnlyConfiguration(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "web.dscd.json")
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(dir, "target.txt")
				if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
			}
			assertMetadataFailure(t, dir)
		})
	}
}

func TestMetadataDisappearsAfterDiscovery(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a-first.yaml", "web.yaml", "z-last.yaml"} {
		input(t, dir, name)
	}
	path := writeMetadata(t, dir, `{"operation":"test"}`)
	var calls []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		name := filepath.Base(in.Configuration)
		calls = append(calls, name)
		if name == "a-first.yaml" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		return dsc.Result{Configuration: name, Operation: &in.Operation, Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"a-first.yaml", "z-last.yaml"}) || len(writer.results) != 3 ||
		writer.results[1].Error == nil || writer.results[1].Error.Kind != "input" || writer.results[1].Operation != nil {
		t.Fatalf("missing metadata fell back to set: calls=%v, results=%+v", calls, writer.results)
	}
}

func TestInvalidMetadataOnlyPassDoesNotStartServer(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	writeMetadata(t, dir, `{"operation":"apply"}`)
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

func TestMetadataSizeLimit(t *testing.T) {
	dir := t.TempDir()
	content := `{"ignored":"` + strings.Repeat("x", maxInputBytes-len(`{"ignored":""}`)) + `"}`
	path := writeMetadata(t, dir, content)
	if op, err := readMetadata(context.Background(), path); err != nil || op != dsc.OperationSet {
		t.Fatalf("exact metadata size limit rejected: %q (%v)", op, err)
	}
	writeMetadata(t, dir, content+" ")
	assertMetadataFailure(t, dir)
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
	input(t, dir, "web.yaml")
	writeMetadata(t, dir, `{"operation":"test"}`)
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
