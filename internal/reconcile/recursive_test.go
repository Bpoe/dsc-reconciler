package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/Bpoe/dsc-reconciler/internal/results"
)

func candidateNames(items []candidate) []string {
	var names []string
	for _, item := range items {
		names = append(names, item.name)
	}
	return names
}

func TestRecursiveDiscoveryOrdering(t *testing.T) {
	dir := t.TempDir()
	want := []string{
		"20-security/10-baseline.yaml", "15-local.json", "10-managed/20-baseline.yaml",
		"managed/networking/25-dns.json", "30-local.yaml",
		"b/a-b.yaml", "a/a.yaml", "a/team/baseline.yaml", "b/team/baseline.yaml", "baseline.yaml",
		"visible.d/config.json",
	}
	for _, name := range append(append([]string{}, want...),
		".syncstate.json", ".staging/pending.yaml", "managed/.staging/pending.json",
		"managed/.baseline.yaml", "managed/.metadata.json",
		"managed/networking/25-dns.parameters.yaml", "other/25-dns.parameters.json",
		"managed/networking/ignored.yml", "managed/networking/ignored.YAML",
		"managed/networking/ignored.json.tmp", "orphan.parameters.yaml") {
		input(t, dir, name)
	}
	for range 5 {
		found, err := discover(dir)
		if err != nil || !reflect.DeepEqual(candidateNames(found), want) {
			t.Fatalf("discovery=%v error=%v, want %v", candidateNames(found), err, want)
		}
		for _, item := range found {
			if item.err != nil {
				t.Fatalf("cross-directory ambiguity: %v", item.err)
			}
			wantParameter := ""
			if item.name == "managed/networking/25-dns.json" {
				wantParameter = filepath.Join(dir, "managed", "networking", "25-dns.parameters.yaml")
			}
			if item.input.Parameters != wantParameter {
				t.Fatalf("%s parameters=%q, want %q", item.name, item.input.Parameters, wantParameter)
			}
		}
	}
	// Full and multi-change passes use the identical comparator, including the
	// extension (a-b.yaml sorts before a.yaml, unlike extensionless keys).
	var executed []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		name, err := relativeName(dir, in.Configuration)
		if err != nil {
			t.Fatal(err)
		}
		executed = append(executed, name)
		return dsc.Result{Outcome: "succeeded"}
	}}
	r := New(dir, time.Hour, client.start, &fakeWriter{}, logger())
	for _, run := range []func() error{
		func() error { return r.Pass(context.Background()) },
		func() error { return r.changed(context.Background(), changedNames(want...)) },
	} {
		executed = nil
		if err := run(); err != nil || !reflect.DeepEqual(executed, want) {
			t.Fatalf("execution=%v error=%v, want %v", executed, err, want)
		}
	}
}

func TestRecursiveParameterAndAmbiguityScope(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"a/web.yaml", "a/web.parameters.json",
		"b/web.json", "b/web.parameters.yaml",
		"c/web.yaml", "other/web.parameters.json",
		"duplicate/web.json", "duplicate/web.yaml",
		"parameters/web.yaml", "parameters/web.parameters.yaml", "parameters/web.parameters.json",
	} {
		input(t, dir, name)
	}
	for _, test := range []struct {
		change string
		want   []string
		fail   bool
	}{
		{"a/web.parameters.json", []string{"a/web.yaml"}, false},
		{"b/web.parameters.yaml", []string{"b/web.json"}, false},
		{"c/web.yaml", []string{"c/web.yaml"}, false},
		{"other/web.parameters.json", nil, false},
		{"duplicate/web.yaml", []string{"duplicate/web.json", "duplicate/web.yaml"}, true},
		{"parameters/web.parameters.json", []string{"parameters/web.yaml"}, true},
		{"../a/web.yaml", nil, false},
		{"/a/web.yaml", nil, false},
		{`a\web.yaml`, nil, false},
		{"a/../b/web.json", nil, false},
	} {
		t.Run(test.change, func(t *testing.T) {
			items, err := changedCandidates(dir, changedNames(test.change))
			if err != nil || !reflect.DeepEqual(candidateNames(items), test.want) {
				t.Fatalf("selection=%v error=%v, want %v", candidateNames(items), err, test.want)
			}
			for _, item := range items {
				if (item.err != nil) != test.fail {
					t.Fatalf("ambiguity=%v, want failure=%v", item.err, test.fail)
				}
				if item.name == "c/web.yaml" && item.input.Parameters != "" {
					t.Fatal("paired a sidecar from another directory")
				}
			}
		})
	}
}

func TestRecursiveResultIdentityAndRetention(t *testing.T) {
	dir := t.TempDir()
	output := t.TempDir()
	for _, name := range []string{"a/web.yaml", "b/web.yaml", "web.yaml"} {
		writeInput(t, dir, name, "metadata: {dscd: {operation: test}}\nresources: []")
	}
	writeInput(t, dir, "a/web.parameters.json", `{"message":"a"}`)
	writeInput(t, dir, "invalid/web.json", `{"metadata":{"dscd":{"operation":"invalid"}}}`)
	writer, err := results.NewWriter(output)
	if err != nil {
		t.Fatal(err)
	}
	captured := make(map[string]dsc.Input)
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		name, _ := relativeName(dir, in.Configuration)
		captured[name] = in
		return dsc.Result{
			SchemaVersion: 1, Operation: &in.Operation, Outcome: "succeeded",
			DSCResult: json.RawMessage(`{"results":[{"inDesiredState":false}],"hadErrors":false}`),
		}
	}}
	r := New(dir, time.Hour, client.start, writer, logger())
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	for _, name := range []string{"a/web.yaml", "b/web.yaml", "web.yaml", "invalid/web.json"} {
		file := filepath.Join(output, filepath.FromSlash(name+".result.json"))
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var result dsc.Result
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Configuration != name || result.SchemaVersion != 1 {
			t.Fatalf("identity not preserved: %+v", result)
		}
		if name == "invalid/web.json" {
			if _, ok := captured[name]; ok || result.Operation != nil ||
				result.Error == nil || result.Error.Kind != "input" {
				t.Fatalf("invalid input submitted or misidentified: %+v", result)
			}
			continue
		}
		if result.Operation == nil || *result.Operation != dsc.OperationTest ||
			result.Outcome != "succeeded" || result.InputHash != inputHash(captured[name]) {
			t.Fatalf("changed execution contract: %+v", result)
		}
		if name == "a/web.yaml" {
			if result.Parameters != "a/web.parameters.json" {
				t.Fatalf("parameters identity=%q", result.Parameters)
			}
			retained = data
		} else if result.Parameters != "" {
			t.Fatalf("cross-directory sidecar: %+v", result)
		}
	}
	if inputHash(captured["b/web.yaml"]) != inputHash(captured["web.yaml"]) {
		t.Fatal("paths changed the input hash")
	}
	if err := os.Remove(filepath.Join(dir, "a", "web.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(output, "a", "web.yaml.result.json"))
	if err != nil || string(after) != string(retained) {
		t.Fatalf("removed configuration lost historical result: %v", err)
	}
}

func TestRecursiveDiscoveryDoesNotFollowSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	input(t, outside, "web.yaml")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	items, err := discover(dir)
	if err != nil || len(items) != 0 {
		t.Fatalf("followed directory symlink: %+v, %v", items, err)
	}
	items, err = changedCandidates(dir, changedNames("linked/web.yaml"))
	if len(items) != 0 || err == nil {
		t.Fatalf("targeted redirected input: %+v, %v", items, err)
	}
}

func TestDirectoryRedirectedAfterDiscovery(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	input(t, dir, "10-first.yaml")
	input(t, dir, "nested/20-last.yaml")
	input(t, outside, "20-last.yaml")
	link := filepath.Join(dir, "redirect")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	calls := 0
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls++
		if err := os.Rename(filepath.Join(dir, "nested"), filepath.Join(dir, ".retired")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(link, filepath.Join(dir, "nested")); err != nil {
			t.Fatal(err)
		}
		return dsc.InputFailure(in, errors.New("controlled failure"))
	}}
	writer := &fakeWriter{}
	if err := New(dir, time.Hour, client.start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(writer.results) != 2 || writer.results[1].Configuration != "nested/20-last.yaml" ||
		writer.results[1].Error == nil || writer.results[1].Error.Kind != "input" {
		t.Fatalf("redirected ancestor executed: calls=%d results=%+v", calls, writer.results)
	}
}
