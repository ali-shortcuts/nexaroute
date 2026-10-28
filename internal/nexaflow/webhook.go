package nexaflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// HandleWebhook verifies one webhook delivery and dispatches a run. The
// delivery is verified in order: trigger lookup on the currently published
// revision, secret resolution through the backend (the secret value is
// never in the definition), HMAC verification with timestamp window and
// in-memory replay cache (see WebhookVerifier), then the durable delivery
// ledger, which is authoritative across restarts. A replayed delivery is
// rejected without dispatching a run. Runs execute as ServicePrincipal;
// node permissions are still enforced per node.
//
// The webhook trigger node's input is {timestamp, signature, body}; the
// run input additionally carries body_json (the parsed body when the body
// is JSON) so downstream mappings can reference input.body_json.
func (e *Engine) HandleWebhook(ctx context.Context, definitionID, triggerNodeID, timestamp, signature string, body []byte) (*Run, error) {
	if len(body) > MaxWorkflowInputBytes {
		return nil, NewNodeError("input_too_large", false, "webhook body exceeds %d bytes", MaxWorkflowInputBytes)
	}
	def, ver, err := e.store.LatestPublished(ctx, definitionID)
	if err != nil {
		return nil, err
	}
	decl, ok := def.TriggerDecl(triggerNodeID)
	if !ok || decl.Kind != TriggerWebhook {
		return nil, NewNodeError("validation_error", false, "trigger node %q is not a webhook trigger", triggerNodeID)
	}
	if e.backend == nil {
		return nil, NewNodeError("backend_error", false, "no backend configured for webhook secret resolution")
	}
	secretRef, _ := decl.Config["secret_ref"].(string)
	if secretRef == "" {
		return nil, NewNodeError("validation_error", false, "webhook trigger %q has no secret_ref", triggerNodeID)
	}
	secret, err := e.backend.ResolveSecretRef(secretRef)
	if err != nil {
		return nil, NewNodeError("permission_denied", false, "webhook secret reference cannot be resolved")
	}
	// The engine's shared verifier provides the in-memory replay cache;
	// the durable ledger below is authoritative across restarts.
	if err := e.webhookVerifier.Verify(timestamp, signature, string(body), secret); err != nil {
		return nil, asNodeError(err)
	}
	sum := sha256.Sum256([]byte(definitionID + "\x00" + triggerNodeID + "\x00" + signature))
	hash := hex.EncodeToString(sum[:])
	first, err := e.store.RecordDelivery(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !first {
		return nil, NewNodeError("replay_rejected", false, "webhook delivery already processed")
	}
	input := map[string]any{
		"timestamp": timestamp,
		"signature": signature,
		"body":      string(body),
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		input["body_json"] = parsed
	}
	return e.StartRun(ctx, ServicePrincipal, definitionID, ver.Revision, TriggerWebhook, triggerNodeID, "webhook:"+hash, input)
}
