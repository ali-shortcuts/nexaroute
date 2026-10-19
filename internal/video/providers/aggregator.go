package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/egress"
	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Phase 20 (#420): the first real external video path — an async
// aggregator-style adapter in the fal.ai queue class. The wire
// contract is the aggregator queue layout:
//
//	submit:  POST   {base}/{model}                          -> {request_id}
//	status:  GET    {base}/{model}/requests/{id}/status     -> {status}
//	result:  GET    {base}/{model}/requests/{id}            -> {video:{url,...}}
//	cancel:  PUT    {base}/{model}/requests/{id}/cancel
//
// Status and result URLs are DERIVED from the provider job ID,
// never stored, so polling resumes after a gateway restart with
// only the persisted job ID — no resubmission, no duplicate
// billing. The adapter is poll-only: it claims no webhook
// support and fails webhook calls loudly. Live verification
// against a real aggregator account is a separate, key-gated
// step (UNVERIFIED until an operator runs it); everything here
// is proven against a recording fake aggregator.

const (
	aggregatorDefaultMaxOutput = int64(512 << 20)
	aggregatorMaxResponseBytes = 8 << 20
)

type Aggregator struct {
	baseURL        string
	model          string
	apiKey         string
	authScheme     string
	maxDuration    float64
	costPerSecond  float64
	imageField     string
	endImageField  string
	maxOutputBytes int64
	client         *http.Client
}

func NewAggregator(cfg video.Config, maxOutputBytes int64) (*Aggregator, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.AggregatorBaseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("aggregator_base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("aggregator over plain HTTP is only allowed on loopback; use HTTPS for remote endpoints")
		}
	}
	model := strings.TrimSpace(cfg.AggregatorModel)
	if model == "" {
		return nil, errors.New("aggregator_model is required")
	}
	envName := strings.TrimSpace(cfg.AggregatorAPIKeyEnv)
	if envName == "" {
		return nil, errors.New("aggregator_api_key_env is required (name of the environment variable holding the API key)")
	}
	apiKey := strings.TrimSpace(os.Getenv(envName))
	if apiKey == "" {
		return nil, fmt.Errorf("aggregator API key environment variable %s is empty or unset", envName)
	}
	scheme := strings.TrimSpace(cfg.AggregatorAuthScheme)
	if scheme == "" {
		scheme = "Key"
	}
	if scheme != "Key" && scheme != "Bearer" {
		return nil, errors.New("aggregator_auth_scheme must be \"Key\" or \"Bearer\"")
	}
	maxDuration := cfg.AggregatorMaxDurationSeconds
	if maxDuration == 0 {
		maxDuration = 300
	}
	if maxDuration < 1 || maxDuration > 3600 || math.IsNaN(maxDuration) || math.IsInf(maxDuration, 0) {
		return nil, errors.New("aggregator_max_duration_seconds must be between 1 and 3600")
	}
	if cfg.AggregatorCostPerSecondUSD < 0 || math.IsNaN(cfg.AggregatorCostPerSecondUSD) || math.IsInf(cfg.AggregatorCostPerSecondUSD, 0) {
		return nil, errors.New("aggregator_cost_per_second_usd must be a finite non-negative amount")
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = aggregatorDefaultMaxOutput
	}
	// All API traffic routes through the egress-guarded transport
	// pinned to the configured aggregator host (audit #371 F8
	// pattern, as in the ComfyUI adapter).
	tr, err := egress.Policy{
		AllowedHosts:  []string{u.Hostname()},
		AllowLoopback: true,
	}.NewTransport(base, "")
	if err != nil {
		return nil, fmt.Errorf("aggregator egress policy: %w", err)
	}
	return &Aggregator{
		baseURL: base, model: model, apiKey: apiKey, authScheme: scheme,
		maxDuration: maxDuration, costPerSecond: cfg.AggregatorCostPerSecondUSD,
		imageField:     strings.TrimSpace(cfg.AggregatorImageField),
		endImageField:  strings.TrimSpace(cfg.AggregatorEndImageField),
		maxOutputBytes: maxOutputBytes,
		client: &http.Client{
			Transport:     tr,
			Timeout:       5 * time.Minute,
			CheckRedirect: egress.RefuseCrossOriginRedirects(5, "aggregator"),
		},
	}, nil
}

func (p *Aggregator) ID() string { return "aggregator" }

func (p *Aggregator) Capabilities(context.Context) (video.Capabilities, error) {
	modes := []video.Mode{video.ModeTextToVideo}
	if p.imageField != "" {
		modes = append(modes, video.ModeImageToVideo)
	}
	return video.Capabilities{
		Modes: modes, Models: []string{p.model},
		MaxDurationSeconds: p.maxDuration,
		AspectRatios:       []video.AspectRatio{video.Aspect16x9, video.Aspect9x16, video.Aspect1x1},
		// A billed third-party generation API reached over HTTPS.
		// The adapter is complete against the provider contract and
		// the recording-fake E2E; a live keyed run is tracked
		// separately and remains UNVERIFIED until performed.
		ProviderKind: video.ProviderKindCommercial, ProductionReady: true,
		// Poll-only async: cancellation is a real API call; there
		// are no webhooks. Only the configured image field (when
		// set) makes first-frame input consumable; everything else
		// stays unclaimed so it stays hidden in the UI.
		SupportsAudio: false, SupportsCancel: true, SupportsWebhooks: false,
		SupportsReferenceFrames: p.imageField != "", SupportsClipExtension: false,
		SupportsVideoEditing: false, SupportsMultiShot: false,
		SupportsComposition: false, SupportsSubtitles: false,
		SupportsNarration: false, SupportsMusicGeneration: false,
		SupportsDialogueGeneration: false,
	}, nil
}

func (p *Aggregator) ValidateRequest(r video.VideoRequest) error {
	r = r.Normalized()
	if err := r.Validate(); err != nil {
		return err
	}
	caps, _ := p.Capabilities(context.Background())
	advertised := false
	for _, m := range caps.Modes {
		if r.Mode == m {
			advertised = true
		}
	}
	if !advertised {
		return fmt.Errorf("aggregator model %q does not advertise mode %q", p.model, r.Mode)
	}
	if r.DurationSeconds > p.maxDuration {
		return fmt.Errorf("requested duration %.2fs exceeds configured aggregator maximum %.2fs", r.DurationSeconds, p.maxDuration)
	}
	if r.ModelPreference != "" && r.ModelPreference != p.model {
		return fmt.Errorf("aggregator model must be %q", p.model)
	}
	if strings.TrimSpace(r.CallbackURL) != "" {
		return errors.New("aggregator adapter is poll-only and does not accept callback URLs")
	}
	if r.AudioRequested || r.DialogueRequested || strings.TrimSpace(r.NarrationText) != "" || strings.TrimSpace(r.SubtitleLanguage) != "" || len(r.SoundRequirements) > 0 {
		return errors.New("aggregator adapter does not generate audio, dialogue, narration, or subtitles")
	}
	if len(r.ShotPlan) > 1 {
		return errors.New("aggregator adapter submits one generation per job; multi-shot composition is a gateway pipeline")
	}
	if len(r.CharacterReferences) > 0 || len(r.LocationReferences) > 0 || len(r.StyleReferences) > 0 || r.ContinuityContext != nil {
		return errors.New("aggregator adapter cannot consume character/location/style references or continuity inputs")
	}
	if r.LastFrameAsset != nil {
		if p.endImageField == "" {
			return errors.New("aggregator adapter has no end-image input configured (aggregator_end_image_field); last-frame assets are not consumable")
		}
		if !isHTTPURL(r.LastFrameAsset.URI) {
			return errors.New("first/last-frame interpolation requires a last-frame asset with an http(s) URI the provider can fetch")
		}
	}
	if p.imageField == "" {
		if len(r.ReferenceAssets) > 0 || r.FirstFrameAsset != nil {
			return errors.New("aggregator adapter has no image input configured (aggregator_image_field); reference/first-frame assets are not consumable")
		}
	} else {
		if len(r.ReferenceAssets) > 1 {
			return errors.New("aggregator adapter consumes at most one image input")
		}
		if img := firstImageAsset(r); img == nil || !isHTTPURL(img.URI) {
			return errors.New("image_to_video requires a first-frame or reference asset with an http(s) URI the provider can fetch")
		}
	}
	return nil
}

func firstImageAsset(r video.VideoRequest) *video.Asset {
	if r.FirstFrameAsset != nil {
		return r.FirstFrameAsset
	}
	if len(r.ReferenceAssets) > 0 {
		return &r.ReferenceAssets[0]
	}
	return nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (p *Aggregator) EstimateCost(r video.VideoRequest) (video.CostEstimate, error) {
	r = r.Normalized()
	est := video.CostEstimate{Provider: p.ID(), Model: p.model, Currency: "USD", BillableSeconds: r.DurationSeconds}
	if p.costPerSecond > 0 {
		amount := r.DurationSeconds * p.costPerSecond
		est.PriceKnown = true
		est.EstimatedUSD = amount
		est.UpperBoundUSD = amount
		est.Breakdown = map[string]float64{"generation": amount}
	}
	return est, nil
}

func (p *Aggregator) submitBody(r video.VideoRequest) map[string]any {
	body := map[string]any{"prompt": r.Prompt}
	if r.DurationSeconds > 0 {
		body["duration_seconds"] = r.DurationSeconds
	}
	if r.AspectRatio != "" {
		body["aspect_ratio"] = string(r.AspectRatio)
	}
	if strings.TrimSpace(r.NegativePrompt) != "" {
		body["negative_prompt"] = r.NegativePrompt
	}
	if r.Seed != nil {
		body["seed"] = *r.Seed
	}
	if p.imageField != "" {
		if img := firstImageAsset(r); img != nil {
			body[p.imageField] = img.URI
		}
	}
	if p.endImageField != "" && r.LastFrameAsset != nil {
		body[p.endImageField] = r.LastFrameAsset.URI
	}
	return body
}

func (p *Aggregator) CreateJob(ctx context.Context, r video.VideoRequest) (video.ProviderJob, error) {
	if err := p.ValidateRequest(r); err != nil {
		return video.ProviderJob{}, err
	}
	r = r.Normalized()
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := p.doJSON(ctx, http.MethodPost, p.baseURL+"/"+p.model, p.submitBody(r), &out); err != nil {
		return video.ProviderJob{}, err
	}
	if strings.TrimSpace(out.RequestID) == "" {
		return video.ProviderJob{}, errors.New("aggregator submit response carried no request_id")
	}
	return video.ProviderJob{ProviderJobID: out.RequestID, Provider: p.ID(), Model: p.model, Accepted: true}, nil
}

func (p *Aggregator) statusURL(id string) string {
	return p.baseURL + "/" + p.model + "/requests/" + url.PathEscape(id) + "/status"
}

func (p *Aggregator) resultURL(id string) string {
	return p.baseURL + "/" + p.model + "/requests/" + url.PathEscape(id)
}

func (p *Aggregator) GetJob(ctx context.Context, id string) (video.ProviderJobStatus, error) {
	if strings.TrimSpace(id) == "" {
		return video.ProviderJobStatus{}, errors.New("aggregator provider job ID is required")
	}
	var st struct {
		Status   string   `json:"status"`
		Progress *float64 `json:"progress"`
		Error    string   `json:"error"`
	}
	if err := p.doJSON(ctx, http.MethodGet, p.statusURL(id), nil, &st); err != nil {
		return video.ProviderJobStatus{}, err
	}
	switch strings.ToUpper(strings.TrimSpace(st.Status)) {
	case "IN_QUEUE", "QUEUED":
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateQueued}, nil
	case "IN_PROGRESS", "PROCESSING", "RUNNING":
		out := video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing}
		if st.Progress != nil && *st.Progress >= 0 && *st.Progress <= 1 {
			out.Progress = *st.Progress
		}
		return out, nil
	case "ERROR", "FAILED", "FAILURE":
		msg := strings.TrimSpace(st.Error)
		if msg == "" {
			msg = "aggregator reported a failed generation"
		}
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateFailed, Error: msg}, nil
	case "COMPLETED", "SUCCEEDED", "SUCCESS":
		return p.completedStatus(ctx, id)
	default:
		// Unknown non-terminal vocabulary: report processing
		// rather than inventing a terminal state.
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing}, nil
	}
}

func (p *Aggregator) completedStatus(ctx context.Context, id string) (video.ProviderJobStatus, error) {
	var res map[string]any
	if err := p.doJSON(ctx, http.MethodGet, p.resultURL(id), nil, &res); err != nil {
		return video.ProviderJobStatus{}, err
	}
	videoURL, contentType := extractAggregatorVideo(res)
	if videoURL == "" {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateFailed, Error: "aggregator completed without a video output URL"}, nil
	}
	asset := video.Asset{
		ID: id + "-video", Kind: "video", URI: videoURL, ContentType: contentType,
		Metadata: map[string]string{"provider_job_id": id},
	}
	return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{asset}}, nil
}

// extractAggregatorVideo walks the result payload shapes the
// aggregator class uses, in documented precedence order.
func extractAggregatorVideo(res map[string]any) (string, string) {
	get := func(m map[string]any, key string) (map[string]any, string, string) {
		raw, ok := m[key]
		if !ok {
			return nil, "", ""
		}
		if s, ok := raw.(string); ok {
			return nil, s, ""
		}
		if obj, ok := raw.(map[string]any); ok {
			u, _ := obj["url"].(string)
			ct, _ := obj["content_type"].(string)
			return obj, u, ct
		}
		return nil, "", ""
	}
	for _, key := range []string{"video", "output", "result"} {
		if _, u, ct := get(res, key); u != "" {
			return u, ct
		}
	}
	if u, _ := res["video_url"].(string); u != "" {
		return u, ""
	}
	if u, _ := res["url"].(string); u != "" {
		return u, ""
	}
	return "", ""
}

func (p *Aggregator) CancelJob(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("aggregator provider job ID is required")
	}
	return p.doJSON(ctx, http.MethodPut, p.resultURL(id)+"/cancel", nil, nil)
}

func (p *Aggregator) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	if sink == nil {
		return nil, errors.New("video asset sink is not configured")
	}
	if len(status.Outputs) == 0 {
		return nil, errors.New("aggregator completion has no video output assets")
	}
	result := make([]video.Asset, 0, len(status.Outputs))
	for _, asset := range status.Outputs {
		if !isHTTPURL(asset.URI) {
			return nil, fmt.Errorf("aggregator output %q is not an http(s) URL", asset.URI)
		}
		if asset.ContentType != "" && !strings.HasPrefix(strings.ToLower(asset.ContentType), "video/") {
			return nil, fmt.Errorf("aggregator declared a non-video output content type %q; refusing download", asset.ContentType)
		}
		data, contentType, err := p.download(ctx, asset.URI)
		if err != nil {
			return nil, err
		}
		asset.ContentType = contentType
		saved, err := sink.Put(ctx, asset, data)
		if err != nil {
			return nil, fmt.Errorf("persist aggregator video output: %w", err)
		}
		result = append(result, saved)
	}
	return result, nil
}

// download fetches one output URL through a fresh egress-guarded
// client pinned to that URL's host — aggregator outputs are
// served from CDN hosts distinct from the API host.
func (p *Aggregator) download(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("parse aggregator output URL: %v", err)
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, "", errors.New("aggregator output over plain HTTP is only allowed on loopback")
		}
	}
	tr, err := egress.Policy{AllowedHosts: []string{u.Hostname()}, AllowLoopback: true}.NewTransport(rawURL, "")
	if err != nil {
		return nil, "", fmt.Errorf("aggregator output egress policy: %w", err)
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Minute, CheckRedirect: egress.RefuseCrossOriginRedirects(5, "aggregator-output")}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download aggregator output: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, p.maxOutputBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, "", fmt.Errorf("read aggregator output: %w", readErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("close aggregator output response: %w", closeErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("aggregator output download returned HTTP %d", resp.StatusCode)
	}
	if int64(len(data)) > p.maxOutputBytes {
		return nil, "", fmt.Errorf("aggregator output exceeds configured asset limit of %d bytes", p.maxOutputBytes)
	}
	if len(data) == 0 {
		return nil, "", errors.New("aggregator returned an empty video output")
	}
	contentType := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.HasPrefix(strings.ToLower(contentType), "video/") && contentType != "application/octet-stream" {
		return nil, "", fmt.Errorf("aggregator output has non-video content type %q; refusing to store it as a video asset", contentType)
	}
	// A Content-Type header is a claim, not proof: a compromised
	// or misbehaving output host can label anything video/*. The
	// bytes must also look like a video container (#398 item 6).
	if !looksLikeVideoContainer(data) {
		return nil, "", errors.New("aggregator output bytes do not look like a video container; refusing to store them as a video asset")
	}
	return data, contentType, nil
}

// looksLikeVideoContainer reports whether the payload carries a
// known video container signature (ISO-BMFF/MP4/MOV ftyp box,
// WebM/Matroska EBML header, AVI RIFF, FLV, MPEG-TS sync byte,
// MPEG-PS pack header) or sniffs as video/* via the standard
// library's content detection.
func looksLikeVideoContainer(data []byte) bool {
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		return true // MP4 / MOV / AVIF-family ISO-BMFF
	}
	if len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return true // WebM / Matroska EBML
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "AVI " {
		return true // AVI
	}
	if len(data) >= 3 && string(data[0:3]) == "FLV" {
		return true // FLV
	}
	if len(data) >= 4 && data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x01 && data[3] == 0xBA {
		return true // MPEG program stream
	}
	if len(data) >= 189 && data[0] == 0x47 && data[188] == 0x47 {
		return true // MPEG-TS (sync byte repeats every 188 bytes)
	}
	return strings.HasPrefix(http.DetectContentType(data), "video/")
}

func (p *Aggregator) RegisterWebhook(context.Context, video.WebhookRegistration) error {
	return errors.New("aggregator provider does not support webhooks (poll-only)")
}
func (p *Aggregator) VerifyWebhook(*http.Request) error {
	return errors.New("aggregator provider does not support webhooks (poll-only)")
}
func (p *Aggregator) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, errors.New("aggregator provider does not support webhooks (poll-only)")
}

func (p *Aggregator) doJSON(ctx context.Context, method, fullURL string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", p.authScheme+" "+p.apiKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("aggregator %s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, aggregatorMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("read aggregator response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("aggregator %s returned HTTP %d: %s", method, resp.StatusCode, snippet)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode aggregator response: %w", err)
		}
	}
	return nil
}
