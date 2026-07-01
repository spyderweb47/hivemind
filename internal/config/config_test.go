package config

import "testing"

func TestEffectivePermissionModeFor(t *testing.T) {
	p := &Project{
		Defaults: Defaults{PermissionMode: "acceptEdits"},
		Agents: []Agent{
			{Name: "alice", PermissionMode: "bypassPermissions"}, // per-agent override
			{Name: "bob"}, // inherits the default
		},
	}
	if got := p.EffectivePermissionModeFor("alice"); got != "bypassPermissions" {
		t.Errorf("alice = %q, want bypassPermissions (override)", got)
	}
	if got := p.EffectivePermissionModeFor("bob"); got != "acceptEdits" {
		t.Errorf("bob = %q, want acceptEdits (default)", got)
	}
	// the supervisor (no Agent entry) uses the project default
	if got := p.EffectivePermissionModeFor(SupervisorName); got != "acceptEdits" {
		t.Errorf("supervisor = %q, want acceptEdits", got)
	}
}

func TestValidPermissionMode(t *testing.T) {
	for _, ok := range []string{"acceptEdits", "plan", "default", "bypassPermissions"} {
		if !ValidPermissionMode(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "yolo", "bypass", "acceptedits"} {
		if ValidPermissionMode(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
