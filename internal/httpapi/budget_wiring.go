package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/pricing"
)

// Budget admission wiring (M5-6). Virtual keys with budget_usd > 0
// get an atomic reserve at admission (clientAuthAllowed, beside
// the TPM reserve): the conservative estimate is priced against
// the MOST EXPENSIVE deployment serving the requested model, so a
// request is only admitted when its worst-case cost fits the
// remaining budget. The first usage report settles the reservation
// to the actual cost (recordUsageEx); cache hits release it (no
// upstream spend); anything unsettled expires into
// committed-at-estimate after the ledger TTL.

type budgetReservation struct {
	keyID string
	id    uint64
}

type budgetReservationContextKey struct{}

func contextWithBudgetReservation(ctx context.Context, res *budgetReservation) context.Context {
	return context.WithValue(ctx, budgetReservationContextKey{}, res)
}

func budgetReservationFromContext(ctx context.Context) *budgetReservation {
	res, _ := ctx.Value(budgetReservationContextKey{}).(*budgetReservation)
	return res
}

// requestBudgetParts estimates (inputTokens, outputAllowance) from
// the request body prefix, mirroring requestTokenEstimate's
// fail-closed posture: an unparsable or oversized body yields a
// huge estimate, so budgeted keys are rejected rather than
// admitted unpriced.
func requestBudgetParts(r *http.Request) (int64, int64) {
	if r == nil || r.Body == nil {
		return 1, 4096
	}
	prefix, truncated, err := readRequestPrefix(r, requestMetadataReadLimit)
	if err != nil || truncated {
		return 1 << 30, 1 << 30
	}
	output := int64(0)
	for _, field := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		v, ok := requestTopLevelMaxInt(prefix, field)
		if !ok {
			return 1 << 30, 1 << 30
		}
		if int64(v) > output {
			output = int64(v)
		}
	}
	if output <= 0 {
		output = 4096 // the gateway's own default output budget
	}
	inputBytes := len(prefix)
	if r.ContentLength >= 0 && r.ContentLength > int64(inputBytes) {
		if r.ContentLength > int64(1<<30) {
			inputBytes = 1 << 30
		} else {
			inputBytes = int(r.ContentLength)
		}
	}
	return int64(inputBytes / 4), output
}

// budgetEstimateFor prices the admission estimate against the most
// expensive deployment serving `model` under the snapshot's pre-built price
// book (C1: one book per config revision, not one per admission). ok=false
// when no deployment serves the model (routing will reject the request on
// its own; no budget hold is taken).
func (s *Server) budgetEstimateFor(snap *configSnapshot, model string, inputEst, outputAllowance int64) (float64, bool) {
	cfg := snap.cfg
	book := snap.book
	now := time.Now()
	found := false
	var worst float64
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			serves := m.ID == model || m.Model == model
			if !serves {
				for _, a := range m.Aliases {
					if a == model {
						serves = true
						break
					}
				}
			}
			if !serves {
				continue
			}
			found = true
			entry, _ := book.ResolveFor(cfg, p.ID+"/"+m.ID, now)
			if est := pricing.Estimate(entry, inputEst, outputAllowance); est > worst {
				worst = est
			}
		}
	}
	return worst, found
}

// budgetRelease drops the request's budget hold without committing
// (cache hits and other zero-upstream-spend serves).
func (s *Server) budgetRelease(r *http.Request) {
	if res := budgetReservationFromContext(r.Context()); res != nil && s.budgets != nil {
		s.budgets.Release(res.keyID, res.id)
	}
}

// recordCost prices one usage report under the version active NOW
// (record time) for the serving deployment, using the snapshot's
// pre-built price book (C1).
func (s *Server) recordCost(snap *configSnapshot, deploymentID string, input, output, cacheRead, cacheWrite int64, inputIncludesCache bool) (float64, string) {
	entry, version := snap.book.ResolveFor(snap.cfg, deploymentID, time.Now())
	return pricing.Cost(entry, input, output, cacheRead, cacheWrite, inputIncludesCache), version
}

// budgetModelServed reports whether any enabled deployment serves
// the model (keeps budget admission from inventing holds for
// requests routing will reject anyway).
func budgetModelServed(cfg config.Config, model string) bool {
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			if m.ID == model || m.Model == model || strings.TrimSpace(model) == "" {
				return true
			}
			for _, a := range m.Aliases {
				if a == model {
					return true
				}
			}
		}
	}
	return false
}
