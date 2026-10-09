package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func TestParameterAssociation(t *testing.T) {
	for _, test := range []struct {
		configuration string
		parameters    string
	}{
		{"web.yaml", ""},
		{"web.json", ""},
		{"web.yaml", "web.parameters.yaml"},
		{"web.yaml", "web.parameters.json"},
		{"web.json", "web.parameters.yaml"},
		{"web.json", "web.parameters.json"},
		{"web.dsc.yaml", "web.dsc.parameters.json"},
	} {
		t.Run(test.configuration+"/"+test.parameters, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, test.configuration)
			input(t, dir, "orphan.parameters.yaml")
			input(t, dir, "orphan.parameters.json")
			if test.parameters != "" {
				input(t, dir, test.parameters)
			}
			found, err := discover(dir)
			if err != nil || len(found) != 1 {
				t.Fatalf("discovery = %+v (%v)", found, err)
			}
			want := dsc.Input{Configuration: filepath.Join(dir, test.configuration)}
			if test.parameters != "" {
				want.Parameters = filepath.Join(dir, test.parameters)
			}
			if found[0].input != want || found[0].err != nil {
				t.Fatalf("input = %+v, want %+v", found[0], want)
			}
			var calls []dsc.Input
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls = append(calls, in)
				return dsc.Result{Configuration: filepath.Base(in.Configuration), Outcome: "succeeded"}
			}}
			writer := &fakeWriter{}
			if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			want.Operation = dsc.OperationSet
			want.ConfigurationText = `{"resources":[]}`
			if want.Parameters != "" {
				want.ParametersText = `{"resources":[]}`
			}
			if !reflect.DeepEqual(calls, []dsc.Input{want}) || len(writer.results) != 1 || writer.results[0].InputHash == "" {
				t.Fatalf("calls = %+v, results = %+v", calls, writer.results)
			}
		})
	}
}

func TestConfigurationNames(t *testing.T) {
	for name, eligible := range map[string]bool{
		"base.yaml": true, "base.json": true, "base.dsc.yaml": true,
		"base.parameters.yaml": false, "base.parameters.json": false,
		"base.dscd.json": true, "base.dscd.yaml": true, "base.dscd.yml": false,
		"base.DSCD.json": true, "base.dscd.json.tmp": false,
		"base.parameters.backup.yaml": true, "base.PARAMETERS.yaml": true,
		"base.yml": false, "base.parameters.yml": false, ".hidden.yaml": false,
		"base.parameters.yaml.tmp": false, "base.YAML": false,
	} {
		if got := configurationName(name); got != eligible {
			t.Errorf("configurationName(%q) = %t, want %t", name, got, eligible)
		}
	}
	dir := t.TempDir()
	input(t, dir, "orphan.parameters.yaml")
	input(t, dir, "orphan.parameters.json")
	found, err := discover(dir)
	if err != nil || len(found) != 0 {
		t.Fatalf("sidecar-only directory: %+v (%v)", found, err)
	}
}

func TestAmbiguousInputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		files      []string
		failures   int
		diagnostic string
	}{
		{"two parameter formats", []string{"web.yaml", "web.parameters.yaml", "web.parameters.json"}, 1, "multiple parameter files"},
		{"two configurations with parameters", []string{"web.yaml", "web.json", "web.parameters.yaml"}, 2, "multiple configuration files"},
		{"two configurations without parameters", []string{"web.yaml", "web.json"}, 2, "multiple configuration files"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range test.files {
				input(t, dir, name)
			}
			// Unrelated documents still execute even when other inputs are ambiguous.
			input(t, dir, "z-unrelated.yaml")
			found, err := discover(dir)
			if err != nil || len(found) != test.failures+1 {
				t.Fatalf("discovery = %+v (%v)", found, err)
			}
			for _, item := range found[:test.failures] {
				if item.err == nil || !strings.Contains(item.err.Error(), test.diagnostic) {
					t.Fatalf("missing discovery error: %+v", item)
				}
				for _, name := range test.files {
					if !strings.HasSuffix(name, ".parameters.yaml") || test.failures == 1 {
						if !strings.Contains(item.err.Error(), name) {
							t.Errorf("error %q missing filename %q", item.err, name)
						}
					}
				}
			}
			var calls []string
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				name := filepath.Base(in.Configuration)
				calls = append(calls, name)
				return dsc.Result{Configuration: name, Outcome: "succeeded"}
			}}
			writer := &fakeWriter{}
			if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"z-unrelated.yaml"}) || len(writer.results) != test.failures+1 {
				t.Fatalf("calls=%v, results=%+v", calls, writer.results)
			}
			for _, result := range writer.results[:test.failures] {
				if result.Outcome != "failed" || result.Error == nil || result.Error.Kind != "input" ||
					result.InputHash != "" || result.ExitCode != nil {
					t.Fatalf("ambiguous input executed or misclassified: %+v", result)
				}
			}
		})
	}
}

func TestParameterDisappearsBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"10-first.yaml", "20-web.yaml", "20-web.parameters.json", "30-last.json"} {
		input(t, dir, name)
	}
	var calls []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		name := filepath.Base(in.Configuration)
		calls = append(calls, name)
		if name == "10-first.yaml" {
			if err := os.Remove(filepath.Join(dir, "20-web.parameters.json")); err != nil {
				t.Error(err)
			}
		}
		return dsc.Result{Configuration: name, Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"10-first.yaml", "30-last.json"}) || len(writer.results) != 3 {
		t.Fatalf("calls=%v results=%+v", calls, writer.results)
	}
	result := writer.results[1]
	if result.Error == nil || result.Error.Kind != "input" || result.Parameters != "20-web.parameters.json" ||
		!strings.Contains(result.Error.Message, "20-web.parameters.json") {
		t.Fatalf("lost missing-parameter failure: %+v", result)
	}
}

func TestNonregularParameterIsNotIgnored(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "web.yaml")
			path := filepath.Join(dir, "web.parameters.yaml")
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(filepath.Join(dir, "web.yaml"), path); err != nil {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
			}
			client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
				t.Fatal("executed configuration despite unusable parameter file")
				return dsc.Result{}
			}}
			writer := &fakeWriter{}
			if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(writer.results) != 1 || writer.results[0].Error == nil ||
				!strings.Contains(writer.results[0].Error.Message, "not a regular file") {
				t.Fatalf("missing parameter error: %+v", writer.results)
			}
		})
	}
}
