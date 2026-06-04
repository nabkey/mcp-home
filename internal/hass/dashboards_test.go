package hass

import "testing"

func TestDashboardConfigRoundTrip(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		switch cmd["type"] {
		case "lovelace/config":
			return map[string]any{"result": map[string]any{"title": "Home", "views": []any{}}}
		case "lovelace/config/save", "lovelace/config/delete":
			return map[string]any{"result": nil}
		}
		return map[string]any{"success": false, "error": map[string]any{"message": "unexpected " + cmd["type"].(string)}}
	})

	// get_config of the default dashboard sends url_path: null.
	cfg, err := c.GetDashboardConfig("", false)
	if err != nil {
		t.Fatalf("GetDashboardConfig: %v", err)
	}
	if cfg["title"] != "Home" {
		t.Errorf("config title = %v, want Home", cfg["title"])
	}
	got := rec.last()
	if got["type"] != "lovelace/config" {
		t.Errorf("type = %v, want lovelace/config", got["type"])
	}
	if v, ok := got["url_path"]; !ok || v != nil {
		t.Errorf("url_path = %v (present=%v), want present null", v, ok)
	}
	if got["force"] != false {
		t.Errorf("force = %v, want false", got["force"])
	}

	// save_config of a named dashboard sends the url_path string and config.
	if err := c.SaveDashboardConfig("lovelace-cams", map[string]any{"views": []any{}}); err != nil {
		t.Fatalf("SaveDashboardConfig: %v", err)
	}
	got = rec.last()
	if got["url_path"] != "lovelace-cams" {
		t.Errorf("url_path = %v, want lovelace-cams", got["url_path"])
	}
	if _, ok := got["config"].(map[string]any); !ok {
		t.Errorf("config not forwarded: %v", got["config"])
	}
}

func TestDashboardCollectionCRUD(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"id": "abc123", "url_path": cmd["url_path"]}}
	})

	if _, err := c.ListDashboards(); err != nil {
		t.Fatalf("ListDashboards: %v", err)
	}
	if _, err := c.CreateDashboard(map[string]any{"url_path": "lovelace-cams", "title": "Cameras"}); err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	if _, err := c.UpdateDashboard("abc123", map[string]any{"title": "Cams"}); err != nil {
		t.Fatalf("UpdateDashboard: %v", err)
	}
	if err := c.DeleteDashboard("abc123"); err != nil {
		t.Fatalf("DeleteDashboard: %v", err)
	}

	types := []string{}
	for _, cmd := range rec.all() {
		types = append(types, cmd["type"].(string))
	}
	want := []string{
		"lovelace/dashboards/list",
		"lovelace/dashboards/create",
		"lovelace/dashboards/update",
		"lovelace/dashboards/delete",
	}
	if len(types) != len(want) {
		t.Fatalf("commands = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, types[i], want[i])
		}
	}
	// update must carry the dashboard_id alongside the changed fields.
	if rec.all()[2]["dashboard_id"] != "abc123" {
		t.Errorf("update dashboard_id = %v, want abc123", rec.all()[2]["dashboard_id"])
	}
}

func TestCreateDashboardRejectsBadURLPath(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{}}
	})
	if _, err := c.CreateDashboard(map[string]any{"url_path": "../../etc", "title": "x"}); err == nil {
		t.Error("expected validation error for path-injection url_path")
	}
	if err := c.DeleteDashboard(""); err == nil {
		t.Error("expected validation error for empty dashboard_id")
	}
	if len(rec.all()) != 0 {
		t.Errorf("no WS command should have been sent, got %v", rec.all())
	}
}

func TestDashboardResourceCRUD(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"id": "res1"}}
	})

	if _, err := c.ListDashboardResources(); err != nil {
		t.Fatalf("ListDashboardResources: %v", err)
	}
	if _, err := c.CreateDashboardResource("/local/card.js", "module"); err != nil {
		t.Fatalf("CreateDashboardResource: %v", err)
	}
	if _, err := c.UpdateDashboardResource("res1", "/local/card2.js", ""); err != nil {
		t.Fatalf("UpdateDashboardResource: %v", err)
	}
	if err := c.DeleteDashboardResource("res1"); err != nil {
		t.Fatalf("DeleteDashboardResource: %v", err)
	}

	create := rec.all()[1]
	if create["url"] != "/local/card.js" || create["res_type"] != "module" {
		t.Errorf("create fields = %v", create)
	}
	update := rec.all()[2]
	if update["url"] != "/local/card2.js" {
		t.Errorf("update url = %v, want /local/card2.js", update["url"])
	}
	if _, ok := update["res_type"]; ok {
		t.Errorf("empty res_type should be omitted, got %v", update["res_type"])
	}
}
