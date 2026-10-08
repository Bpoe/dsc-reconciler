package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/Bpoe/dsc-reconciler/internal/results"
)

func TestHashFraming(t *testing.T) {
	in := dsc.Input{Configuration: "a.yaml", Parameters: "a.parameters.json", Operation: dsc.OperationTest, ConfigurationText: "configuration", ParametersText: "parameters"}
	config := sha256.Sum256([]byte(in.ConfigurationText))
	params := sha256.Sum256([]byte(in.ParametersText))
	bytes := append([]byte("dscd-input-v2\x00"), config[:]...)
	bytes = append(bytes, 1)
	bytes = append(bytes, params[:]...)
	bytes = append(bytes, 0)
	bytes = append(bytes, []byte("test")...)
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256(bytes))
	if inputHash(in) != expected {
		t.Fatal("hash framing changed")
	}
}

func TestInputLimitsAndUTF8(t *testing.T) {
	for _, content := range [][]byte{[]byte{0xff}, []byte("more than four bytes")} {
		path := filepath.Join(t.TempDir(), "a.yaml")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readInputFile(context.Background(), path, 4); err == nil {
			t.Fatal("accepted invalid or oversized input")
		}
	}
}

func TestFileHashReadsEntireInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.parameters.json")
	data := append(bytes.Repeat([]byte("opaque input"), 10000), '!')
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	text, err := readInputFile(context.Background(), path, maxInputBytes)
	if err != nil || text != string(data) {
		t.Fatalf("incomplete input read: %v", err)
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
			in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "web.parameters.json"), Operation: dsc.OperationSet}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(in.Configuration, "configuration")
			write(in.Parameters, "parameters")
			captured, err := readInput(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			before := inputHash(captured)
			write(in.Configuration, test.configuration)
			write(in.Parameters, test.parameters)
			if !test.present {
				in.Parameters = ""
			}
			captured, err = readInput(context.Background(), in)
			after := inputHash(captured)
			if err != nil || (before != after) != test.different || len(after) != len("sha256:")+64 {
				t.Fatalf("before=%q after=%q different=%t err=%v", before, after, test.different, err)
			}
		})
	}
}

func TestInputHashSeparatesFilesAndPresence(t *testing.T) {
	dir := t.TempDir()
	in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "web.parameters.json"), Operation: dsc.OperationSet}
	seen := make(map[string]bool)
	for _, contents := range [][2]string{{"ab", "c"}, {"a", "bc"}, {"abc", ""}} {
		for path, value := range map[string]string{in.Configuration: contents[0], in.Parameters: contents[1]} {
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
		captured, err := readInput(context.Background(), in)
		sum := inputHash(captured)
		if err != nil || seen[sum] {
			t.Fatalf("ambiguous hash %q: %v", sum, err)
		}
		seen[sum] = true
	}
	in.Parameters = ""
	captured, err := readInput(context.Background(), in)
	sum := inputHash(captured)
	if err != nil || seen[sum] {
		t.Fatalf("absent and empty sidecars are indistinguishable: %q (%v)", sum, err)
	}
}

func TestInputHashCancellationAndReadErrors(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	in := dsc.Input{Configuration: filepath.Join(dir, "web.yaml"), Parameters: filepath.Join(dir, "missing.parameters.json")}
	_, err := readInput(context.Background(), in)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing.parameters.json") {
		t.Fatalf("missing parameters: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readInput(ctx, in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled input: %v", err)
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
	r := New(dir, time.Second, client.start, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
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
