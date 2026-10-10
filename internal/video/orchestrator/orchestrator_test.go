package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type fakeProvider struct {
	id           string
	caps         video.Capabilities
	capsErr      error
	validateErr  error
	estimate     video.CostEstimate
	estimateErr  error
	submit       video.ProviderJob
	submitErr    error
	statuses     []video.ProviderJobStatus
	statusErrs   []error
	cancelErr    error
	downloaded   []video.Asset
	downloadErr  error
	createCalls  int
	statusCalls  int
	cancelCalls  int
	lastRequest  video.VideoRequest
	lastDownload video.ProviderJobStatus
}

func (p *fakeProvider) ID() string { return p.id }
func (p *fakeProvider) Capabilities(context.Context) (video.Capabilities, error) {
	return p.caps, p.capsErr
}
func (p *fakeProvider) ValidateRequest(r video.VideoRequest) error {
	p.lastRequest = r
	return p.validateErr
}
func (p *fakeProvider) EstimateCost(video.VideoRequest) (video.CostEstimate, error) {
	return p.estimate, p.estimateErr
}
func (p *fakeProvider) CreateJob(context.Context, video.VideoRequest) (video.ProviderJob, error) {
	p.createCalls++
	if p.submit.ProviderJobID == "" && p.submitErr == nil {
		p.submit = video.ProviderJob{ProviderJobID: "remote-1", Provider: p.id, Model: "model-a", Accepted: true}
	}
	return p.submit, p.submitErr
}
func (p *fakeProvider) GetJob(context.Context, string) (video.ProviderJobStatus, error) {
	i := p.statusCalls
	p.statusCalls++
	if i < len(p.statusErrs) && p.statusErrs[i] != nil {
		return video.ProviderJobStatus{}, p.statusErrs[i]
	}
	if len(p.statuses) == 0 {
		return video.ProviderJobStatus{}, errors.New("no fake status configured")
	}
	if i >= len(p.statuses) {
		i = len(p.statuses) - 1
	}
	return p.statuses[i], nil
}
func (p *fakeProvider) CancelJob(context.Context, string) error { p.cancelCalls++; return p.cancelErr }
func (p *fakeProvider) DownloadOutputs(_ context.Context, st video.ProviderJobStatus, _ video.AssetSink) ([]video.Asset, error) {
	p.lastDownload = st
	return p.downloaded, p.downloadErr
}
func (p *fakeProvider) RegisterWebhook(context.Context, video.WebhookRegistration) error { return nil }
func (p *fakeProvider) VerifyWebhook(*http.Request) error                                { return nil }
func (p *fakeProvider) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, nil
}

// http.Request cannot be abbreviated in a method signature, so keep interface conformance explicit.
var _ video.VideoProvider = (*fakeProvider)(nil)

// requestWithDefaults is valid against provider caps after Normalized adds defaults.
func requestWithDefaults() video.VideoRequest {
	return video.VideoRequest{ProjectID: "project", EpisodeID: "episode", Prompt: "a scene", DurationSeconds: 2, Mode: video.ModeTextToVideo, ProviderPreference: "fake"}
}
func providerWithDefaults() *fakeProvider {
	return &fakeProvider{id: "fake", caps: video.Capabilities{Modes: []video.Mode{video.ModeTextToVideo}, Models: []string{"model-a"}, MaxDurationSeconds: 10, AspectRatios: []video.AspectRatio{video.Aspect16x9}, SupportsAudio: true}, estimate: video.CostEstimate{Provider: "fake", Model: "model-a", Currency: "USD", EstimatedUSD: 2, UpperBoundUSD: 3, PriceKnown: true}}
}
func validStoredAsset() video.Asset {
	return video.Asset{URI: "/tmp/output.mp4", ContentType: "video/mp4", SizeBytes: 10, SHA256: strings.Repeat("a", 64), Filename: "output.mp4"}
}

func TestEstimateAndProviderValidationRejectUnsafeInputs(t *testing.T) {
	for name, estimate := range map[string]video.CostEstimate{
		"unknown":              {Currency: "USD", EstimatedUSD: 1, UpperBoundUSD: 1},
		"negative":             {Currency: "USD", EstimatedUSD: -1, UpperBoundUSD: 1, PriceKnown: true},
		"upper below estimate": {Currency: "USD", EstimatedUSD: 2, UpperBoundUSD: 1, PriceKnown: true},
		"wrong currency":       {Currency: "EUR", EstimatedUSD: 1, UpperBoundUSD: 1, PriceKnown: true},
		"nan":                  {Currency: "USD", EstimatedUSD: math.NaN(), UpperBoundUSD: 1, PriceKnown: true},
		"inf":                  {Currency: "USD", EstimatedUSD: 1, UpperBoundUSD: math.Inf(1), PriceKnown: true},
	} {
		if validEstimate(estimate) {
			t.Errorf("%s estimate accepted: %#v", name, estimate)
		}
	}
	if !validEstimate(video.CostEstimate{Currency: " usd ", EstimatedUSD: 0, UpperBoundUSD: 0, PriceKnown: true}) {
		t.Fatal("zero USD estimate rejected")
	}
	base := requestWithDefaults().Normalized()
	validCaps := providerWithDefaults().caps
	cases := []struct {
		name string
		caps video.Capabilities
		r    video.VideoRequest
	}{
		{"mode", video.Capabilities{MaxDurationSeconds: 2, Models: []string{"model-a"}, AspectRatios: []video.AspectRatio{video.Aspect16x9}}, base},
		{"max duration", validCaps, func() video.VideoRequest { r := base; r.DurationSeconds = 11; return r }()},
		{"missing max", func() video.Capabilities { c := validCaps; c.MaxDurationSeconds = 0; return c }(), base},
		{"missing models", func() video.Capabilities { c := validCaps; c.Models = nil; return c }(), func() video.VideoRequest { r := base; r.ModelPreference = ""; return r }()},
		{"unknown model", validCaps, func() video.VideoRequest { r := base; r.ModelPreference = "other"; return r }()},
		{"missing ratios", func() video.Capabilities { c := validCaps; c.AspectRatios = nil; return c }(), base},
		{"unknown ratio", validCaps, func() video.VideoRequest { r := base; r.AspectRatio = video.Aspect1x1; return r }()},
		{"audio", func() video.Capabilities { c := validCaps; c.SupportsAudio = false; return c }(), func() video.VideoRequest { r := base; r.AudioRequested = true; return r }()},
		{"dialogue", func() video.Capabilities { c := validCaps; c.SupportsAudio = false; return c }(), func() video.VideoRequest { r := base; r.DialogueRequested = true; return r }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateProviderRequest(tc.r, tc.caps); err == nil {
				t.Fatal("invalid request/capabilities accepted")
			}
		})
	}
	selected, err := validateProviderRequest(func() video.VideoRequest { r := base; r.ModelPreference = ""; return r }(), validCaps)
	if err != nil || selected.ModelPreference != "model-a" {
		t.Fatalf("default model selection=%#v err=%v", selected, err)
	}
	if _, err := validateProviderRequest(base, func() video.Capabilities { c := validCaps; c.MaxDurationSeconds = math.NaN(); return c }()); err == nil {
		t.Fatal("NaN provider duration accepted")
	}
}

func TestCreateDryRunIdempotencyAndAdmissionFailures(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	st, q := queue.NewMemoryStore(), queue.New(2)
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}}
	firstReq := requestWithDefaults()
	firstReq.IdempotencyKey = "create-key"
	created, err := o.Create(ctx, firstReq)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != video.StateQueued || created.Model != "model-a" || created.EstimatedCostUSD != 2 {
		t.Fatalf("created job=%#v", created)
	}
	again := requestWithDefaults()
	again.IdempotencyKey = created.IdempotencyKey
	got, err := o.Create(ctx, again)
	if err != nil || got.JobID != created.JobID {
		t.Fatalf("idempotent create=%#v err=%v", got, err)
	}
	if p.createCalls != 0 {
		t.Fatal("Create submitted to provider")
	}

	dry := providerWithDefaults()
	dryStore := queue.NewMemoryStore()
	dryOrch := &Orchestrator{Store: dryStore, Queue: queue.New(1), Providers: Registry{"fake": dry}}
	dryReq := requestWithDefaults()
	dryReq.DryRun = true
	dryJob, err := dryOrch.Create(ctx, dryReq)
	if err != nil || dryJob.State != video.StateDryRun || dryOrch.Queue.Len() != 0 {
		t.Fatalf("dry run job=%#v err=%v queue=%d", dryJob, err, dryOrch.Queue.Len())
	}

	fullP := providerWithDefaults()
	fullQ := queue.New(1)
	if err := fullQ.Enqueue(ctx, queueJobForTest("already-full")); err != nil {
		t.Fatal(err)
	}
	fullStore := queue.NewMemoryStore()
	fullOrch := &Orchestrator{Store: fullStore, Queue: fullQ, Providers: Registry{"fake": fullP}, Ledger: &cost.Ledger{}}
	fullReq := requestWithDefaults()
	fullReq.IdempotencyKey = "full"
	if _, err := fullOrch.Create(ctx, fullReq); err == nil || !strings.Contains(err.Error(), "queue admission failed") {
		t.Fatalf("full queue error=%v", err)
	}
	failed, err := fullStore.GetByIdempotency(ctx, "full")
	if err != nil || failed.State != video.StateFailed || failed.LastError != "queue admission failed" {
		t.Fatalf("failed admission was not durable: %#v err=%v", failed, err)
	}
	if fullOrch.Ledger.Reserved() != 0 {
		t.Fatal("reservation leaked after queue admission failure")
	}
}

func TestCreateRejectsConfigurationProviderAndEstimateErrors(t *testing.T) {
	ctx := context.Background()
	r := requestWithDefaults()
	if _, err := (&Orchestrator{}).Create(ctx, r); err == nil {
		t.Fatal("unconfigured orchestrator accepted request")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (&Orchestrator{}).Create(cancelled, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Create error=%v", err)
	}
	p := providerWithDefaults()
	p.capsErr = errors.New("capability outage")
	if _, err := (&Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(1), Providers: Registry{"fake": p}}).Create(ctx, r); err == nil || !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("capability error=%v", err)
	}
	p = providerWithDefaults()
	p.validateErr = errors.New("provider validation")
	if _, err := (&Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(1), Providers: Registry{"fake": p}}).Create(ctx, r); err == nil || !strings.Contains(err.Error(), "provider rejected") {
		t.Fatalf("provider validation error=%v", err)
	}
	for name, mutate := range map[string]func(*fakeProvider){
		"missing provider":  func(p *fakeProvider) { p.id = "other" },
		"estimate error":    func(p *fakeProvider) { p.estimateErr = errors.New("pricing outage") },
		"estimate provider": func(p *fakeProvider) { p.estimate.Provider = "other" },
		"estimate model":    func(p *fakeProvider) { p.estimate.Model = "other" },
		"invalid estimate":  func(p *fakeProvider) { p.estimate.Currency = "EUR" },
	} {
		t.Run(name, func(t *testing.T) {
			p := providerWithDefaults()
			if name == "missing provider" {
				o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(1), Providers: Registry{"fake": p}}
				bad := r
				bad.ProviderPreference = "missing"
				if _, err := o.Create(ctx, bad); err == nil {
					t.Fatal("missing provider accepted")
				}
				return
			}
			mutate(p)
			o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(1), Providers: Registry{"fake": p}}
			if _, err := o.Create(ctx, r); err == nil {
				t.Fatal("invalid provider response accepted")
			}
		})
	}
	noLedger := providerWithDefaults()
	noLedgerReq := r
	noLedgerReq.MaxCostUSD = 1
	if _, err := (&Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(1), Providers: Registry{"fake": noLedger}}).Create(ctx, noLedgerReq); err == nil || !strings.Contains(err.Error(), "cost ledger") {
		t.Fatalf("max-cost without ledger error=%v", err)
	}
}

func queueJobForTest(id string) video.VideoJob {
	j := video.NewJob(id, requestWithDefaults())
	j.Provider = "fake"
	return j
}

func TestRecoverResumesSafeStatesAndFencesAmbiguousStages(t *testing.T) {
	ctx := context.Background()
	st, q := queue.NewMemoryStore(), queue.New(10)
	p := providerWithDefaults()
	jobs := []video.VideoJob{
		queueJobForTest("queued"),
		queueJobForTest("admitted-with-id"),
		queueJobForTest("admitted-unknown"),
		queueJobForTest("active"),
		queueJobForTest("active-unknown"),
		queueJobForTest("planning"),
		queueJobForTest("completed"),
		queueJobForTest("dry"),
	}
	jobs[1].State, jobs[1].ProviderJobID = video.StateAdmitted, "remote-admitted"
	jobs[2].State = video.StateAdmitted
	jobs[3].State, jobs[3].ProviderJobID = video.StateProcessing, "remote-active"
	jobs[4].State = video.StatePolling
	jobs[5].State = video.StatePlanning
	jobs[6].State = video.StateCompleted
	jobs[7].State, jobs[7].Request.DryRun = video.StateDryRun, true
	for _, j := range jobs {
		if err := st.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 3 {
		t.Fatalf("recovery queue length=%d want 3", q.Len())
	}
	for _, id := range []string{"admitted-unknown", "active-unknown", "planning"} {
		j, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != video.StateNeedsManualAction || j.LastError == "" {
			t.Fatalf("%s not fenced: %#v", id, j)
		}
	}
	admitted, _ := st.Get(ctx, "admitted-with-id")
	if admitted.State != video.StateSubmitted {
		t.Fatalf("admitted state=%s", admitted.State)
	}
	if p.createCalls != 0 {
		t.Fatal("recovery resubmitted provider work")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	blocked := queue.New(1)
	if err := blocked.Enqueue(ctx, queueJobForTest("block")); err != nil {
		t.Fatal(err)
	}
	queued := queue.NewMemoryStore()
	j := queueJobForTest("recover-full")
	if err := queued.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: queued, Queue: blocked, Providers: Registry{"fake": p}}).Recover(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("backpressure cancellation error=%v", err)
	}
}

func TestRunJobSubmissionOutcomeAndCompletion(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote-42", Provider: "fake", Model: "model-a", Accepted: true}
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-42", State: video.StateProcessing, Progress: .3, Usage: map[string]string{"frames": "3"}}, {ProviderJobID: "remote-42", State: video.StateCompleted, Progress: 1, Usage: map[string]string{"actual_cost_usd": "1.25"}, Outputs: []video.Asset{{ID: "out", Filename: "out.mp4"}}}}
	p.downloaded = []video.Asset{validStoredAsset()}
	st, q := queue.NewMemoryStore(), queue.New(1)
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: &cost.Ledger{}, PollInterval: time.Nanosecond, MaxPolls: 3}
	created, err := o.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted || got.Progress != 1 || got.ActualCostUSD != 1.25 || len(got.OutputAssets) != 1 {
		t.Fatalf("completion=%#v", got)
	}
	if got.UsageMetadata["frames"] != "3" || o.Ledger.Reserved() != 0 || o.Ledger.Spent() != 1.25 {
		t.Fatalf("usage/ledger not settled: %#v reserved=%v spent=%v", got.UsageMetadata, o.Ledger.Reserved(), o.Ledger.Spent())
	}
	if p.lastDownload.Outputs[0].Metadata["project_id"] != "project" || p.lastDownload.Outputs[0].Metadata["job_id"] != created.JobID {
		t.Fatalf("download metadata=%#v", p.lastDownload.Outputs[0].Metadata)
	}

	for name, submit := range map[string]video.ProviderJob{
		"empty id":     {Accepted: true, Provider: "fake", Model: "model-a"},
		"inconsistent": {ProviderJobID: "remote", Accepted: false, Provider: "other", Model: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			fp := providerWithDefaults()
			fp.submit = submit
			store := queue.NewMemoryStore()
			orch := &Orchestrator{Store: store, Providers: Registry{"fake": fp}}
			j := queueJobForTest(name)
			j.Provider = "fake"
			if err := store.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			err := orch.RunJob(ctx, j)
			if err == nil {
				t.Fatal("bad submission accepted")
			}
			got, _ := store.Get(ctx, name)
			if got.State != video.StateNeedsManualAction {
				t.Fatalf("state=%s error=%q", got.State, got.LastError)
			}
		})
	}
}

type testSink struct{}

func (testSink) Put(context.Context, video.Asset, []byte) (video.Asset, error) {
	return validStoredAsset(), nil
}

func TestRunJobErrorStatesCancellationAndSafeNoops(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*fakeProvider){
		"provider failure": func(p *fakeProvider) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateFailed, Progress: .2}}
		},
		"cancelled status": func(p *fakeProvider) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCancelled, Progress: 0}}
		},
		"bad progress": func(p *fakeProvider) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: 2}}
		},
		"different remote id": func(p *fakeProvider) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "other", State: video.StateProcessing, Progress: .1}}
		},
		"unsupported state": func(p *fakeProvider) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateExpired, Progress: .1}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := providerWithDefaults()
			p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
			setup(p)
			st := queue.NewMemoryStore()
			q := queue.New(1)
			orch := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, PollInterval: time.Nanosecond, MaxPolls: 1}
			j := queueJobForTest(name)
			j.Provider = "fake"
			if err := st.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			if err := q.Enqueue(ctx, j); err != nil {
				t.Fatal(err)
			}
			item, _ := q.Next(ctx)
			if err := orch.RunJob(ctx, item); err == nil {
				t.Fatal("invalid provider status accepted")
			}
			got, _ := st.Get(ctx, j.JobID)
			if got.State != video.StateFailed && got.State != video.StateCancelled && got.State != video.StateNeedsManualAction {
				t.Fatalf("unexpected state=%s", got.State)
			}
		})
	}

	p := providerWithDefaults()
	st := queue.NewMemoryStore()
	orch := &Orchestrator{Store: st, Providers: Registry{"fake": p}}
	for _, state := range []video.JobState{video.StateCompleted, video.StateFailed, video.StateCancelled, video.StateDryRun} {
		j := queueJobForTest(string(state))
		j.State = state
		if err := st.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		if err := orch.RunJob(ctx, j); err != nil {
			t.Fatalf("terminal %s: %v", state, err)
		}
	}
	manual := queueJobForTest("manual")
	manual.State = video.StateNeedsManualAction
	if err := st.Create(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(ctx, manual); err == nil {
		t.Fatal("manual job ran")
	}
}

func TestCancelAndWebhookPaths(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	st := queue.NewMemoryStore()
	orch := &Orchestrator{Store: st, Providers: Registry{"fake": p}, Ledger: &cost.Ledger{}}
	noRemote := queueJobForTest("no-remote")
	noRemote.Provider = "fake"
	if err := st.Create(ctx, noRemote); err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, noRemote.JobID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, noRemote.JobID); got.State != video.StateCancelled {
		t.Fatalf("no-remote state=%s", got.State)
	}
	active := queueJobForTest("active-cancel")
	active.Provider, active.ProviderJobID, active.State = "fake", "remote", video.StateProcessing
	if err := st.Create(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, active.JobID); err != nil || p.cancelCalls != 1 {
		t.Fatalf("active cancel err=%v calls=%d", err, p.cancelCalls)
	}
	terminal := queueJobForTest("terminal")
	terminal.State = video.StateCompleted
	if err := st.Create(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, terminal.JobID); err == nil {
		t.Fatal("terminal cancellation accepted")
	}
	missingProvider := queueJobForTest("missing-provider")
	missingProvider.Provider, missingProvider.ProviderJobID = "missing", "remote"
	if err := st.Create(ctx, missingProvider); err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, missingProvider.JobID); err == nil {
		t.Fatal("cancelled with unavailable remote provider")
	}
	p.cancelErr = errors.New("remote cancel failed")
	retry := queueJobForTest("cancel-fails")
	retry.Provider, retry.ProviderJobID = "fake", "remote"
	if err := st.Create(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, retry.JobID); err == nil {
		t.Fatal("provider cancel error suppressed")
	}

	webStore := queue.NewMemoryStore()
	webhookJob := queueJobForTest("webhook")
	webhookJob.ProviderJobID, webhookJob.State = "remote-web", video.StateProcessing
	if err := webStore.Create(ctx, webhookJob); err != nil {
		t.Fatal(err)
	}
	wp := NewWebhookProcessor(webStore)
	if err := wp.Apply(ctx, "", video.ProviderJobStatus{}); err == nil {
		t.Fatal("empty event accepted")
	}
	if err := wp.Apply(ctx, "evt", video.ProviderJobStatus{ProviderJobID: "missing", State: video.StateCompleted}); err == nil {
		t.Fatal("unknown webhook accepted")
	}
	if err := wp.Apply(ctx, "evt", video.ProviderJobStatus{ProviderJobID: "remote-web", State: video.StateCompleted, Progress: 1, Error: "ignored after completion"}); err != nil {
		t.Fatal(err)
	}
	if err := wp.Apply(ctx, "evt", video.ProviderJobStatus{ProviderJobID: "remote-web", State: video.StateFailed}); err != nil {
		t.Fatal("dedupe failed: ", err)
	}
	if got, _ := webStore.Get(ctx, webhookJob.JobID); got.State != video.StateCompleted || got.Progress != 1 {
		t.Fatalf("webhook update=%#v", got)
	}
	badTransition := "evt-bad"
	if err := wp.Apply(ctx, badTransition, video.ProviderJobStatus{ProviderJobID: "remote-web", State: video.StateProcessing}); err == nil {
		t.Fatal("invalid webhook regression accepted")
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(ts))
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Video-Timestamp", ts)
	req.Header.Set("X-Video-Signature", hex.EncodeToString(mac.Sum(nil)))
	if err := VerifyHMAC(req, "secret"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.Header.Set("X-Video-Timestamp", "not-a-time") }, func(r *http.Request) { r.Header.Set("X-Video-Signature", "bad") }} {
		bad := req.Clone(ctx)
		mutate(bad)
		if err := VerifyHMAC(bad, "secret"); err == nil {
			t.Fatal("invalid HMAC accepted")
		}
	}
}

func TestPollCancellationPreservesResumableState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: .1}}
	st := queue.NewMemoryStore()
	q := queue.New(1)
	orch := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, PollInterval: time.Hour}
	j := queueJobForTest("shutdown")
	j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
	if err := st.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := orch.RunJob(ctx, j); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
	got, _ := st.Get(context.Background(), j.JobID)
	if got.State != video.StateSubmitted {
		t.Fatalf("shutdown mutated state=%s", got.State)
	}
}

// Keep compile-time checking local if the fake interface changes.
var _ = fmt.Sprintf
var _ = sync.Mutex{}
