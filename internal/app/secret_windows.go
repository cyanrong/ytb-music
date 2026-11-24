//go:build windows

package app

import (
	"encoding/base64"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const dpapiPrefix = "dpapi:"

const secretStorageSupported = true

func sealSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	in := []byte(plain)
	inBlob := windows.DataBlob{
		Size: uint32(len(in)),
		Data: &in[0],
	}
	var outBlob windows.DataBlob
	if err := windows.CryptProtectData(&inBlob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &outBlob); err != nil {
		return "", err
	}
	defer func() {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
	}()
	cipher := unsafe.Slice(outBlob.Data, outBlob.Size)
	return dpapiPrefix + base64.StdEncoding.EncodeToString(cipher), nil
}

func openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, dpapiPrefix) {
		// Upgrade legacy plaintext if present.
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, dpapiPrefix))
	if err != nil || len(raw) == 0 {
		return "", err
	}
	inBlob := windows.DataBlob{
		Size: uint32(len(raw)),
		Data: &raw[0],
	}
	var outBlob windows.DataBlob
	if err := windows.CryptUnprotectData(&inBlob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &outBlob); err != nil {
		return "", err
	}
	defer func() {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
	}()
	plain := string(unsafe.Slice(outBlob.Data, outBlob.Size))
	return plain, nil
}
