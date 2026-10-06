package results

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Protected DACL: the service identity, SYSTEM and administrators only.
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
}

func makeDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := makeDirectory(parent); err != nil {
		return err
	}
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd,
	}
	return windows.CreateDirectory(p, &attributes)
}

func protectFile(path string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(longPath(path), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func validateDestination(name, path string) error {
	if len(utf16.Encode([]rune(name))) > 255 {
		return fmt.Errorf("result basename exceeds 255 UTF-16 code units: %q", name)
	}
	if len(utf16.Encode([]rune(longPath(path)))) >= 32767 {
		return fmt.Errorf("result path exceeds the Windows extended-path limit: %q", path)
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("invalid Windows result basename %q", name)
	}
	for _, r := range name {
		if r < 32 {
			return fmt.Errorf("invalid Windows result basename %q", name)
		}
	}
	stem, _, _ := strings.Cut(strings.ToUpper(name), ".")
	chars := []rune(stem)
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
		(len(chars) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && strings.ContainsRune("123456789\u00b9\u00b2\u00b3", chars[3])) {
		return fmt.Errorf("reserved Windows result basename %q", name)
	}
	return nil
}

func longPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

func replaceFile(source, target string) (bool, error) {
	from, err := windows.UTF16PtrFromString(longPath(source))
	if err != nil {
		return false, err
	}
	to, err := windows.UTF16FromString(longPath(target))
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(from, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return false, err
	}
	// FILE_RENAME_INFO has a variable-length UTF-16 name and native HANDLE alignment.
	type renameInfo struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	var layout renameInfo
	buffer := make([]byte, int(unsafe.Sizeof(layout))+2*len(to))
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = 0x1 | 0x2 // REPLACE_IF_EXISTS | POSIX_SEMANTICS: preserve open readers.
	info.FileNameLength = uint32(2 * (len(to) - 1))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[unsafe.Offsetof(layout.FileName)])), len(to)), to)
	err = windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer)))
	return err == nil, errors.Join(err, windows.CloseHandle(handle))
}

// Windows has no supported unprivileged equivalent of a POSIX directory fsync.
func syncDirectory(string) error { return nil }
