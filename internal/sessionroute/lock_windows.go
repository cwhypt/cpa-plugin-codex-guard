package sessionroute

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = kernel32.NewProc("LockFileEx")
var unlockFileEx = kernel32.NewProc("UnlockFileEx")

func lockStateFile(file *os.File) (func(), error) {
	overlapped := new(syscall.Overlapped)
	const exclusiveLock = 2
	ok, _, err := lockFileEx.Call(file.Fd(), exclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	if ok == 0 {
		return nil, err
	}
	return func() {
		unlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	}, nil
}
