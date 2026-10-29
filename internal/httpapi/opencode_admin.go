package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// singleCredentialProvider builds a test-only provider copy that uses
// exactly one credential: the primary key when idx is nil, or pool
// entry idx otherwise. The copy is never persisted. Out-of-range is an
// error; a disabled entry is an error (tests never silently enable a
// credential the operator disabled). The returned name is the entry's
// configured name, for per-(model, credential) test reporting.
func singleCredentialProvider(p config.ProviderConfig, idx *int) (config.ProviderConfig, string, error) {
	if idx == nil {
		return p, "primary", nil
	}
	if *idx < 0 || *idx >= len(p.Credentials) {
		return p, "", fmt.Errorf("credential index %d out of range", *idx)
	}
	entry := p.Credentials[*idx]
	if !entry.Enabled {
		return p, "", fmt.Errorf("credential %d is disabled", *idx)
	}
	out := p
	out.APIKey = ""
	out.APIKeyEnv = ""
	out.Credentials = []config.CredentialConfig{entry}
	return out, entry.Name, nil
}

// auditProviderMutation records one audit event per provider mutation
// naming the provider id and the pool indices changed, never values.
// Best-effort: when no audit store is configured (tests, minimal
// deployments) the mutation still succeeds and the missing event is
// simply not recorded.
func (s *Server) auditProviderMutation(r *http.Request, action, providerID string, poolIndices []int) {
	identity := adminIdentityFromRequest(r)
	actor := identity.Subject
	if actor == "" {
		actor = "anonymous"
	}
	event := auditEvent(r, actor, action, "success", http.StatusOK, "admin", authz.ManageKeys)
	target := "provider:" + providerID
	if len(poolIndices) > 0 {
		parts := make([]string, 0, len(poolIndices))
		for _, i := range poolIndices {
			parts = append(parts, fmt.Sprintf("%d", i))
		}
		target += " credentials:" + strings.Join(parts, ",")
	}
	event.Target = target
	_ = s.recordAudit(event)
}
