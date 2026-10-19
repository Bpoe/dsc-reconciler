package reconcile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"golang.org/x/sys/windows"
)

func TestWindowsConfigurationProtectionThroughPublication(t *testing.T) {
	for _, outcome := range []string{"success", "execution failure", "publication failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			const text = `{"resources":[]}`
			config := writeInput(t, dir, "a.json", text)
			replacement := writeInput(t, dir, ".replacement", `{"resources":["replacement"]}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executing := make(chan struct{})
			publishing := make(chan struct{})
			executeRelease := make(chan struct{})
			publicationRelease := make(chan struct{})
			var releaseExecute, releasePublication sync.Once
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				if in.ConfigurationText != text {
					t.Errorf("configuration text = %q, want %q", in.ConfigurationText, text)
				}
				close(executing)
				<-executeRelease
				result := dsc.Result{Outcome: "succeeded"}
				switch outcome {
				case "execution failure":
					result.Outcome = "failed"
					result.Error = &dsc.Failure{Kind: "dsc"}
				case "cancellation":
					result.Outcome = "canceled"
					result.Error = &dsc.Failure{Kind: "canceled"}
				}
				return result
			}}
			writer := &fakeWriter{write: func(ctx context.Context, _ dsc.Result) error {
				if ctx.Err() != nil {
					t.Error("publication inherited canceled execution context")
				}
				close(publishing)
				<-publicationRelease
				if outcome == "publication failure" {
					return errors.New("publication failed")
				}
				return nil
			}}
			r := New(dir, time.Second, client.start, writer, logger())
			done := make(chan struct{})
			var passErr error
			go func() {
				passErr = r.Pass(ctx)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				releaseExecute.Do(func() { close(executeRelease) })
				releasePublication.Do(func() { close(publicationRelease) })
				waitWindowsProtection(t, done)
			})

			waitWindowsProtection(t, executing)
			assertWindowsConfigurationProtected(t, config, replacement, text)
			if outcome == "cancellation" {
				cancel()
			}
			releaseExecute.Do(func() { close(executeRelease) })
			waitWindowsProtection(t, publishing)
			assertWindowsConfigurationProtected(t, config, replacement, text)
			releasePublication.Do(func() { close(publicationRelease) })
			waitWindowsProtection(t, done)
			if passErr != nil && !(outcome == "cancellation" && errors.Is(passErr, context.Canceled)) {
				t.Fatalf("pass: %v", passErr)
			}
			if got := writer.snapshot(); len(got) != 1 {
				t.Fatalf("published %d results, want 1", len(got))
			}
			if err := os.Remove(config); err != nil {
				t.Fatalf("configuration still protected after %s: %v", outcome, err)
			}
		})
	}
}

func assertWindowsConfigurationProtected(t *testing.T, config, replacement, text string) {
	t.Helper()
	if err := os.Remove(config); err == nil {
		t.Error("deleted protected configuration")
	}
	if err := replaceWindowsConfiguration(replacement, config); err == nil {
		t.Error("replaced protected configuration")
	}
	name, err := windows.UTF16PtrFromString(config)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err == nil {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			t.Error(closeErr)
		}
		t.Error("opened protected configuration for writing")
	} else if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("write open failed for reason other than protection: %v", err)
	}
	reader, err := openInputFile(config)
	if err != nil {
		t.Fatalf("compatible reader blocked by protection: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(got) != text {
		t.Errorf("protected configuration read = %q, read error = %v, close error = %v", got, readErr, closeErr)
	}
}

func TestWindowsConfigurationProtectionIsPerAttempt(t *testing.T) {
	dir := t.TempDir()
	first := writeInput(t, dir, "10-a.json", `{"resources":[]}`)
	second := writeInput(t, dir, "20-b.json", `{"resources":[]}`)
	calls := 0
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls++
		if in.Configuration == first {
			writer, err := os.OpenFile(second, os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("future configuration protected before its attempt: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(first); err != nil {
			t.Fatalf("previous configuration protected during next attempt: %v", err)
		}
		return dsc.Result{Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(writer.snapshot()) != 2 {
		t.Fatalf("executed %d times and published %d results", calls, len(writer.snapshot()))
	}
	if err := os.Remove(second); err != nil {
		t.Fatalf("final configuration handle leaked: %v", err)
	}
}

func TestWindowsConfigurationStaleAfterDiscovery(t *testing.T) {
	for _, change := range []string{"disappeared", "sharing violation"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			config := writeInput(t, dir, "a.json", `{"resources":[]}`)
			candidates, err := discover(dir)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("discovery = %v, %v", candidates, err)
			}
			release := func() {}
			if change == "disappeared" {
				if err := os.Remove(config); err != nil {
					t.Fatal(err)
				}
			} else {
				name, err := windows.UTF16PtrFromString(config)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := windows.CreateFile(name, windows.GENERIC_WRITE,
					windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
					nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				release = func() {
					once.Do(func() {
						if err := windows.CloseHandle(handle); err != nil {
							t.Error(err)
						}
					})
				}
				t.Cleanup(release)
			}
			file, err := openConfiguration(context.Background(), config)
			if file != nil {
				if closeErr := file.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if !errors.Is(err, errStaleInput) {
				t.Errorf("protection error = %v, want stale input", err)
			}
			calls := 0
			client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
				calls++
				return dsc.Result{Outcome: "succeeded"}
			}}
			writer := &fakeWriter{}
			r := New(dir, time.Second, client.start, writer, logger())
			if err := r.execute(context.Background(), candidates); err != nil {
				t.Fatalf("stale candidate pass: %v", err)
			}
			if calls != 0 || len(writer.snapshot()) != 0 {
				t.Fatalf("stale candidate executed %d times and published %d results", calls, len(writer.snapshot()))
			}
			release()
			if change == "disappeared" {
				writeInput(t, dir, "a.json", `{"resources":[]}`)
			}
			if err := r.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(writer.snapshot()) != 1 {
				t.Fatalf("next pass executed %d times and published %d results", calls, len(writer.snapshot()))
			}
			if err := os.Remove(config); err != nil {
				t.Fatalf("configuration handle leaked: %v", err)
			}
		})
	}
}

func TestWindowsReplacedConfigurationRediscoversParameters(t *testing.T) {
	for _, change := range []string{"added", "removed", "changed format", "resolved ambiguity"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			config := writeInput(t, dir, "a.yaml", "resources: []\n")
			oldParameters := filepath.Join(dir, "a.parameters.yaml")
			if change != "added" {
				writeInput(t, dir, "a.parameters.yaml", "old: parameters\n")
			}
			if change == "resolved ambiguity" {
				writeInput(t, dir, "a.parameters.json", `{"old":"ambiguous"}`)
			}
			candidates, err := discover(dir)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("discovery = %v, %v", candidates, err)
			}
			if change != "added" {
				if err := os.Remove(oldParameters); err != nil {
					t.Fatal(err)
				}
			}
			const configurationText = "metadata: {dscd: {operation: test}}\nresources: []\n"
			expected := dsc.Input{
				Configuration:     config,
				ConfigurationText: configurationText,
				Operation:         dsc.OperationTest,
			}
			if change != "removed" {
				expected.Parameters = writeInput(t, dir, "a.parameters.json", `{"current":"parameters"}`)
				expected.ParametersText = `{"current":"parameters"}`
			}
			replacement := writeInput(t, dir, ".replacement", configurationText)
			if err := replaceWindowsConfiguration(replacement, config); err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls++
				if in != expected {
					t.Errorf("submitted input = %+v, want current pair %+v", in, expected)
				}
				return dsc.Result{Outcome: "succeeded", Operation: &in.Operation}
			}}
			writer := &fakeWriter{}
			r := New(dir, time.Second, client.start, writer, logger())
			if err := r.execute(context.Background(), candidates); err != nil {
				t.Fatal(err)
			}
			got := writer.snapshot()
			if calls != 1 || len(got) != 1 {
				t.Fatalf("executed %d times and published %d results", calls, len(got))
			}
			wantParameters := ""
			if expected.Parameters != "" {
				wantParameters = filepath.Base(expected.Parameters)
			}
			if got[0].Error != nil || got[0].InputHash != inputHash(expected) ||
				got[0].Parameters != wantParameters || got[0].Operation == nil || *got[0].Operation != expected.Operation {
				t.Fatalf("result does not describe current pair: %+v", got[0])
			}
			if err := os.Remove(config); err != nil {
				t.Fatalf("replacement handle leaked: %v", err)
			}
		})
	}
}

func replaceWindowsConfiguration(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING)
}

func waitWindowsProtection(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for configuration protection test")
	}
}
