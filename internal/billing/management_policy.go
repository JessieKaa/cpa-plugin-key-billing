package billing

import "fmt"

// ManagementStatus is the administrator-facing state of a key. Precedence:
// deleted, then managed outside CPA's configured key list (external), then
// managed, then unmanaged. An unmanaged key bypasses plugin enforcement and
// accounting entirely.
type ManagementStatus string

const (
	ManagementStatusDeleted         ManagementStatus = "deleted"
	ManagementStatusExternalManaged ManagementStatus = "external-managed"
	ManagementStatusManaged         ManagementStatus = "managed"
	ManagementStatusUnmanaged       ManagementStatus = "unmanaged"
)

func keyManagementStatus(key *KeyState) ManagementStatus {
	switch {
	case key == nil || key.PlanID == "":
		if key != nil && !key.DeletedAt.IsZero() {
			return ManagementStatusDeleted
		}
		return ManagementStatusUnmanaged
	case !key.DeletedAt.IsZero():
		return ManagementStatusDeleted
	case !key.InConfig:
		return ManagementStatusExternalManaged
	default:
		return ManagementStatusManaged
	}
}

// ManagementPolicy reports whether one caller scope currently participates in
// plugin enforcement and accounting. Plan binding is the opt-in marker: a key
// without a bound subscription plan is unmanaged and bypasses the plugin
// entirely.
type ManagementPolicy struct {
	// Scope is the canonical normalized caller scope. Every downstream lookup —
	// routing, scheduling, concurrency, quota, quota-block logging, and event
	// attribution — uses it, so mixed-case or padded input cannot split one key
	// into two accounting identities.
	Scope string
	// Managed reports whether plugin enforcement applies to this scope for the
	// callback that resolved the policy.
	Managed bool
	// ConfigurationError is set when a managed binding references state that
	// should not exist. Admission terminates with a controlled 503 instead of
	// silently unbinding the key.
	ConfigurationError string
}

// ManagementPolicy evaluates one state snapshot under a read lock. Each
// callback resolves the key's current binding independently: binding or
// unbinding takes effect on subsequent callbacks, and an in-flight request may
// observe mixed pre- and post-change state.
func (s *Store) ManagementPolicy(scope string) ManagementPolicy {
	scope = normalizeScope(scope)
	if scope == "" {
		return ManagementPolicy{}
	}
	var policy ManagementPolicy
	s.read(func(state *State) {
		policy.Scope = scope
		key := state.Keys[scope]
		// Blank, unknown, deleted, and unbound keys are unmanaged. A principal
		// without a deletion timestamp keeps its binding even when it was never
		// in CPA's configured key list.
		if key == nil || !key.DeletedAt.IsZero() || key.PlanID == "" {
			return
		}
		policy.Managed = true
		if _, exists := state.FindPlan(key.PlanID); !exists {
			// SQLite load validation rejects a dangling plan reference
			// outright, so reaching this branch means in-memory corruption.
			// Keep the key managed and fail admission loudly; never unbind
			// silently, which would turn corruption into free traffic.
			policy.ConfigurationError = fmt.Sprintf(
				"The subscription plan %q bound to this API key does not exist", key.PlanID)
		}
	})
	return policy
}

// validateManagedBindings rejects any configuration edit that would leave a
// key bound to a plan that does not exist. Load-time validation makes that
// state unreachable through the database; this guard keeps future mutations
// from turning a broken managed binding into an unmanaged pass-through.
func (s *State) validateManagedBindings() error {
	for scope, key := range s.Keys {
		if key == nil || key.PlanID == "" {
			continue
		}
		if _, exists := s.FindPlan(key.PlanID); !exists {
			return invalidf("API key %s is bound to subscription plan %q, which does not exist", scope, key.PlanID)
		}
	}
	return nil
}
