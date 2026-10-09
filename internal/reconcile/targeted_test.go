package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func changedNames(names ...string) map[string]struct{} {
	changed := make(map[string]struct{}, len(names))
	for _, name := range names {
		changed[name] = struct{}{}
	}
	return changed
}

func TestTargetedSelection(t *testing.T) {
	for _, test := range []struct {
		name    string
		files   []string
		changes []string
		want    []string
		fail    bool
	}{
		{"configuration", []string{"web.yaml"}, []string{"web.yaml"}, []string{"web.yaml"}, false},
		{"json configuration", []string{"web.json"}, []string{"web.json"}, []string{"web.json"}, false},
		{"yaml parameters", []string{"web.json", "web.parameters.yaml"}, []string{"web.parameters.yaml"}, []string{"web.json"}, false},
		{"json parameters", []string{"web.yaml", "web.parameters.json"}, []string{"web.parameters.json"}, []string{"web.yaml"}, false},
		{"orphan", []string{"web.parameters.json"}, []string{"web.parameters.json"}, nil, false},
		{"removed configuration", nil, []string{"web.yaml"}, nil, false},
		{"removed parameters", []string{"web.yaml"}, []string{"web.parameters.json"}, nil, false},
		{"unrelated", []string{"web.yaml", "notes.txt"}, []string{"notes.txt"}, nil, false},
		{"old companion", []string{"web.dscd.json"}, []string{"web.dscd.json"}, []string{"web.dscd.json"}, false},
		{"duplicate configurations", []string{"web.yaml", "web.json"}, []string{"web.yaml"}, []string{"web.json", "web.yaml"}, true},
		{"duplicate parameters", []string{"web.yaml", "web.parameters.json", "web.parameters.yaml"}, []string{"web.parameters.json"}, []string{"web.yaml"}, true},
		{"pair notifications", []string{"web.yaml", "web.parameters.json"}, []string{"web.parameters.json", "web.yaml"}, []string{"web.yaml"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "unrelated.yaml")
			for _, name := range test.files {
				input(t, dir, name)
			}
			var calls []string
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls = append(calls, filepath.Base(in.Configuration))
				return dsc.InputFailure(in, errors.New("controlled DSC failure"))
			}}
			writer := &fakeWriter{}
			r := New(dir, time.Second, client.start, writer, logger())
			if err := r.changed(context.Background(), changedNames(test.changes...)); err != nil {
				t.Fatal(err)
			}
			var published []string
			for _, result := range writer.results {
				published = append(published, result.Configuration)
				if test.fail && (result.InputHash != "" || result.Error == nil || result.Error.Kind != "input") {
					t.Fatalf("invalid input result: %+v", result)
				}
			}
			if !reflect.DeepEqual(published, test.want) {
				t.Fatalf("published %v, want %v", published, test.want)
			}
			if test.fail {
				if len(calls) != 0 {
					t.Fatalf("executed ambiguous input: %v", calls)
				}
			} else if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("executed %v, want %v", calls, test.want)
			}
		})
	}
}

func TestTargetedParameterFirstAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "unrelated.yaml")
	parameter := writeInput(t, dir, "web.parameters.json", `{"message":"ready"}`)
	var calls []dsc.Input
	starts, closes := 0, 0
	writer := &fakeWriter{}
	r := New(dir, time.Second, func(context.Context) (DSC, error) {
		starts++
		return &fakeSession{
			execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
				calls = append(calls, in)
				return dsc.InputFailure(in, errors.New("controlled failure")), nil
			},
			close: func() error { closes++; return nil },
		}, nil
	}, writer, logger())
	for range 2 {
		if err := r.changed(context.Background(), changedNames("web.parameters.json")); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 0 || len(writer.results) != 0 {
		t.Fatal("orphan parameter triggered reconciliation")
	}
	config := writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: test}}\nresources: []\n")
	if err := r.changed(context.Background(), changedNames("web.yaml")); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || starts != 1 || closes != 1 || calls[0].Configuration != config ||
		calls[0].Parameters != parameter || calls[0].ParametersText != `{"message":"ready"}` ||
		calls[0].Operation != dsc.OperationTest || writer.results[0].InputHash != inputHash(calls[0]) {
		t.Fatalf("calls=%+v starts=%d closes=%d results=%+v", calls, starts, closes, writer.results)
	}
}

func TestTargetedNonregularInputs(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.json")
	if err := os.Mkdir(filepath.Join(dir, "web.yaml"), 0700); err != nil {
		t.Fatal(err)
	}
	candidates, err := changedCandidates(dir, changedNames("web.yaml"))
	if err != nil || len(candidates) != 0 {
		t.Fatalf("configuration directory triggered work: %+v, %v", candidates, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "web.parameters.yaml"), 0700); err != nil {
		t.Fatal(err)
	}
	writer := &fakeWriter{}
	client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
		t.Fatal("executed with nonregular sidecar")
		return dsc.Result{}
	}}
	if err := New(dir, time.Second, client.start, writer, logger()).changed(context.Background(), changedNames("web.parameters.yaml")); err != nil {
		t.Fatal(err)
	}
	if len(writer.results) != 1 || writer.results[0].Error == nil || writer.results[0].Error.Kind != "input" {
		t.Fatalf("missing sidecar failure: %+v", writer.results)
	}
}
