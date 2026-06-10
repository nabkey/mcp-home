package hass

import (
	"fmt"
	"strings"

	"github.com/nabkey/mcp-home/internal/validate"
)

// ServicePolicy refuses calls to denied Home Assistant services. Patterns are
// "domain.service" pairs where either part may be "*" (e.g. "lock.unlock",
// "alarm_control_panel.*", "*.delete").
//
// The policy guards direct service calls (call_home_service, scene
// activation, to-do list changes) and ad-hoc execute_script sequences. It is
// a guardrail against unwanted assistant actions, not a security boundary:
// stored automations and scripts run inside Home Assistant and are not
// inspected.
type ServicePolicy struct {
	deny []servicePattern
}

type servicePattern struct {
	domain  string // "*" matches any domain
	service string // "*" matches any service
}

// NewServicePolicy parses deny patterns into a policy. A nil or empty pattern
// list yields a nil policy, which allows everything.
func NewServicePolicy(denyPatterns []string) (*ServicePolicy, error) {
	if len(denyPatterns) == 0 {
		return nil, nil
	}
	p := &ServicePolicy{}
	for _, raw := range denyPatterns {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		domain, service, ok := strings.Cut(raw, ".")
		if !ok {
			return nil, fmt.Errorf("invalid deny pattern %q: want domain.service (either part may be *)", raw)
		}
		for _, part := range []string{domain, service} {
			if part == "*" {
				continue
			}
			if err := validate.Identifier("deny pattern part", part); err != nil {
				return nil, fmt.Errorf("invalid deny pattern %q: %w", raw, err)
			}
		}
		p.deny = append(p.deny, servicePattern{domain: domain, service: service})
	}
	if len(p.deny) == 0 {
		return nil, nil
	}
	return p, nil
}

// Check returns an error if calling domain.service is denied. A nil policy
// allows everything.
func (p *ServicePolicy) Check(domain, service string) error {
	if p == nil {
		return nil
	}
	for _, pat := range p.deny {
		if (pat.domain == "*" || pat.domain == domain) && (pat.service == "*" || pat.service == service) {
			return fmt.Errorf("service %s.%s is denied by HASS_DENY_SERVICES", domain, service)
		}
	}
	return nil
}

// CheckSequence walks a Home Assistant action sequence (as used by
// execute_script) and rejects it if any step invokes a denied service. Both
// the modern "action:" and legacy "service:" step keys are inspected,
// recursively, so nested choose/if/repeat blocks are covered.
func (p *ServicePolicy) CheckSequence(sequence any) error {
	if p == nil {
		return nil
	}
	switch v := sequence.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "action" || key == "service" {
				if s, ok := val.(string); ok {
					if domain, service, found := strings.Cut(s, "."); found {
						if err := p.Check(domain, service); err != nil {
							return err
						}
					}
				}
			}
			if err := p.CheckSequence(val); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := p.CheckSequence(item); err != nil {
				return err
			}
		}
	}
	return nil
}
