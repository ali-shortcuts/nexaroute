// Package authz contains the transport-independent authorization contract for
// the admin and future SSO adapters. It deliberately does not parse cookies,
// API keys, OIDC tokens, or SAML assertions; adapters must first authenticate
// an identity and then call Authorize with its immutable claims.
package authz

import "strings"

type Role string

const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleOperator  Role = "operator"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

type Permission string

const (
	ReadConfig     Permission = "config.read"
	WriteConfig    Permission = "config.write"
	ReadProviders  Permission = "providers.read"
	WriteProviders Permission = "providers.write"
	ReadRouting    Permission = "routing.read"
	WriteRouting   Permission = "routing.write"
	ReadUsage      Permission = "usage.read"
	RunEvaluation  Permission = "evaluation.run"
	ReadAudit      Permission = "audit.read"
	ManageKeys     Permission = "keys.manage"
	ManageVideo    Permission = "video.manage"
)

type Identity struct {
	Subject string
	Issuer  string
	Roles   []Role
}

var rolePermissions = map[Role]map[Permission]bool{
	RoleOwner: {
		ReadConfig: true, WriteConfig: true, ReadProviders: true, WriteProviders: true,
		ReadRouting: true, WriteRouting: true, ReadUsage: true, RunEvaluation: true,
		ReadAudit: true, ManageKeys: true, ManageVideo: true,
	},
	RoleAdmin: {
		ReadConfig: true, WriteConfig: true, ReadProviders: true, WriteProviders: true,
		ReadRouting: true, WriteRouting: true, ReadUsage: true, RunEvaluation: true,
		ReadAudit: true, ManageKeys: true, ManageVideo: true,
	},
	RoleOperator: {
		ReadConfig: true, ReadProviders: true, ReadRouting: true, WriteRouting: true,
		ReadUsage: true, RunEvaluation: true, ManageVideo: true,
	},
	RoleDeveloper: {
		ReadConfig: true, ReadProviders: true, ReadRouting: true, ReadUsage: true,
		RunEvaluation: true, ReadAudit: true,
	},
	RoleViewer: {
		ReadConfig: true, ReadProviders: true, ReadRouting: true, ReadUsage: true,
	},
}

func (i Identity) Authenticated() bool { return strings.TrimSpace(i.Subject) != "" }

func (i Identity) HasRole(want Role) bool {
	for _, role := range i.Roles {
		if role == want {
			return true
		}
	}
	return false
}

func (i Identity) Authorize(permission Permission) bool {
	if !i.Authenticated() {
		return false
	}
	for _, role := range i.Roles {
		if rolePermissions[role][permission] {
			return true
		}
	}
	return false
}

func PermissionsFor(role Role) []Permission {
	set := rolePermissions[role]
	out := make([]Permission, 0, len(set))
	for permission := range set {
		out = append(out, permission)
	}
	return out
}
