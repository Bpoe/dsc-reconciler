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

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
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
	for _, name := range []string{"10-a.yaml", "10-a.json", "with spaces.yml", "team/deep/web.yaml", "other/web.yaml"} {
		path, err := writer.Destination(name)
		if err != nil || path != filepath.Join(writer.dir, filepath.FromSlash(name+".result.json")) {
			t.Errorf("%q => %q, %v", name, path, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../a.yaml", `..\a.yaml`, "/a.yaml", `C:\a.yaml`,
		"a/../b.yaml", "a//b.yaml", "a/./b.yaml", "a/b.yaml/", `a\b.yaml`,
		"a\x00.json", string([]byte{0xff}) + ".yaml", strings.Repeat("x", 245) + ".json",
		strings.Repeat("x", 256) + "/a.yaml"} {
		if _, err := writer.Destination(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestReplacementAndFailure(t *testing.T) {
	for _, name := range []string{"a.yaml", "team/deep/a.yaml"} {
		t.Run(name, func(t *testing.T) { testReplacementAndFailure(t, name) })
	}
}

func testReplacementAndFailure(t *testing.T, name string) {
	writer := newTestWriter(t)
	result := testResult(name, "succeeded")
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
	result = testResult(strings.TrimSuffix(name, "a.yaml")+"blocked.yaml", "succeeded")
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
	for _, name := range []string{"a.yaml", "team/deep/a.yaml"} {
		t.Run(name, func(t *testing.T) { testCompleteReplacementVisibility(t, name) })
	}
}

func testCompleteReplacementVisibility(t *testing.T, name string) {
	writer := newTestWriter(t)
	r := testResult(name, "succeeded")
	r.Stderr = strings.Repeat("a", 65536)
	if err := writer.Write(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(r.Configuration)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader := make(chan error, 1)
	observed := make(chan byte, 1)
	go func() {
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
			select {
			case observed <- got.Stderr[0]:
			case <-ctx.Done():
			}
		}
		reader <- nil
	}()
	waitFor := func(want byte) error {
		for {
			select {
			case got := <-observed:
				if got == want {
					return nil
				}
			case err := <-reader:
				reader <- err
				return errors.Join(errors.New("reader stopped"), err)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := waitFor('a'); err != nil {
		t.Fatal(err)
	}
	var writeErr error
	for i := 0; i < 20; i++ {
		r.Stderr = strings.Repeat(string(rune('a'+i%2)), 65536)
		if writeErr = writer.Write(context.Background(), r); writeErr != nil {
			break
		}
		if writeErr = waitFor(byte('a' + i%2)); writeErr != nil {
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
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(entry.Name(), ".dscd-") {
			t.Errorf("temporary file remains: %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNestedResultParentFailures(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			writer := newTestWriter(t)
			outside := t.TempDir()
			parent := filepath.Join(writer.dir, "blocked")
			if kind == "file" {
				if err := os.WriteFile(parent, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, parent); err != nil {
				t.Skipf("directory symlink unavailable: %v", err)
			}
			if err := writer.Write(context.Background(), testResult("blocked/a.yaml", "succeeded")); err == nil {
				t.Fatal("accepted non-directory or redirected parent")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote outside results root: %v, %v", entries, err)
			}
			assertNoTemps(t, writer.dir)
		})
	}
}
