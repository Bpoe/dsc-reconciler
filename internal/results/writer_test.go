package results

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsc-reconciler/internal/dsc"
)

func newTestWriter(t *testing.T) *Writer {
	t.Helper()
	writer, err := NewWriter(filepath.Join(t.TempDir(), "results with spaces"))
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func testResult(name, outcome string) dsc.Result {
	return dsc.Result{SchemaVersion: 1, Configuration: name, StartedAt: time.Now().UTC(),
		FinishedAt: time.Now().UTC(), Outcome: outcome}
}

func TestDestination(t *testing.T) {
	writer := newTestWriter(t)
	for _, name := range []string{"10-a.yaml", "10-a.json", "with spaces.yml"} {
		path, err := writer.Destination(name)
		if err != nil || filepath.Base(path) != name+".result.json" {
			t.Errorf("%q => %q, %v", name, path, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../a.yaml", `..\a.yaml`, "a\x00.json", string([]byte{0xff}) + ".yaml", strings.Repeat("x", 245) + ".json"} {
		if _, err := writer.Destination(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestReplacementAndFailure(t *testing.T) {
	writer := newTestWriter(t)
	result := testResult("a.yaml", "succeeded")
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(result.Configuration)
	assertResult := func(want string) {
		t.Helper()
		data, err := readShared(path)
		if err != nil {
			t.Fatal(err)
		}
		var got dsc.Result
		if err := json.Unmarshal(data, &got); err != nil || got.Outcome != want {
			t.Fatalf("published = %s (%v)", data, err)
		}
		assertNoTemps(t, writer.dir)
	}
	assertResult("succeeded")
	result.Outcome = "failed"
	result.Error = &dsc.Failure{Kind: "dsc", Message: "DSC reported errors"}
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	assertResult("failed")
	result.DSCResult = json.RawMessage("invalid")
	if err := writer.Write(context.Background(), result); err == nil {
		t.Fatal("accepted invalid JSON")
	}
	assertResult("failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Write(ctx, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	assertResult("failed")
	result = testResult("blocked.yaml", "succeeded")
	blocked, _ := writer.Destination(result.Configuration)
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(context.Background(), result); err == nil {
		t.Fatal("replaced directory")
	}
	assertNoTemps(t, writer.dir)
}

func TestCompleteReplacementVisibility(t *testing.T) {
	writer := newTestWriter(t)
	r := testResult("a.yaml", "succeeded")
	r.Stderr = strings.Repeat("a", 65536)
	if err := writer.Write(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(r.Configuration)
	ctx, cancel := context.WithCancel(context.Background())
	reader := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		for ctx.Err() == nil {
			data, err := readShared(path)
			if err != nil {
				reader <- err
				return
			}
			var got dsc.Result
			if err := json.Unmarshal(data, &got); err != nil {
				reader <- err
				return
			}
			if got.Stderr != strings.Repeat("a", 65536) && got.Stderr != strings.Repeat("b", 65536) {
				reader <- errors.New("incomplete replacement")
				return
			}
		}
		reader <- nil
	}()
	<-started
	var writeErr error
	for i := 0; i < 20; i++ {
		r.Stderr = strings.Repeat(string(rune('a'+i%2)), 65536)
		if writeErr = writer.Write(context.Background(), r); writeErr != nil {
			break
		}
	}
	cancel()
	if readErr := <-reader; readErr != nil || writeErr != nil {
		t.Fatalf("reader: %v; writer: %v", readErr, writeErr)
	}
	assertNoTemps(t, writer.dir)
}

func TestStartupFailures(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(file); err == nil {
		t.Fatal("accepted file as output directory")
	}
	if _, err := NewWriter(filepath.Join(file, "child")); err == nil {
		t.Fatal("accepted non-directory parent")
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".dscd-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v (%v)", files, err)
	}
}
