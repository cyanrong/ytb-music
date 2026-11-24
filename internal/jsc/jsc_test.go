package jsc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
// files on disk.
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
	mapPath := filepath.Join(dir, "secret.map")
	os.WriteFile(mapPath, []byte(`{"version":3,"sources":["secret.js"],"names":[],"mappings":"AAAA"}`), 0o600)
	script := "function f() { throw new Error('boom'); }\nf();\n//# sourceMappingURL=" + filepath.ToSlash(mapPath)
	_, err := Eval(script)
	if err == nil || strings.Contains(err.Error(), "secret.js") {
		t.Fatalf("source map should be ignored in compute-only context, got: %v", err)
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
