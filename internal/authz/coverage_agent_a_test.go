package authz

import (
	"sort"
	"testing"
)

// TestPermissionsForReturnsExactRoleSets pins the complete permission set of
// every role. PermissionsFor previously had no coverage for a known role:
// the only caller test used an unknown role, so the range body never ran.
// The exact sets matter because Authorize consults the same map; a drift
// between the two would silently widen or narrow a role.
func TestPermissionsForReturnsExactRoleSets(t *testing.T) {
	want := map[Role][]Permission{
		RoleOwner:     {ReadConfig, WriteConfig, ReadProviders, WriteProviders, ReadRouting, WriteRouting, ReadUsage, RunEvaluation, ReadAudit, ManageKeys, ManageVideo},
		RoleAdmin:     {ReadConfig, WriteConfig, ReadProviders, WriteProviders, ReadRouting, WriteRouting, ReadUsage, RunEvaluation, ReadAudit, ManageKeys, ManageVideo},
		RoleOperator:  {ReadConfig, ReadProviders, ReadRouting, WriteRouting, ReadUsage, RunEvaluation, ManageVideo},
		RoleDeveloper: {ReadConfig, ReadProviders, ReadRouting, ReadUsage, RunEvaluation, ReadAudit},
		RoleViewer:    {ReadConfig, ReadProviders, ReadRouting, ReadUsage},
	}
	for role, expected := range want {
		got := PermissionsFor(role)
		if len(got) != len(expected) {
			t.Fatalf("role %s: got %d permissions %v, want %d %v", role, len(got), got, len(expected), expected)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sorted := append([]Permission(nil), expected...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		for i := range sorted {
			if got[i] != sorted[i] {
				t.Fatalf("role %s: got %v, want %v", role, got, sorted)
			}
		}
		// The set must agree with Authorize for every known permission.
		id := Identity{Subject: "user-1", Roles: []Role{role}}
		all := []Permission{ReadConfig, WriteConfig, ReadProviders, WriteProviders, ReadRouting, WriteRouting, ReadUsage, RunEvaluation, ReadAudit, ManageKeys, ManageVideo}
		for _, p := range all {
			allowed := false
			for _, e := range expected {
				if e == p {
					allowed = true
				}
			}
			if id.Authorize(p) != allowed {
				t.Fatalf("role %s: Authorize(%s)=%v, PermissionsFor says %v", role, p, !allowed, allowed)
			}
		}
	}
}

// TestPermissionsForUnknownRoleIsEmptyAndNilSafe documents that an unknown
// role yields no permissions (and no panic on the nil map read).
func TestPermissionsForUnknownRoleIsEmptyAndNilSafe(t *testing.T) {
	if got := PermissionsFor(Role("no-such-role")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v, want empty", got)
	}
}
