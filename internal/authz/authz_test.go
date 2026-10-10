package authz

import "testing"

func TestRolePermissionMatrix(t *testing.T) {
	cases := []struct {
		role  Role
		allow Permission
		deny  Permission
	}{
		{RoleOwner, ManageKeys, ManageKeys},
		{RoleAdmin, WriteConfig, Permission("controlplane.write")},
		{RoleOperator, WriteRouting, WriteConfig},
		{RoleDeveloper, RunEvaluation, WriteRouting},
		{RoleViewer, ReadUsage, WriteProviders},
	}
	for _, tc := range cases {
		id := Identity{Subject: "user-1", Roles: []Role{tc.role}}
		if !id.Authorize(tc.allow) {
			t.Errorf("role %s should allow %s", tc.role, tc.allow)
		}
		if id.Authorize(tc.deny) && tc.allow != tc.deny {
			t.Errorf("role %s should deny %s", tc.role, tc.deny)
		}
	}
}

func TestAuthorizationFailsClosedWithoutAuthentication(t *testing.T) {
	if (Identity{Roles: []Role{RoleOwner}}).Authorize(WriteConfig) {
		t.Fatal("roles without a subject must not authorize")
	}
}

func TestClaimedWildcardPermission(t *testing.T) {
	id := Identity{Subject: "sso-user", Permissions: []Permission{"video.*"}}
	if !id.Authorize(ManageVideo) {
		t.Fatal("video wildcard should authorize video.manage")
	}
	if id.Authorize(WriteConfig) {
		t.Fatal("video wildcard must not authorize config.write")
	}
}

func TestPermissionsForUnknownRoleIsEmpty(t *testing.T) {
	if got := PermissionsFor(Role("unknown")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v", got)
	}
}
