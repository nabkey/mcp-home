package hass

import "testing"

func TestRegistryCommand(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"area_id": "kitchen", "name": "Kitchen"}}
	})

	result, err := c.RegistryCommand("area", "update", map[string]any{"area_id": "kitchen", "name": "Kitchen"})
	if err != nil {
		t.Fatalf("RegistryCommand: %v", err)
	}
	if result["name"] != "Kitchen" {
		t.Errorf("result name = %v, want Kitchen", result["name"])
	}

	got := rec.last()
	if got["type"] != "config/area_registry/update" {
		t.Errorf("type = %v, want config/area_registry/update", got["type"])
	}
	if got["area_id"] != "kitchen" {
		t.Errorf("area_id = %v, want kitchen", got["area_id"])
	}
}

func TestRegistryEntityRemoveVerb(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": nil}
	})

	// The entity registry uses "remove" rather than "delete".
	if _, err := c.RegistryCommand("entity", "remove", map[string]any{"entity_id": "light.kitchen"}); err != nil {
		t.Fatalf("RegistryCommand: %v", err)
	}
	got := rec.last()
	if got["type"] != "config/entity_registry/remove" {
		t.Errorf("type = %v, want config/entity_registry/remove", got["type"])
	}
}

func TestRegistrySpecActionSupport(t *testing.T) {
	// device supports only update; entity has no create; area has all three.
	if _, ok := registrySpecs["device"].verbs["delete"]; ok {
		t.Error("device registry should not expose a delete action")
	}
	if _, ok := registrySpecs["entity"].verbs["create"]; ok {
		t.Error("entity registry should not expose a create action")
	}
	if registrySpecs["entity"].verbs["delete"] != "remove" {
		t.Errorf("entity delete should map to remove, got %q", registrySpecs["entity"].verbs["delete"])
	}
	for _, a := range []string{"create", "update", "delete"} {
		if _, ok := registrySpecs["area"].verbs[a]; !ok {
			t.Errorf("area registry missing action %q", a)
		}
	}
}
