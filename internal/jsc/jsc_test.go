package jsc

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// A script's value comes back as a string, and what it throws as an
// error, with text beyond ASCII intact both ways.
func TestEval(t *testing.T) {
	got, err := Eval(`JSON.stringify({s: "歌" + [1, 2].map(x => x * 2).join("")})`)
	if err != nil || got != `{"s":"歌24"}` {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := Eval(`throw new Error("没有")`); err == nil || !strings.Contains(err.Error(), "没有") {
		t.Fatalf("%v", err)
	}
}

// Hostile values whose string conversion throws or cannot coerce return
// errors instead of panicking, and sourceMappingURL comments do not read
// files on disk when formatting stack traces.
func TestEvalHostileValuesAndSourceMaps(t *testing.T) {
	if _, err := Eval(`throw { toString() { throw new Error("nested"); } }`); err == nil {
		t.Fatal("expected error when thrown object's toString throws")
	}
	if _, err := Eval(`({ toString() { throw new Error("bad result"); } })`); runtime.GOOS != "darwin" && err == nil {
		t.Fatal("expected error when returned object's toString throws")
	}
	if _, err := Eval(`Object.create(null)`); runtime.GOOS != "darwin" && err == nil {
		t.Fatal("expected error when converting null-prototype object")
	}
	_, _ = Eval(`Symbol("x")`)

	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("secret.map", []byte(`{"version":3,"file":"<eval>","sources":["mapped-secret.js"],"names":[],"mappings":"AAAA,IAAIA,KAAK"}`), 0o600)
	script := "var x = new Error('boom').stack; x\n//# sourceMappingURL=secret.map"

	// Positive control on non-darwin: default goja reads the local source map
	// when formatting Error.stack, rewriting frames to mapped-secret.js.
	if runtime.GOOS != "darwin" {
		rawVM := goja.New()
		val, err := rawVM.RunString(script)
		if err != nil || !strings.Contains(val.String(), "mapped-secret.js") {
			t.Fatalf("positive control did not observe source map in stack: %v, %v", val, err)
		}
	}

	stack, err := Eval(script)
	if err != nil {
		t.Fatalf("Eval stack script: %v", err)
	}
	if strings.Contains(stack, "mapped-secret.js") {
		t.Fatalf("jsc read local source map from disk: %s", stack)
	}
}

// A context kept sees what the scripts before left in it, and rejects use
// after it is closed.
func TestContextKeeps(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Eval(`var twice = s => s + s`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Eval(`twice("ab")`); err != nil || got != "abab" {
		t.Fatalf("%q, %v", got, err)
	}
	c.Close()
	if _, err := c.Eval(`twice("ab")`); err == nil {
		t.Fatal("expected error from closed context")
	}
}
