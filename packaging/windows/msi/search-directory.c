#include <windows.h>
#include <msi.h>
#include <msiquery.h>
#include <wchar.h>

/* DrLocator rejects ".." segments. Derive a canonical parent directory before
   AppSearch; the native MSI file signature still verifies dsc.exe exists. */
__declspec(dllexport) UINT __stdcall PrepareDSCSearchDirectory(MSIHANDLE install)
{
    WCHAR executable[32768], directory[32768];
    DWORD length = ARRAYSIZE(executable), result;
    WCHAR *separator;
    UINT status = MsiSetPropertyW(install, L"DSC_SEARCH_DIR", L"");
    if (status != ERROR_SUCCESS)
        return status;

    status = MsiGetPropertyW(install, L"DSC_PATH", executable, &length);
    if (status == ERROR_MORE_DATA)
        return ERROR_SUCCESS;
    if (status != ERROR_SUCCESS)
        return status;
    if (length < 3 || !((executable[0] >= L'A' && executable[0] <= L'Z') ||
                       (executable[0] >= L'a' && executable[0] <= L'z')) ||
        executable[1] != L':' || executable[2] != L'\\' ||
        wcschr(executable, L'"') != NULL)
        return ERROR_SUCCESS;

    separator = wcsrchr(executable, L'\\');
    separator[1] = L'\0';
    result = GetFullPathNameW(executable, ARRAYSIZE(directory), directory, NULL);
    if (result == 0 || result >= ARRAYSIZE(directory))
        return ERROR_SUCCESS;
    return MsiSetPropertyW(install, L"DSC_SEARCH_DIR", directory);
}
