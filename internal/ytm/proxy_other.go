//go:build !windows

package ytm

import "net/url"

func systemProxy(*url.URL) (*url.URL, error) {
	return nil, nil
}
