package hass

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestNewServicePolicy(t *testing.T) {
	if p, err := NewServicePolicy(nil); err != nil || p != nil {
		t.Errorf("NewServicePolicy(nil) = %v, %v; want nil, nil", p, err)
	}
	if p, err := NewServicePolicy([]string{" ", ""}); err != nil || p != nil {
		t.Errorf("NewServicePolicy(blank) = %v, %v; want nil, nil", p, err)
	}

	invalid := []string{"lock", "no-dot-here", "lock.un/lock", "../x.y"}
	for _, pat := range invalid {
		if _, err := NewServicePolicy([]string{pat}); err == nil {
			t.Errorf("NewServicePolicy(%q) should fail", pat)
		}
	}
}

func TestServicePolicyCheck(t *testing.T) {
	p, err := NewServicePolicy([]string{"lock.unlock", "alarm_control_panel.*", "*.delete"})
	if err != nil {
		t.Fatalf("NewServicePolicy: %v", err)
	}

	denied := [][2]string{
		{"lock", "unlock"},
		{"alarm_control_panel", "alarm_disarm"},
		{"alarm_control_panel", "anything"},
		{"automation", "delete"},
	}
	for _, d := range denied {
		if err := p.Check(d[0], d[1]); err == nil {
			t.Errorf("Check(%s, %s) = nil, want denied", d[0], d[1])
		}
	}

	allowed := [][2]string{
		{"lock", "lock"},
		{"light", "turn_on"},
		{"automation", "turn_off"},
	}
	for _, a := range allowed {
		if err := p.Check(a[0], a[1]); err != nil {
			t.Errorf("Check(%s, %s) = %v, want nil", a[0], a[1], err)
		}
	}

	// A nil policy allows everything.
	var nilPolicy *ServicePolicy
	if err := nilPolicy.Check("lock", "unlock"); err != nil {
		t.Errorf("nil policy Check = %v, want nil", err)
	}
}

func TestServicePolicyCheckSequence(t *testing.T) {
	p, err := NewServicePolicy([]string{"lock.unlock"})
	if err != nil {
		t.Fatalf("NewServicePolicy: %v", err)
	}

	// Denied service nested inside a choose block, modern "action:" syntax.
	nested := []any{
		map[string]any{
			"choose": []any{
				map[string]any{
					"conditions": []any{},
					"sequence": []any{
						map[string]any{"action": "lock.unlock", "target": map[string]any{"entity_id": "lock.front"}},
					},
				},
			},
		},
	}
	if err := p.CheckSequence(nested); err == nil {
		t.Error("CheckSequence should reject nested denied action")
	}

	// Legacy "service:" key.
	legacy := []any{map[string]any{"service": "lock.unlock"}}
	if err := p.CheckSequence(legacy); err == nil {
		t.Error("CheckSequence should reject legacy service key")
	}

	ok := []any{
		map[string]any{"action": "light.turn_on"},
		map[string]any{"delay": "00:00:05"},
	}
	if err := p.CheckSequence(ok); err != nil {
		t.Errorf("CheckSequence(allowed) = %v, want nil", err)
	}

	var nilPolicy *ServicePolicy
	if err := nilPolicy.CheckSequence(nested); err != nil {
		t.Errorf("nil policy CheckSequence = %v, want nil", err)
	}
}

func TestCallServiceEnforcesPolicy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("denied service call must not reach Home Assistant: %s", r.URL.Path)
	})
	p, err := NewServicePolicy([]string{"lock.unlock"})
	if err != nil {
		t.Fatalf("NewServicePolicy: %v", err)
	}
	c.SetServicePolicy(p)

	_, err = c.CallService(context.Background(), "lock", "unlock", map[string]any{"entity_id": "lock.front"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("CallService = %v, want denied error", err)
	}
}

func TestCallServiceAllowsUndeniedServices(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/services/light/turn_on" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[]`))
	})
	p, err := NewServicePolicy([]string{"lock.unlock"})
	if err != nil {
		t.Fatalf("NewServicePolicy: %v", err)
	}
	c.SetServicePolicy(p)

	if _, err := c.CallService(context.Background(), "light", "turn_on", nil); err != nil {
		t.Errorf("CallService(light.turn_on) = %v, want nil", err)
	}
}
