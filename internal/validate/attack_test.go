package validate

import (
	"net/url"
	"path"
	"strings"
	"testing"
)

// Values an attacker (or a prompt-injected model) might put in a field that
// is interpolated into an upstream URL path, e.g. /api/services/{domain}/{service}.
var traversal = []string{
	".", "..", "...", "./x", "a/..", "a/../b", "..%2f", "%2e%2e", "%2E%2E%2F",
	"a\\b", "..\\..", "a\x00", "a\x00.jpg", "a\n", "a\r\n", "a\t", " a",
	"ａ", "a／b", "a∕b", "a‥", "a;b", "a?b=c", "a#b", "a&b", "a=b", "a+b",
	"a b", "a'b", `a"b`, "a<b>", "a|b", "a`b", "a$b", "a@b", "a:b", "a,b",
	"http://evil", "//evil", "~root", "*", "a*",
	strings.Repeat("a", maxIdentifierLen+1),
}

func TestIdentifierRejectsTraversal(t *testing.T) {
	for _, v := range traversal {
		if err := Identifier("field", v); err == nil {
			t.Errorf("Identifier(%q) accepted", v)
		}
	}
}

// checkInvariant is what "safe for a URL path segment" means: the value
// survives path escaping unchanged, is not a dot segment, and cannot move
// a path when joined into it.
func checkInvariant(t *testing.T, v string) {
	t.Helper()
	if Identifier("f", v) != nil {
		return
	}
	if url.PathEscape(v) != v {
		t.Errorf("accepted %q needs escaping", v)
	}
	if v == "." || v == ".." {
		t.Errorf("accepted dot segment %q", v)
	}
	if got := path.Join("/api/states", v); got != "/api/states/"+v {
		t.Errorf("accepted %q moves the path: %s", v, got)
	}
	if strings.ContainsAny(v, "/\\%?#&=+ \t\r\n\x00") || len(v) > maxIdentifierLen {
		t.Errorf("accepted %q", v)
	}
}

func FuzzIdentifier(f *testing.F) {
	for _, s := range append(traversal, "light.living_room", "a..b", "turn_on", "x-1") {
		f.Add(s)
	}
	f.Fuzz(checkInvariant)
}

// The upper bound is generous (Home Assistant entity ids are far shorter),
// but a value cannot be an unbounded string spliced into a URL.
func TestIdentifierLengthBound(t *testing.T) {
	if err := Identifier("f", strings.Repeat("a", maxIdentifierLen)); err != nil {
		t.Errorf("max-length identifier rejected: %v", err)
	}
}
