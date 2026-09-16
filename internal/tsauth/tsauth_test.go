package tsauth

import "testing"

func TestAllowed(t *testing.T) {
	user := Identity{Login: "chris@example.com"}
	tagged := Identity{IsTagged: true, Tags: []string{"tag:voice-agent"}}

	cases := []struct {
		name   string
		id     Identity
		logins []string
		tags   []string
		want   bool
	}{
		{"open when no lists", user, nil, nil, true},
		{"login match", user, []string{"chris@example.com"}, nil, true},
		{"login mismatch", user, []string{"other@example.com"}, nil, false},
		{"tag match", tagged, nil, []string{"tag:voice-agent"}, true},
		{"tagged node cannot use login list", tagged, []string{"chris@example.com"}, nil, false},
		{"user cannot use tag list", user, nil, []string{"tag:voice-agent"}, false},
	}
	for _, c := range cases {
		if got := allowed(c.id, c.logins, c.tags); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
