package testutil

import (
	"errors"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReplaceResult matches dscd's FileRenameInfoEx replacement, including its
// POSIX_SEMANTICS flag. MoveFileEx/os.Rename does not preserve open readers.
func ReplaceResult(source, target string) error {
	from, err := windows.UTF16PtrFromString(longPath(source))
	if err != nil {
		return err
	}
	to, err := windows.UTF16FromString(longPath(target))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(from, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	type renameInfo struct {
		flags  uint32
		root   windows.Handle
		length uint32
		name   [1]uint16
	}
	var layout renameInfo
	offset := unsafe.Offsetof(layout.name)
	buffer := make([]byte, int(unsafe.Sizeof(layout))+2*len(to))
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.flags = 0x1 | 0x2 // REPLACE_IF_EXISTS | POSIX_SEMANTICS
	info.length = uint32(2 * (len(to) - 1))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset])), len(to)), to)
	err = windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer)))
	return errors.Join(err, windows.CloseHandle(handle))
}

func longPath(path string) string {
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
