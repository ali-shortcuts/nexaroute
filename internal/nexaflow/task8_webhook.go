package nexaflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Task 8 webhook service: signature verification plus a DURABLE
// delivery ledger. The #689 WebhookVerifier alone is not enough: its
// replay cache is in-memory, so a replay after a gateway restart was
// accepted. This service verifies the signature (timestamp window +
// HMAC over timestamp + "." + body, same scheme as the verifier) and
// then claims the delivery in the ledger before starting anything. A
// replayed, stale, malformed, or wrongly signed delivery never reaches
// the start function, so it can never create a second run.
//
// Ledger key: SHA-256 over definition + trigger + signature. The
// signature already binds timestamp and body (the verifier MACs both),
// so the identical delivery always maps to the same key, while the
// same body delivered to a different trigger is a different key.

var (
	ErrWebhookReplay = errors.New("nexaflow: webhook delivery already seen")
	ErrWebhookAuth   = errors.New("nexaflow: webhook signature invalid")
)

// WebhookService verifies and de-duplicates webhook deliveries.
type WebhookService struct {
	Verifier *WebhookVerifier
	Ledger   DeliveryLedger
	// Start creates the run for an accepted delivery and returns its id.
	Start func(ctx context.Context, definitionID, triggerNodeID string, body []byte) (string, error)
}

func NewWebhookService(verifier *WebhookVerifier, ledger DeliveryLedger, start func(ctx context.Context, definitionID, triggerNodeID string, body []byte) (string, error)) (*WebhookService, error) {
	if verifier == nil {
		verifier = NewWebhookVerifier()
	}
	if ledger == nil {
		return nil, errors.New("nexaflow: webhook service requires a delivery ledger")
	}
	if start == nil {
		return nil, errors.New("nexaflow: webhook service requires a start function")
	}
	return &WebhookService{Verifier: verifier, Ledger: ledger, Start: start}, nil
}

// Handle verifies one delivery and, only on success, starts its run.
// Malformed timestamps, stale timestamps, bad signatures, and replays
// all return errors and never call Start.
func (s *WebhookService) Handle(ctx context.Context, definitionID, triggerNodeID, timestamp, signature, body, secret string) (string, error) {
	if definitionID == "" || triggerNodeID == "" {
		return "", fmt.Errorf("nexaflow: webhook delivery without definition or trigger")
	}
	if err := s.Verifier.VerifySignature(timestamp, signature, body, secret); err != nil {
		return "", fmt.Errorf("%w: %v", ErrWebhookAuth, err)
	}
	sum := sha256.Sum256([]byte(definitionID + "\x00" + triggerNodeID + "\x00" + signature))
	key := "webhook:" + hex.EncodeToString(sum[:])
	claimed, err := s.Ledger.Claim(ctx, key)
	if err != nil {
		return "", fmt.Errorf("nexaflow: webhook delivery claim: %w", err)
	}
	if !claimed {
		return "", ErrWebhookReplay
	}
	return s.Start(ctx, definitionID, triggerNodeID, []byte(body))
}
