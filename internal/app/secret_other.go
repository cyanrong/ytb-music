//go:build !windows

package app

const secretStorageSupported = false

func sealSecret(plain string) (string, error) {
	return plain, nil
}

func openSecret(stored string) (string, error) {
	return stored, nil
}
