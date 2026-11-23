//go:build !darwin

package jsc

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/dop251/goja"
)

var (
	runtimes   sync.Map // uintptr -> *goja.Runtime
	nextHandle atomic.Uintptr
)

func newContext() (uintptr, error) {
	vm := goja.New()
	id := nextHandle.Add(1)
	runtimes.Store(id, vm)
	return id, nil
}

func releaseContext(ref uintptr) {
	runtimes.Delete(ref)
}

func evalIn(ref uintptr, script string) (string, error) {
	v, ok := runtimes.Load(ref)
	if !ok {
		return "", errors.New("jsc: the context is closed")
	}
	vm := v.(*goja.Runtime)
	val, err := vm.RunString(script)
	if err != nil {
		var exc *goja.Exception
		if errors.As(err, &exc) {
			return "", errors.New("jsc: " + exc.Value().String())
		}
		return "", errors.New("jsc: " + err.Error())
	}
	if val == nil {
		return "undefined", nil
	}
	return val.String(), nil
}
