//go:build !windows

package ytm

import (
	"context"
	"net/url"
)

func systemProxy(context.Context, *url.URL) (*url.URL, ProxySource, error) {
	return nil, ProxySourceNone, nil
}
