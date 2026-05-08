// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import "testing"

func TestChecker_AddTurnAllow_AutoAllowsMatchingCalls(t *testing.T) {
	c := NewChecker(nil, nil, nil, "prompt")

	// Without the grant, mode=prompt → Prompt.
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet ./..."}, nil); d != Prompt {
		t.Fatalf("baseline: got %v, want Prompt", d)
	}

	// User clicks "Allow Turn" — DerivePattern produces bash(go *).
	if err := c.AddTurnAllow("bash(go *)"); err != nil {
		t.Fatal(err)
	}
	if !c.HasTurnAllows() {
		t.Error("HasTurnAllows should be true after grant")
	}
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet ./..."}, nil); d != Allow {
		t.Errorf("after grant: got %v, want Allow", d)
	}
	// A different command in the same family also matches the wildcard.
	if d, _ := c.Check("bash", map[string]any{"cmd": "go test ./..."}, nil); d != Allow {
		t.Errorf("matching wildcard: got %v, want Allow", d)
	}
}

func TestChecker_ResetTurnAllows_RestoresPrompting(t *testing.T) {
	c := NewChecker(nil, nil, nil, "prompt")
	if err := c.AddTurnAllow("bash(go *)"); err != nil {
		t.Fatal(err)
	}

	c.ResetTurnAllows()
	if c.HasTurnAllows() {
		t.Error("HasTurnAllows should be false after reset")
	}
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet"}, nil); d != Prompt {
		t.Errorf("after reset: got %v, want Prompt (grant should not survive)", d)
	}
}

func TestChecker_TurnAllowDoesNotOverrideDeny(t *testing.T) {
	// A `deny` rule must still win over an explicit "Allow Turn" grant.
	// This locks in the security property: a misclick on "Allow Turn"
	// can't grant access to something the user (or admin) has
	// explicitly denied.
	c := NewChecker(nil, nil, []string{"bash(rm *)"}, "prompt")
	if err := c.AddTurnAllow("bash(rm *)"); err != nil {
		t.Fatal(err)
	}
	d, _ := c.Check("bash", map[string]any{"cmd": "rm -rf /tmp/x"}, nil)
	if d != Deny {
		t.Errorf("got %v, want Deny — turn-allow must not override deny rule", d)
	}
}

func TestChecker_TurnAllowOverridesAsk(t *testing.T) {
	// `ask` rules say "always confirm by default". An explicit "Allow
	// Turn" grant means the user just confirmed for the rest of this
	// turn, so the ask rule should stop firing for matching calls.
	c := NewChecker(nil, []string{"bash(go *)"}, nil, "prompt")
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet"}, nil); d != Prompt {
		t.Fatalf("ask baseline: got %v, want Prompt", d)
	}
	if err := c.AddTurnAllow("bash(go *)"); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet"}, nil); d != Allow {
		t.Errorf("after grant: got %v, want Allow (turn grant should silence ask)", d)
	}
}

func TestChecker_ResetTurnAllowsPreservesPersistentRules(t *testing.T) {
	// Critical: ResetTurnAllows must clear ONLY turn-scoped grants.
	// Persistent allow/ask/deny rules survive untouched.
	c := NewChecker([]string{"read(**)"}, []string{"bash(go *)"}, []string{"bash(rm *)"}, "prompt")
	_ = c.AddTurnAllow("bash(grep *)")

	c.ResetTurnAllows()

	// Persistent allow still works.
	if d, _ := c.Check("read", map[string]any{"path": "/tmp/x"}, nil); d != Allow {
		t.Errorf("persistent allow lost: got %v, want Allow", d)
	}
	// Persistent ask still works.
	if d, _ := c.Check("bash", map[string]any{"cmd": "go vet"}, nil); d != Prompt {
		t.Errorf("persistent ask lost: got %v, want Prompt", d)
	}
	// Persistent deny still works.
	if d, _ := c.Check("bash", map[string]any{"cmd": "rm -rf"}, nil); d != Deny {
		t.Errorf("persistent deny lost: got %v, want Deny", d)
	}
	// Reset grant is gone.
	if d, _ := c.Check("bash", map[string]any{"cmd": "grep foo"}, nil); d != Prompt {
		t.Errorf("turn grant survived reset: got %v, want Prompt", d)
	}
}

func TestChecker_AddTurnAllow_RejectsInvalidPattern(t *testing.T) {
	c := NewChecker(nil, nil, nil, "prompt")
	if err := c.AddTurnAllow("not a valid pattern"); err == nil {
		t.Error("expected error on malformed pattern")
	}
	if c.HasTurnAllows() {
		t.Error("invalid pattern must not register as a grant")
	}
}

func TestChecker_HasTurnAllows_FreshCheckerIsFalse(t *testing.T) {
	c := NewChecker(nil, nil, nil, "prompt")
	if c.HasTurnAllows() {
		t.Error("fresh checker should have no turn grants")
	}
}
