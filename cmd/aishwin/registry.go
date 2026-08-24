//go:build windows

package main

// registry.go: thin HKEY_CURRENT_USER DWORD read/write helpers backing
// settings.go's persistence. A dev build (aishwindev tag) uses a SEPARATE
// registry subkey (see settings.go's registryKeyPath) so testing this
// feature can never overwrite a real user's persisted settings.

import (
	"syscall"
	"unsafe"
)

var advapi32 = syscall.NewLazyDLL("advapi32.dll")

var (
	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

const (
	// HKEY_CURRENT_USER (0x80000001): a predefined handle constant, passed
	// as a plain unsigned bit pattern -- unlike CW_USEDEFAULT elsewhere in
	// this codebase, this is a HANDLE parameter, not a signed int one, so
	// there is no sign-extension subtlety to work around here.
	hkeyCurrentUser = 0x80000001
	keyAllAccess    = 0xF003F
	regDword        = 4
	regSz           = 1
	errorSuccess    = 0

	// regStringBufChars is a generous fixed buffer size for
	// registryGetString -- every value this app stores (a hostname,
	// username, or port-as-string) is far shorter, and UTF16ToString stops
	// at the first NUL regardless of how much of the buffer is unused.
	regStringBufChars = 512
)

func openOrCreateSettingsKey() (syscall.Handle, error) {
	var hKey syscall.Handle
	r, _, err := procRegCreateKeyExW.Call(
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(utf16ptr(registryKeyPath))),
		0, 0, 0, keyAllAccess, 0,
		uintptr(unsafe.Pointer(&hKey)), 0,
	)
	if r != errorSuccess {
		return 0, err
	}
	return hKey, nil
}

// registryGetDWORD reads a DWORD value, reporting ok=false if the key or
// value doesn't exist yet (first run) or isn't a DWORD.
func registryGetDWORD(name string) (value uint32, ok bool) {
	hKey, err := openOrCreateSettingsKey()
	if err != nil {
		return 0, false
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	var regType uint32
	size := uint32(unsafe.Sizeof(value))
	r, _, _ := procRegQueryValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(utf16ptr(name))),
		0,
		uintptr(unsafe.Pointer(&regType)),
		uintptr(unsafe.Pointer(&value)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != errorSuccess || regType != regDword {
		return 0, false
	}
	return value, true
}

// registrySetDWORD writes a DWORD value, creating the key if needed.
func registrySetDWORD(name string, value uint32) error {
	hKey, err := openOrCreateSettingsKey()
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	r, _, err := procRegSetValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(utf16ptr(name))),
		0, regDword,
		uintptr(unsafe.Pointer(&value)), 4,
	)
	if r != errorSuccess {
		return err
	}
	return nil
}

// registryGetString reads a REG_SZ value, reporting ok=false if the key or
// value doesn't exist yet (first run) or isn't a string.
func registryGetString(name string) (value string, ok bool) {
	hKey, err := openOrCreateSettingsKey()
	if err != nil {
		return "", false
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	var regType uint32
	buf := make([]uint16, regStringBufChars)
	size := uint32(regStringBufChars * 2)
	r, _, _ := procRegQueryValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(utf16ptr(name))),
		0,
		uintptr(unsafe.Pointer(&regType)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != errorSuccess || regType != regSz {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

// registrySetString writes a REG_SZ value, creating the key if needed.
func registrySetString(name string, value string) error {
	hKey, err := openOrCreateSettingsKey()
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	u, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	r, _, err2 := procRegSetValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(utf16ptr(name))),
		0, regSz,
		uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)*2),
	)
	if r != errorSuccess {
		return err2
	}
	return nil
}
