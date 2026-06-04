package hass

import "testing"

func TestExecuteScript(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{
			"context":  map[string]any{"id": "ctx1"},
			"response": map[string]any{},
		}}
	})

	seq := []any{
		map[string]any{"action": "light.turn_on", "target": map[string]any{"entity_id": "light.kitchen"}},
	}
	result, err := c.ExecuteScript(seq, map[string]any{"brightness": 200})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if _, ok := result["context"]; !ok {
		t.Errorf("result missing context: %v", result)
	}

	got := rec.last()
	if got["type"] != "execute_script" {
		t.Errorf("type = %v, want execute_script", got["type"])
	}
	if _, ok := got["sequence"].([]any); !ok {
		t.Errorf("sequence not forwarded as list: %v", got["sequence"])
	}
	if _, ok := got["variables"].(map[string]any); !ok {
		t.Errorf("variables not forwarded: %v", got["variables"])
	}
}

func TestExecuteScriptRejectsEmptySequence(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{}}
	})
	if _, err := c.ExecuteScript(nil, nil); err == nil {
		t.Error("expected error for empty sequence")
	}
	if len(rec.all()) != 0 {
		t.Errorf("no command should be sent for empty sequence, got %v", rec.all())
	}
}

func TestExecuteScriptOmitsEmptyVariables(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{}}
	})
	if _, err := c.ExecuteScript([]any{map[string]any{"delay": "00:00:01"}}, nil); err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if _, ok := rec.last()["variables"]; ok {
		t.Error("variables should be omitted when empty")
	}
}
