//go:build !windows

package ytm

import "net/url"

func systemProxy(*url.URL) (*url.URL, ProxySource, error) {
	return nil, ProxySourceNone, nil
}
