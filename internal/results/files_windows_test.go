package results

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func readShared(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return io.ReadAll(file)
}

func TestSharingViolationPreservesPreviousResult(t *testing.T) {
	writer := newTestWriter(t)
	result := testResult("a.yaml", "succeeded")
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(result.Configuration)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		windows.CloseHandle(handle)
		t.Fatal(err)
	}
	result.Outcome = "failed"
	writeErr := writer.Write(context.Background(), result)
	windows.CloseHandle(handle)
	if writeErr == nil {
		t.Fatal("replacement succeeded despite a reader denying delete sharing")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("sharing failure lost old result")
	}
	assertNoTemps(t, writer.dir)
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatalf("retry after releasing reader: %v", err)
	}
}

func TestProtectedDACL(t *testing.T) {
	writer := newTestWriter(t)
	result := testResult("a.yaml", "succeeded")
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(result.Configuration)
	for _, path := range []string{writer.dir, path} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("unprotected DACL: %s (%v)", path, err)
		}
		expected, err := privateDescriptor()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.ReplaceAll(sd.String(), "D:PAI", "D:P")
		want := expected.String()
		if path != writer.dir {
			want = strings.ReplaceAll(want, ";OICI;", ";;")
		}
		if got != want {
			t.Fatal("DACL does not restrict full access to SYSTEM, administrators and the current identity")
		}
	}
}

func TestReservedWindowsNames(t *testing.T) {
	w := newTestWriter(t)
	for _, name := range []string{"NUL.yaml", "COM1.json", "LPT\u00b9.yml", "x:stream.yaml", "x?.yaml"} {
		if _, err := w.Destination(name); err == nil {
			t.Errorf("accepted reserved name %q", name)
		}
	}
}
