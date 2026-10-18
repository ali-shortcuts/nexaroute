package providers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/egress"
	"github.com/ali-shortcuts/nexaroute/internal/video"
)

const (
	comfyMaxWorkflowBytes = 8 << 20
	comfyMaxResponseBytes = 8 << 20
	comfyDefaultMaxOutput = int64(512 << 20)
)

type ComfyUI struct {
	baseURL        string
	workflow       map[string]map[string]any
	model          string
	maxDuration    float64
	maxOutputBytes int64
	client         *http.Client
	cfg            video.Config
	promptInput    string
	negativeInput  string
	widthInput     string
	heightInput    string
	framesInput    string
	fpsInput       string
	seedInput      string
}

type comfyOutputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
	Format    string `json:"format,omitempty"`
}

type comfyHistoryEntry struct {
	Outputs map[string]map[string]json.RawMessage `json:"outputs"`
	Status  struct {
		StatusStr string `json:"status_str"`
		Completed bool   `json:"completed"`
	} `json:"status"`
}

type comfyQueue struct {
	Running [][]json.RawMessage `json:"queue_running"`
	Pending [][]json.RawMessage `json:"queue_pending"`
}

func NewComfyUI(cfg video.Config, maxOutputBytes int64) (*ComfyUI, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.ComfyUIBaseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("comfyui_base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("ComfyUI over plain HTTP is only allowed on loopback; use HTTPS for remote endpoints")
		}
	}
	workflowPath := strings.TrimSpace(cfg.ComfyUIWorkflowPath)
	if workflowPath == "" {
		return nil, errors.New("comfyui_workflow_path is required")
	}
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("read ComfyUI API workflow: %w", err)
	}
	if len(data) == 0 || len(data) > comfyMaxWorkflowBytes {
		return nil, fmt.Errorf("ComfyUI workflow must be between 1 byte and %d bytes", comfyMaxWorkflowBytes)
	}
	var workflow map[string]map[string]any
	if err := json.Unmarshal(data, &workflow); err != nil || len(workflow) == 0 {
		return nil, errors.New("ComfyUI workflow must be a non-empty API-format JSON object")
	}
	model := strings.TrimSpace(cfg.ComfyUIModel)
	if model == "" {
		model = "comfyui-workflow"
	}
	maxDuration := cfg.ComfyUIMaxDurationSeconds
	if maxDuration == 0 {
		maxDuration = 60
	}
	if maxDuration < 1 || maxDuration > 3600 || math.IsNaN(maxDuration) || math.IsInf(maxDuration, 0) {
		return nil, errors.New("comfyui_max_duration_seconds must be between 1 and 3600")
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = comfyDefaultMaxOutput
	}
	// Audit #371 F8: the client must route through the egress-guarded
	// transport. Dials are pinned to the configured base host and every
	// resolved address passes the deny lists and the special-purpose IP
	// blocklist — https://169.254.169.254/ as comfyui_base_url is refused
	// by the policy (at construction, before any dial) instead of being
	// dialed. Loopback stays reachable: ComfyUI is a self-hosted
	// operator tool and commonly runs on loopback; link-local, private
	// and other special-purpose addresses stay blocked regardless.
	tr, err := egress.Policy{
		AllowedHosts:  []string{u.Hostname()},
		AllowLoopback: true,
	}.NewTransport(base, "")
	if err != nil {
		return nil, fmt.Errorf("ComfyUI egress policy: %w", err)
	}
	p := &ComfyUI{
		baseURL:        base,
		workflow:       workflow,
		model:          model,
		maxDuration:    maxDuration,
		maxOutputBytes: maxOutputBytes,
		client: &http.Client{
			Transport:     tr,
			Timeout:       5 * time.Minute,
			CheckRedirect: egress.RefuseCrossOriginRedirects(5, "comfyui"),
		},
		cfg:           cfg,
		promptInput:   defaultInput(cfg.ComfyUIPromptInput, "text"),
		negativeInput: defaultInput(cfg.ComfyUINegativeInput, "text"),
		widthInput:    defaultInput(cfg.ComfyUIWidthInput, "width"),
		heightInput:   defaultInput(cfg.ComfyUIHeightInput, "height"),
		framesInput:   defaultInput(cfg.ComfyUIFramesInput, "length"),
		fpsInput:      defaultInput(cfg.ComfyUIFPSInput, "frame_rate"),
		seedInput:     defaultInput(cfg.ComfyUISeedInput, "noise_seed"),
	}
	for _, required := range []struct{ id, input, label string }{
		{cfg.ComfyUIPromptNodeID, p.promptInput, "prompt"},
		{cfg.ComfyUIWidthNodeID, p.widthInput, "width"},
		{cfg.ComfyUIHeightNodeID, p.heightInput, "height"},
		{cfg.ComfyUIFramesNodeID, p.framesInput, "frames"},
	} {
		if err := p.validateInput(required.id, required.input, required.label); err != nil {
			return nil, err
		}
	}
	optional := []struct{ id, input, label string }{
		{cfg.ComfyUINegativeNodeID, p.negativeInput, "negative prompt"},
		{cfg.ComfyUIFPSNodeID, p.fpsInput, "fps"},
		{cfg.ComfyUISeedNodeID, p.seedInput, "seed"},
	}
	for _, item := range optional {
		if strings.TrimSpace(item.id) != "" {
			if err := p.validateInput(item.id, item.input, item.label); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

func defaultInput(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func (p *ComfyUI) validateInput(nodeID, input, label string) error {
	node, ok := p.workflow[strings.TrimSpace(nodeID)]
	if !ok {
		return fmt.Errorf("ComfyUI workflow has no configured %s node %q", label, nodeID)
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		return fmt.Errorf("ComfyUI workflow node %q has no inputs object", nodeID)
	}
	if _, ok := inputs[input]; !ok {
		return fmt.Errorf("ComfyUI workflow node %q has no input %q for %s", nodeID, input, label)
	}
	return nil
}

func (p *ComfyUI) ID() string { return "comfyui" }

func (p *ComfyUI) Capabilities(context.Context) (video.Capabilities, error) {
	return video.Capabilities{
		Modes: []video.Mode{video.ModeTextToVideo}, Models: []string{p.model},
		MaxDurationSeconds: p.maxDuration,
		AspectRatios:       []video.AspectRatio{video.Aspect16x9, video.Aspect9x16, video.Aspect1x1},
		// The ComfyUI adapter is a LOCAL backend: it submits a
		// user-supplied API-format workflow to a ComfyUI server on the
		// operator's own machine. It is production-ready as a local
		// adapter, but it is not a commercial video API and must never
		// be presented as one.
		ProviderKind: video.ProviderKindLocal, ProductionReady: true,
		// The adapter drives one text-to-video workflow per job. It
		// cannot consume reference/first/last-frame inputs, extend
		// clips, edit existing video, accept multi-shot submissions,
		// compose provider-side, or generate any audio; narration and
		// subtitles are a Gateway-side local pipeline, not a provider
		// capability.
		SupportsAudio: false, SupportsCancel: false, SupportsWebhooks: false,
		SupportsReferenceFrames: false, SupportsClipExtension: false,
		SupportsVideoEditing: false, SupportsMultiShot: false,
		SupportsComposition: false, SupportsSubtitles: false,
		SupportsNarration: false, SupportsMusicGeneration: false,
		SupportsDialogueGeneration: false,
	}, nil
}

func (p *ComfyUI) ValidateRequest(r video.VideoRequest) error {
	r = r.Normalized()
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Mode != video.ModeTextToVideo {
		return errors.New("ComfyUI workflow adapter currently supports text_to_video only")
	}
	if r.DurationSeconds > p.maxDuration {
		return fmt.Errorf("requested duration %.2fs exceeds configured ComfyUI maximum %.2fs", r.DurationSeconds, p.maxDuration)
	}
	if r.ModelPreference != "" && r.ModelPreference != p.model {
		return fmt.Errorf("ComfyUI workflow model must be %q", p.model)
	}
	if r.AudioRequested || r.DialogueRequested || strings.TrimSpace(r.NarrationText) != "" || strings.TrimSpace(r.SubtitleLanguage) != "" {
		return errors.New("ComfyUI workflow adapter does not claim audio, narration, or subtitle support")
	}
	if len(r.SoundRequirements) > 0 {
		return errors.New("ComfyUI workflow adapter does not generate music or sound effects")
	}
	if len(r.ShotPlan) > 1 {
		return errors.New("ComfyUI adapter submits one workflow per job; multi-shot episode orchestration is not implemented")
	}
	if strings.TrimSpace(r.Prompt) == "" && (len(r.ShotPlan) != 1 || strings.TrimSpace(r.ShotPlan[0].Prompt) == "") {
		return errors.New("ComfyUI requires a non-empty text prompt")
	}
	if len(r.ReferenceAssets) > 0 || r.FirstFrameAsset != nil || r.LastFrameAsset != nil ||
		len(r.CharacterReferences) > 0 || len(r.LocationReferences) > 0 || len(r.StyleReferences) > 0 {
		return errors.New("ComfyUI workflow adapter does not map reference or first/last-frame assets into the workflow")
	}
	if r.ContinuityContext != nil {
		return errors.New("ComfyUI workflow adapter does not map continuity context into the workflow")
	}
	if strings.TrimSpace(r.CallbackURL) != "" {
		return errors.New("ComfyUI provider does not support callbacks or webhooks")
	}
	if strings.ToLower(strings.TrimSpace(r.OutputFormat)) != "mp4" {
		return errors.New("ComfyUI workflow adapter currently supports MP4 output only")
	}
	negativePrompt := strings.TrimSpace(r.NegativePrompt)
	if negativePrompt == "" && len(r.ShotPlan) == 1 {
		negativePrompt = strings.TrimSpace(r.ShotPlan[0].NegativePrompt)
	}
	if negativePrompt != "" && strings.TrimSpace(p.cfg.ComfyUINegativeNodeID) == "" {
		return errors.New("negative_prompt requires a configured ComfyUI negative-prompt node")
	}
	if r.Seed != nil && strings.TrimSpace(p.cfg.ComfyUISeedNodeID) == "" {
		return errors.New("seed requires a configured ComfyUI seed node")
	}
	if r.FPS != 24 && strings.TrimSpace(p.cfg.ComfyUIFPSNodeID) == "" {
		return errors.New("non-default fps requires a configured ComfyUI FPS node")
	}
	if r.AspectRatio == video.AspectCustom && (r.Width < 1 || r.Height < 1) {
		return errors.New("custom aspect ratio requires explicit positive width and height")
	}
	width, height := outputDimensions(r)
	if width < 64 || height < 64 || width > 4096 || height > 4096 {
		return errors.New("ComfyUI output dimensions must be between 64 and 4096 pixels")
	}
	return nil
}

func (p *ComfyUI) EstimateCost(r video.VideoRequest) (video.CostEstimate, error) {
	if err := p.ValidateRequest(r); err != nil {
		return video.CostEstimate{}, err
	}
	return video.CostEstimate{Provider: p.ID(), Model: p.model, Currency: "USD", EstimatedUSD: 0, UpperBoundUSD: 0, PriceKnown: true, BillableSeconds: r.DurationSeconds}, nil
}

func (p *ComfyUI) CreateJob(ctx context.Context, r video.VideoRequest) (video.ProviderJob, error) {
	r = r.Normalized()
	if err := p.ValidateRequest(r); err != nil {
		return video.ProviderJob{}, err
	}
	data, err := json.Marshal(p.workflow)
	if err != nil {
		return video.ProviderJob{}, fmt.Errorf("clone ComfyUI workflow: %w", err)
	}
	var workflow map[string]map[string]any
	if err := json.Unmarshal(data, &workflow); err != nil {
		return video.ProviderJob{}, fmt.Errorf("clone ComfyUI workflow: %w", err)
	}
	prompt := strings.TrimSpace(r.Prompt)
	if prompt == "" {
		parts := make([]string, 0, len(r.ShotPlan))
		for _, shot := range r.ShotPlan {
			parts = append(parts, strings.TrimSpace(shot.Prompt))
		}
		prompt = strings.Join(parts, "\n")
	}
	if err := setWorkflowInput(workflow, p.cfg.ComfyUIPromptNodeID, p.promptInput, prompt); err != nil {
		return video.ProviderJob{}, err
	}
	negativePrompt := strings.TrimSpace(r.NegativePrompt)
	if negativePrompt == "" && len(r.ShotPlan) == 1 {
		negativePrompt = strings.TrimSpace(r.ShotPlan[0].NegativePrompt)
	}
	if strings.TrimSpace(p.cfg.ComfyUINegativeNodeID) != "" {
		if err := setWorkflowInput(workflow, p.cfg.ComfyUINegativeNodeID, p.negativeInput, negativePrompt); err != nil {
			return video.ProviderJob{}, err
		}
	}
	width, height := outputDimensions(r)
	if err := setWorkflowInput(workflow, p.cfg.ComfyUIWidthNodeID, p.widthInput, width); err != nil {
		return video.ProviderJob{}, err
	}
	if err := setWorkflowInput(workflow, p.cfg.ComfyUIHeightNodeID, p.heightInput, height); err != nil {
		return video.ProviderJob{}, err
	}
	frames := int(math.Round(r.DurationSeconds * float64(r.FPS)))
	if frames < 1 {
		frames = 1
	}
	if err := setWorkflowInput(workflow, p.cfg.ComfyUIFramesNodeID, p.framesInput, frames); err != nil {
		return video.ProviderJob{}, err
	}
	if strings.TrimSpace(p.cfg.ComfyUIFPSNodeID) != "" {
		if err := setWorkflowInput(workflow, p.cfg.ComfyUIFPSNodeID, p.fpsInput, r.FPS); err != nil {
			return video.ProviderJob{}, err
		}
	}
	if r.Seed != nil && strings.TrimSpace(p.cfg.ComfyUISeedNodeID) != "" {
		if err := setWorkflowInput(workflow, p.cfg.ComfyUISeedNodeID, p.seedInput, *r.Seed); err != nil {
			return video.ProviderJob{}, err
		}
	}
	clientIDBytes := make([]byte, 16)
	if _, err := rand.Read(clientIDBytes); err != nil {
		return video.ProviderJob{}, fmt.Errorf("generate ComfyUI client ID: %w", err)
	}
	payload := map[string]any{"prompt": workflow, "client_id": hex.EncodeToString(clientIDBytes)}
	var response struct {
		PromptID   string          `json:"prompt_id"`
		NodeErrors json.RawMessage `json:"node_errors"`
	}
	if err := p.doJSON(ctx, http.MethodPost, "/prompt", payload, &response); err != nil {
		return video.ProviderJob{}, fmt.Errorf("submit ComfyUI workflow: %w", err)
	}
	if strings.TrimSpace(response.PromptID) == "" {
		return video.ProviderJob{}, errors.New("ComfyUI did not return a prompt_id; inspect the workflow and node validation errors")
	}
	return video.ProviderJob{ProviderJobID: response.PromptID, Provider: p.ID(), Model: p.model, Accepted: true}, nil
}

func outputDimensions(r video.VideoRequest) (int, int) {
	if r.Width > 0 && r.Height > 0 {
		return r.Width, r.Height
	}
	switch r.AspectRatio {
	case video.Aspect9x16:
		return 720, 1280
	case video.Aspect1x1:
		return 1024, 1024
	default:
		return 1280, 720
	}
}

func setWorkflowInput(workflow map[string]map[string]any, nodeID, input string, value any) error {
	node, ok := workflow[strings.TrimSpace(nodeID)]
	if !ok {
		return fmt.Errorf("ComfyUI workflow node %q disappeared while preparing request", nodeID)
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		return fmt.Errorf("ComfyUI workflow node %q has no inputs object", nodeID)
	}
	if _, ok := inputs[input]; !ok {
		return fmt.Errorf("ComfyUI workflow node %q has no input %q", nodeID, input)
	}
	inputs[input] = value
	workflow[nodeID] = node
	return nil
}

func (p *ComfyUI) GetJob(ctx context.Context, id string) (video.ProviderJobStatus, error) {
	if strings.TrimSpace(id) == "" {
		return video.ProviderJobStatus{}, errors.New("ComfyUI prompt ID is required")
	}
	var history map[string]comfyHistoryEntry
	if err := p.doJSON(ctx, http.MethodGet, "/history/"+url.PathEscape(id), nil, &history); err == nil {
		if entry, ok := history[id]; ok {
			if strings.EqualFold(entry.Status.StatusStr, "error") {
				return video.ProviderJobStatus{ProviderJobID: id, State: video.StateFailed, Error: "ComfyUI workflow reported an execution error"}, nil
			}
			outputs := collectComfyVideoOutputs(id, entry.Outputs)
			if entry.Status.Completed {
				if len(outputs) == 0 {
					return video.ProviderJobStatus{ProviderJobID: id, State: video.StateFailed, Error: "ComfyUI workflow completed without an MP4 output"}, nil
				}
				return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCompleted, Progress: 1, Outputs: outputs}, nil
			}
			return video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing, Progress: 0.75, Outputs: outputs}, nil
		}
	}
	var queueState comfyQueue
	if err := p.doJSON(ctx, http.MethodGet, "/queue", nil, &queueState); err != nil {
		return video.ProviderJobStatus{}, fmt.Errorf("read ComfyUI queue: %w", err)
	}
	if queueContains(queueState.Running, id) {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing, Progress: 0.25}, nil
	}
	if queueContains(queueState.Pending, id) {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateQueued}, nil
	}
	return video.ProviderJobStatus{}, errors.New("ComfyUI prompt is absent from history and queue")
}

func queueContains(items [][]json.RawMessage, id string) bool {
	for _, item := range items {
		if len(item) < 2 {
			continue
		}
		var got string
		if json.Unmarshal(item[1], &got) == nil && got == id {
			return true
		}
	}
	return false
}

func collectComfyVideoOutputs(promptID string, outputs map[string]map[string]json.RawMessage) []video.Asset {
	var assets []video.Asset
	for nodeID, nodeOutput := range outputs {
		for _, key := range []string{"videos", "gifs", "images"} {
			raw, ok := nodeOutput[key]
			if !ok {
				continue
			}
			var files []comfyOutputFile
			if json.Unmarshal(raw, &files) != nil {
				continue
			}
			for _, file := range files {
				ext := strings.ToLower(filepath.Ext(file.Filename))
				if ext != ".mp4" {
					continue
				}
				id := promptID + "-" + nodeID + "-" + file.Filename
				assets = append(assets, video.Asset{
					ID: id, Kind: "video", Filename: filepath.Base(file.Filename),
					Metadata: map[string]string{"filename": file.Filename, "subfolder": file.Subfolder, "type": file.Type, "node_id": nodeID, "provider": "comfyui"},
				})
			}
		}
	}
	return assets
}

func (p *ComfyUI) CancelJob(ctx context.Context, id string) error {
	var queueState comfyQueue
	if err := p.doJSON(ctx, http.MethodGet, "/queue", nil, &queueState); err != nil {
		return fmt.Errorf("read ComfyUI queue before cancellation: %w", err)
	}
	if queueContains(queueState.Running, id) {
		return errors.New("ComfyUI cannot cancel one running prompt without interrupting unrelated work; refusing global interrupt")
	}
	if !queueContains(queueState.Pending, id) {
		return errors.New("ComfyUI prompt is not pending; refusing to mark remote work cancelled")
	}
	return p.doJSON(ctx, http.MethodPost, "/queue", map[string]any{"delete": []string{id}}, nil)
}

func (p *ComfyUI) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	if sink == nil {
		return nil, errors.New("video asset sink is not configured")
	}
	if len(status.Outputs) == 0 {
		return nil, errors.New("ComfyUI completion has no video output assets")
	}
	result := make([]video.Asset, 0, len(status.Outputs))
	for _, asset := range status.Outputs {
		meta := asset.Metadata
		query := url.Values{}
		query.Set("filename", meta["filename"])
		query.Set("subfolder", meta["subfolder"])
		query.Set("type", meta["type"])
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/view?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download ComfyUI output: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, p.maxOutputBytes+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read ComfyUI output: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close ComfyUI output response: %w", closeErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("ComfyUI output download returned HTTP %d", resp.StatusCode)
		}
		if int64(len(data)) > p.maxOutputBytes {
			return nil, fmt.Errorf("ComfyUI output exceeds configured asset limit of %d bytes", p.maxOutputBytes)
		}
		if len(data) == 0 {
			return nil, errors.New("ComfyUI returned an empty video output")
		}
		asset.ContentType = resp.Header.Get("Content-Type")
		if strings.Contains(asset.ContentType, ";") {
			asset.ContentType = strings.TrimSpace(strings.SplitN(asset.ContentType, ";", 2)[0])
		}
		if asset.ContentType == "" || asset.ContentType == "application/octet-stream" {
			if guessed := mime.TypeByExtension(filepath.Ext(asset.Filename)); guessed != "" {
				asset.ContentType = guessed
			} else {
				asset.ContentType = "video/mp4"
			}
		}
		if !strings.HasPrefix(strings.ToLower(asset.ContentType), "video/") && asset.ContentType != "application/octet-stream" {
			return nil, fmt.Errorf("ComfyUI output has non-video content type %q", asset.ContentType)
		}
		saved, err := sink.Put(ctx, asset, data)
		if err != nil {
			return nil, fmt.Errorf("persist ComfyUI video output: %w", err)
		}
		result = append(result, saved)
	}
	return result, nil
}

func (p *ComfyUI) RegisterWebhook(context.Context, video.WebhookRegistration) error {
	return errors.New("ComfyUI provider does not support webhooks")
}
func (p *ComfyUI) VerifyWebhook(*http.Request) error {
	return errors.New("ComfyUI provider does not support webhooks")
}
func (p *ComfyUI) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, errors.New("ComfyUI provider does not support webhooks")
}

func (p *ComfyUI) doJSON(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode ComfyUI request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, comfyMaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > comfyMaxResponseBytes {
		return fmt.Errorf("ComfyUI response exceeds %d bytes", comfyMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ComfyUI API returned HTTP %d", resp.StatusCode)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode ComfyUI response: %w", err)
	}
	return nil
}

var _ video.VideoProvider = (*ComfyUI)(nil)
