package hass

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNameFoldsTypography(t *testing.T) {
	for in, want := range map[string]string{
		"Ivy’s Room":           "ivy's room",
		"  the  Ivy's   room ": "ivy's room",
		"77” OLED":             "77\" oled",
		"Theater":              "theater", // "the" only as a whole word
	} {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Aliases follow er.async_get_entity_aliases: null stands for the computed
// name, and a list without null means the computed name no longer matches.
func TestMatchNamesFollowsRegistryAliases(t *testing.T) {
	aliases := map[string][]any{
		"light.a": {nil, "reading lamp"},
		"light.b": {"desk lamp"},
		"light.c": {},
	}
	cases := map[string][]string{
		"light.a": {"Lamp A", "reading lamp"},
		"light.b": {"desk lamp"},
		"light.c": {},
		"light.d": {"Lamp D"}, // no registry aliases fetched
	}
	for id, want := range cases {
		friendly := "Lamp " + strings.ToUpper(id[len(id)-1:])
		if got := matchNames(friendly, aliases, id); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: names = %v, want %v", id, got, want)
		}
	}
}

func TestIsExposedPrefersExplicitSettings(t *testing.T) {
	s := State{EntityID: "switch.x"}
	explicitOff := map[string]any{"options": map[string]any{"conversation": map[string]any{"should_expose": false}}}
	config := map[string]any{"entity_category": "config"}
	plain := map[string]any{}

	if isExposed(s, "switch", explicitOff, map[string]bool{"switch.x": true}, true) {
		t.Error("registry should_expose=false must win")
	}
	if isExposed(s, "switch", config, nil, false) {
		t.Error("config entities are not exposed by default")
	}
	if !isExposed(s, "switch", plain, nil, false) {
		t.Error("switches are exposed by default")
	}
	if isExposed(State{EntityID: "button.x"}, "button", plain, nil, false) {
		t.Error("buttons are not a default-exposed domain")
	}
	if isExposed(s, "switch", nil, map[string]bool{}, true) {
		t.Error("an unregistered entity absent from the expose list is not exposed")
	}
}

func testVocab() *houseVocab {
	return &houseVocab{
		entities: []vocabEntity{
			{ID: "media_player.bedroom", Domain: "media_player", Names: []string{"Ivy’s room"}, Exposed: true},
			{ID: "light.ivy", Domain: "light", Names: []string{"Ivy's Room"}, Exposed: true},
			{ID: "switch.hidden", Domain: "switch", Names: []string{"Hidden"}},
		},
		areas: []vocabArea{{ID: "ivys_room", Name: "Ivy’s Room"}, {ID: "garage", Name: "Garage", Aliases: []string{"car port"}}},
	}
}

func TestCanonicalisationIsUniqueAndScoped(t *testing.T) {
	v := testVocab()
	if got := v.canonicalName("ivy's room", []string{"media_player"}); got != "Ivy’s room" {
		t.Errorf("media name = %q", got)
	}
	// Two spellings in scope that normalise alike: leave it to HA.
	if got := v.canonicalName("ivys room", nil); got != "ivys room" {
		t.Errorf("unrelated name changed: %q", got)
	}
	if got := v.canonicalName("ivy's room", nil); got != "ivy's room" {
		t.Errorf("ambiguous name rewritten to %q", got)
	}
	// Unexposed entities are never the target of a rewrite.
	if got := v.canonicalName("hidden", nil); got != "hidden" {
		t.Errorf("rewrote to unexposed %q", got)
	}
	if got := v.canonicalArea("Ivy's room"); got != "Ivy’s Room" {
		t.Errorf("area = %q", got)
	}
	if got := v.canonicalArea("the car port"); got != "Garage" {
		t.Errorf("alias = %q", got)
	}
}

func TestSuggestNamesRanksSharedWords(t *testing.T) {
	v := &houseVocab{entities: []vocabEntity{
		{ID: "cover.l", Domain: "cover", Names: []string{"Garage Door Controller Left Garage Door"}, Area: "Garage", Exposed: true},
		{ID: "switch.f", Domain: "switch", Names: []string{"Garage Freezer Plug"}, Area: "Garage", Exposed: true},
		{ID: "light.o", Domain: "light", Names: []string{"Office Light"}, Exposed: true},
	}}
	got := v.suggestNames("left garage door", nil, 2)
	if len(got) == 0 || !strings.HasPrefix(got[0], "Garage Door Controller Left Garage Door") {
		t.Errorf("suggestions = %v", got)
	}
}
