package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/Bpoe/dsc-reconciler/internal/results"
)

func TestFileHashReadsEntireInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.parameters.json")
	data := append(bytes.Repeat([]byte("opaque input"), 10000), '!')
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum, err := fileHash(context.Background(), path)
	if err != nil || sum != sha256.Sum256(data) {
		t.Fatalf("incomplete streamed input hash: %x (%v)", sum, err)
	}
}

func TestInputHash(t *testing.T) {
	for _, test := range []struct {
		name          string
		configuration string
		parameters    string
		present       bool
		different     bool
	}{
		{"unchanged", "configuration", "parameters", true, false},
		{"only configuration changed", "changed configuration", "parameters", true, true},
		{"only parameters changed", "configuration", "changed parameters", true, true},
		{"empty parameters", "configuration", "", true, true},
		{"no parameters", "configuration", "", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "web.parameters.json")}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(in.Configuration, "configuration")
			write(in.Parameters, "parameters")
			before, err := inputHash(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			write(in.Configuration, test.configuration)
			write(in.Parameters, test.parameters)
			if !test.present {
				in.Parameters = ""
			}
			after, err := inputHash(context.Background(), in)
			if err != nil || (before != after) != test.different || len(after) != len("sha256:")+64 {
				t.Fatalf("before=%q after=%q different=%t err=%v", before, after, test.different, err)
			}
		})
	}
}

func TestInputHashSeparatesFilesAndPresence(t *testing.T) {
	dir := t.TempDir()
	in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "web.parameters.json")}
	seen := make(map[string]bool)
	for _, contents := range [][2]string{{"ab", "c"}, {"a", "bc"}, {"abc", ""}} {
		for path, value := range map[string]string{in.Configuration: contents[0], in.Parameters: contents[1]} {
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
		sum, err := inputHash(context.Background(), in)
		if err != nil || seen[sum] {
			t.Fatalf("ambiguous hash %q: %v", sum, err)
		}
		seen[sum] = true
	}
	in.Parameters = ""
	sum, err := inputHash(context.Background(), in)
	if err != nil || seen[sum] {
		t.Fatalf("absent and empty sidecars are indistinguishable: %q (%v)", sum, err)
	}
}

func TestInputHashCancellationAndReadErrors(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "missing.parameters.json")}
	sum, err := inputHash(context.Background(), in)
	if sum != "" || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing.parameters.json") {
		t.Fatalf("missing parameters: hash=%q err=%v", sum, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sum, err = inputHash(ctx, in)
	if sum != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hash=%q err=%v", sum, err)
	}
	result := dsc.InputFailure(in, err)
	if result.Outcome != "canceled" || result.Error.Kind != "canceled" {
		t.Fatalf("canceled input: %+v", result)
	}
}

func TestParameterMetadataAndRediscovery(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	writer, err := results.NewWriter(filepath.Join(t.TempDir(), "results"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	var calls []dsc.Input
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls = append(calls, in)
		parameters := ""
		if in.Parameters != "" {
			parameters = filepath.Base(in.Parameters)
		}
		return dsc.Result{SchemaVersion: 1, Configuration: filepath.Base(in.Configuration), Parameters: parameters, Outcome: "succeeded"}
	}}
	r := New(dir, time.Second, client, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
	readResult := func() dsc.Result {
		t.Helper()
		if err := r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		path, err := writer.Destination("web.yaml")
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("PRIVATE-PARAMETER-")) || strings.Contains(logs.String(), "PRIVATE-PARAMETER-") {
			t.Fatal("raw parameter contents leaked into daemon metadata or logs")
		}
		var result dsc.Result
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.InputHash == "" {
			t.Fatal("input hash was not published")
		}
		return result
	}
	without := readResult()
	path := filepath.Join(dir, "web.parameters.json")
	if err := os.WriteFile(path, []byte("PRIVATE-PARAMETER-ONE (intentionally not JSON)"), 0600); err != nil {
		t.Fatal(err)
	}
	first := readResult()
	if first.Parameters != "web.parameters.json" || first.InputHash == without.InputHash || len(calls) != 2 || calls[1].Parameters != path {
		t.Fatalf("sidecar not associated: %+v, calls=%+v", first, calls)
	}
	if err := os.WriteFile(path, []byte("PRIVATE-PARAMETER-TWO (still opaque)"), 0600); err != nil {
		t.Fatal(err)
	}
	second := readResult()
	if second.InputHash == first.InputHash || len(calls) != 3 {
		t.Fatal("parameter-only edit did not change identity and reconcile again")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	removed := readResult()
	if removed.InputHash != without.InputHash || removed.Parameters != "" || calls[3].Parameters != "" {
		t.Fatalf("removed parameter sidecar still used: %+v", removed)
	}
}
