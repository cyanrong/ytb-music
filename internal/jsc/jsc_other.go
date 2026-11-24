//go:build !darwin

package jsc

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// evalTimeout bounds any single script execution so a pathological player
// script cannot stall currentSolver forever and block fallback to standard
// audio; tests shorten it.
var evalTimeout = 15 * time.Second

var (
	runtimes   sync.Map // uintptr -> *goja.Runtime
	nextHandle atomic.Uintptr
)

func newContext() (uintptr, error) {
	vm := goja.New()
	// Keep contexts strictly compute-only: never read local files referenced
	// by //# sourceMappingURL comments in third-party scripts.
	vm.SetParserOptions(parser.WithDisableSourceMaps)
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
	vm.ClearInterrupt()
	timer := time.AfterFunc(evalTimeout, func() {
		vm.Interrupt("execution timed out")
	})
	val, err := vm.RunString(script)
	timer.Stop()
	vm.ClearInterrupt()
	if err != nil {
		var exc *goja.Exception
		if errors.As(err, &exc) {
			if msg, convErr := safeString(vm, exc.Value()); convErr == nil && msg != "" {
				return "", errors.New("jsc: " + msg)
			}
			return "", errors.New("jsc: uncaught exception")
		}
		return "", errors.New("jsc: " + err.Error())
	}
	if val == nil || goja.IsUndefined(val) {
		return "undefined", nil
	}
	if goja.IsNull(val) {
		return "null", nil
	}
	s, convErr := safeString(vm, val)
	if convErr != nil {
		return "", errors.New("jsc: cannot convert result to string")
	}
	return s, nil
}

// safeString converts val via JavaScript's ToString inside vm.Try so that
// throwing toString() methods never panic the host process.
func safeString(vm *goja.Runtime, val goja.Value) (string, error) {
	if val == nil {
		return "", nil
	}
	var out string
	if ex := vm.Try(func() {
		out = val.ToString().String()
	}); ex != nil {
		return "", ex
	}
	return out, nil
}
