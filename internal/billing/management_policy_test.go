package billing

import (
	"strings"
	"testing"
	"time"
)

func syncPolicyKeys(t *testing.T, store *Store, keys ...string) {
	t.Helper()
	if _, errSync := store.SyncKeys(keys, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
}

func policyTestPlan(t *testing.T, store *Store) Plan {
	t.Helper()
	plan, errCreate := store.CreatePlanWithBindings(Plan{
		ID: "policy-plan", Name: "Policy plan",
		Windows: []QuotaWindow{{Name: "额度", AmountUSD: 5, PeriodSeconds: 3600}},
	}, nil)
	if errCreate != nil {
		t.Fatalf("CreatePlanWithBindings error = %v", errCreate)
	}
	return plan
}

// The policy table: blank, unknown, deleted, and unbound keys are unmanaged;
// only a live key with a valid plan binding is managed.
func TestManagementPolicyStates(t *testing.T) {
	store := newStore(t)
	plan := policyTestPlan(t, store)
	const unboundKey, managedKey = "sk-policy-unbound-001", "sk-policy-managed-01"
	syncPolicyKeys(t, store, unboundKey, managedKey)
	if errBind := store.BindKey(CallerScope(managedKey), plan.ID); errBind != nil {
		t.Fatalf("BindKey error = %v", errBind)
	}

	for _, test := range []struct {
		name     string
		scope    string
		managed  bool
		external bool
	}{
		{name: "blank scope", scope: ""},
		{name: "unknown key", scope: CallerScope("sk-policy-unknown-01")},
		{name: "configured unbound key", scope: CallerScope(unboundKey)},
		{name: "configured managed key", scope: CallerScope(managedKey), managed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := store.ManagementPolicy(test.scope)
			if policy.Managed != test.managed {
				t.Fatalf("Managed = %t, want %t (scope=%q)", policy.Managed, test.managed, test.scope)
			}
			if policy.ConfigurationError != "" {
				t.Fatalf("ConfigurationError = %q, want none", policy.ConfigurationError)
			}
			if test.scope == "" {
				if policy.Scope != "" {
					t.Fatalf("Scope = %q, want blank", policy.Scope)
				}
			} else if want := normalizeScope(test.scope); policy.Scope != want {
				t.Fatalf("Scope = %q, want canonical %q", policy.Scope, want)
			}
		})
	}

	t.Run("mixed-case scope resolves the same key", func(t *testing.T) {
		scope := CallerScope(managedKey)
		policy := store.ManagementPolicy(strings.ToUpper(scope) + " ")
		if !policy.Managed || policy.Scope != scope {
			t.Fatalf("policy = %+v, want managed scope %q", policy, scope)
		}
	})

	t.Run("deleted key", func(t *testing.T) {
		syncPolicyKeys(t, store, unboundKey) // managedKey leaves the config list
		if policy := store.ManagementPolicy(CallerScope(managedKey)); policy.Managed {
			t.Fatalf("deleted key policy = %+v, want unmanaged", policy)
		}
	})

	t.Run("external principal with a valid plan", func(t *testing.T) {
		const external = "sk-policy-external-01"
		scope := CallerScope(external)
		store.ReplaceAll(func(state *State) {
			if key := state.ensureKey(scope, PreviewKey(external)); key != nil {
				key.PlanID = plan.ID
			}
		})
		policy := store.ManagementPolicy(scope)
		if !policy.Managed || policy.ConfigurationError != "" {
			t.Fatalf("external principal policy = %+v, want managed without error", policy)
		}
	})
}

// The administrator-facing status enum: deleted > external-managed > managed
// > unmanaged, following the binding and configuration state.
func TestKeyManagementStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		key  KeyState
		want ManagementStatus
	}{
		{name: "unmanaged configured key", key: KeyState{InConfig: true}, want: ManagementStatusUnmanaged},
		{name: "managed configured key", key: KeyState{InConfig: true, PlanID: "p"}, want: ManagementStatusManaged},
		{name: "external managed principal", key: KeyState{PlanID: "p"}, want: ManagementStatusExternalManaged},
		{name: "deleted bound key", key: KeyState{InConfig: true, PlanID: "p", DeletedAt: time.Unix(1, 0)}, want: ManagementStatusDeleted},
		{name: "deleted unbound key", key: KeyState{DeletedAt: time.Unix(1, 0)}, want: ManagementStatusDeleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := keyManagementStatus(&test.key); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

// An in-memory dangling plan reference stays managed and fails loudly; it never
// becomes an unmanaged pass-through.
func TestManagementPolicyReportsMissingPlan(t *testing.T) {
	store := newStore(t)
	plan := policyTestPlan(t, store)
	const key = "sk-policy-corrupt-001"
	syncPolicyKeys(t, store, key)
	if errBind := store.BindKey(CallerScope(key), plan.ID); errBind != nil {
		t.Fatalf("BindKey error = %v", errBind)
	}
	store.ReplaceAll(func(state *State) { state.Plans = nil })

	policy := store.ManagementPolicy(CallerScope(key))
	if !policy.Managed {
		t.Fatalf("policy = %+v, want the broken binding to stay managed", policy)
	}
	if policy.ConfigurationError == "" || !strings.Contains(policy.ConfigurationError, plan.ID) {
		t.Fatalf("ConfigurationError = %q, want it to name the missing plan %q", policy.ConfigurationError, plan.ID)
	}
	// The silent repair path is gone: the binding survives the lookup.
	store.Read(func(state *State) {
		if keyState := state.Keys[CallerScope(key)]; keyState == nil || keyState.PlanID != plan.ID {
			t.Fatalf("binding was cleared: %+v", keyState)
		}
	})
}

// Configuration mutations cannot publish a key bound to a missing plan.
func TestConfigurationMutationsRejectDanglingPlanBindings(t *testing.T) {
	store := newStore(t)
	plan := policyTestPlan(t, store)
	const key = "sk-policy-guard-0001"
	syncPolicyKeys(t, store, key)
	if errBind := store.BindKey(CallerScope(key), plan.ID); errBind != nil {
		t.Fatalf("BindKey error = %v", errBind)
	}

	_, err := editConfiguration(store, func(state *State) (struct{}, Changes, error) {
		state.Plans = nil
		return struct{}{}, Changes{Plans: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), plan.ID) {
		t.Fatalf("dangling binding accepted: %v", err)
	}
	store.Read(func(state *State) {
		if len(state.Plans) != 1 {
			t.Fatalf("rejected edit changed state: %+v", state.Plans)
		}
	})
}
