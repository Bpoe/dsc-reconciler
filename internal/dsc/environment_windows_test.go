package dsc

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestBundledEnvironment(t *testing.T) {
	executable := `C:\Program Files\dscd\dscd.exe`
	bundle := filepath.Join(filepath.Dir(executable), "dsc")
	path := filepath.Join(bundle, "dsc.exe")
	for _, test := range []struct {
		name string
		path string
		env  []string
		want []string
	}{
		{"inherited PATH", path,
			[]string{`Path=C:\external;C:\Windows`, `PSModulePath=C:\modules`, "OTHER=value"},
			[]string{"Path=" + bundle + `;C:\external;C:\Windows`, `PSModulePath=C:\modules`, "OTHER=value"}},
		{"custom discovery", path,
			[]string{`PATH=C:\external`, `dsc_resource_path=C:\custom;C:\other`},
			[]string{"PATH=" + bundle + `;C:\external`, `dsc_resource_path=C:\custom;C:\other;` + bundle}},
		{"empty paths", path, []string{"PATH=", "DSC_RESOURCE_PATH="},
			[]string{"PATH=" + bundle, "DSC_RESOURCE_PATH=" + bundle}},
		{"missing PATH", path, []string{"OTHER=value"}, []string{"OTHER=value", "PATH=" + bundle}},
		{"restricted discovery", path,
			[]string{`PATH=C:\external`, `DSC_RESTRICTED_PATH=C:\restricted`, `DSC_RESOURCE_PATH=C:\custom`},
			[]string{`PATH=C:\external`, `DSC_RESTRICTED_PATH=C:\restricted`, `DSC_RESOURCE_PATH=C:\custom`}},
		{"external override", `C:\external\dsc.exe`,
			[]string{`PATH=C:\external`, `DSC_RESOURCE_PATH=C:\custom`},
			[]string{`PATH=C:\external`, `DSC_RESOURCE_PATH=C:\custom`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.env...)
			got := bundledEnvironment(test.path, executable, test.env)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("environment = %q, want %q", got, test.want)
			}
			if !reflect.DeepEqual(test.env, original) {
				t.Fatalf("modified parent environment: %q", test.env)
			}
		})
	}
}
