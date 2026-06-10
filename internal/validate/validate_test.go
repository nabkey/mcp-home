package validate

import "testing"

func TestIdentifier(t *testing.T) {
	valid := []string{
		"light",
		"turn_on",
		"light.living_room",
		"automation-1",
		"Front_Door.Camera-2",
		"a",
		"0abc",
	}
	for _, v := range valid {
		if err := Identifier("field", v); err != nil {
			t.Errorf("Identifier(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"../etc/passwd",
		"a/b",
		".hidden",
		"-leading-hyphen",
		"_leading_underscore",
		"with space",
		"semi;colon",
		"new\nline",
		"per%cent",
		"quest?ion",
		"ha#sh",
	}
	for _, v := range invalid {
		if err := Identifier("field", v); err == nil {
			t.Errorf("Identifier(%q) = nil, want error", v)
		}
	}
}

func TestIdentifierErrorMentionsName(t *testing.T) {
	err := Identifier("camera", "")
	if err == nil || err.Error() != "camera is required" {
		t.Errorf("err = %v, want %q", err, "camera is required")
	}
}
