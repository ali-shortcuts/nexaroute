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

func TestOIDCRoleMatrixIsLeastPrivilege(t *testing.T) {
	cases := []struct {
		role  Role
		allow []Permission
		deny  []Permission
	}{
		{
			RoleViewer,
			[]Permission{ReadConfig, ReadProviders, ReadRouting, ReadUsage},
			[]Permission{WriteConfig, WriteProviders, WriteRouting, RunEvaluation, ReadAudit, ManageKeys, ManageVideo},
		},
		{
			RoleOperator,
			[]Permission{ReadConfig, ReadProviders, ReadRouting, ReadUsage, WriteRouting, RunEvaluation, ManageVideo},
			[]Permission{WriteConfig, WriteProviders, ReadAudit, ManageKeys},
		},
		{
			RoleAdmin,
			[]Permission{ReadConfig, WriteConfig, ReadProviders, WriteProviders, ReadRouting, WriteRouting, ReadUsage, RunEvaluation, ReadAudit, ManageKeys, ManageVideo},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			identity := Identity{Subject: "verified-subject", Issuer: "https://id.example", Roles: []Role{tc.role}}
			for _, permission := range tc.allow {
				if !identity.Authorize(permission) {
					t.Errorf("%s should allow %s", tc.role, permission)
				}
			}
			for _, permission := range tc.deny {
				if identity.Authorize(permission) {
					t.Errorf("%s must deny %s", tc.role, permission)
				}
			}
		})
	}
}

func TestAuthorizationFailsClosedWithoutAuthentication(t *testing.T) {
	if (Identity{Roles: []Role{RoleOwner}}).Authorize(WriteConfig) {
		t.Fatal("roles without a subject must not authorize")
	}
}

func TestPermissionsForUnknownRoleIsEmpty(t *testing.T) {
	if got := PermissionsFor(Role("unknown")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v", got)
	}
}
