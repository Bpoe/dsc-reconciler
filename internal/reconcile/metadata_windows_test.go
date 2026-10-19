package reconcile

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"golang.org/x/sys/windows"
)

func TestSharingConflictedConfigurationWithMetadataIsRetried(t *testing.T) {
	dir := t.TempDir()
	path := writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: test}}\n")
	input(t, dir, "z-last.json")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != windows.InvalidHandle {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}
	}()
	var calls []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls = append(calls, filepath.Base(in.Configuration))
		if in.Configuration == path && in.Operation != dsc.OperationTest {
			t.Fatalf("retried configuration operation = %q, want test", in.Operation)
		}
		return dsc.Result{Configuration: filepath.Base(in.Configuration), Operation: &in.Operation, Outcome: "succeeded"}
	}}
	writer := &fakeWriter{}
	r := New(dir, time.Second, client.start, writer, logger())
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"z-last.json"}) || len(writer.results) != 1 ||
		writer.results[0].Configuration != "z-last.json" || writer.results[0].Outcome != "succeeded" {
		t.Fatalf("sharing conflict was not skipped: calls=%v, results=%+v", calls, writer.results)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	calls = nil
	writer.results = nil
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"web.yaml", "z-last.json"}) || len(writer.results) != 2 {
		t.Fatalf("retry did not execute both configurations: calls=%v, results=%+v", calls, writer.results)
	}
	result := writer.results[0]
	if result.Configuration != "web.yaml" || result.Outcome != "succeeded" ||
		result.Operation == nil || *result.Operation != dsc.OperationTest {
		t.Fatalf("retry did not preserve metadata operation: %+v", result)
	}
}
