package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Media passthrough endpoints (M5-1c): embeddings, image
// generations, text-to-speech, transcriptions and rerank. These are
// OpenAI-wire APIs with no canonical-IR translation — the gateway
// routes them by model to a deployment of an openai_compatible or
// azure_openai provider and passes the payload through with the
// deployment's model substituted, mirroring chat's failover, health
// recording and error semantics. Azure deployments use native
// per-deployment URLs (resolveMediaUpstream/azureMediaPath, same
// deployment/api-version semantics as chat); Vertex/Bedrock and other
// families have no byte-compatible native equivalent and are refused
// explicitly rather than guessed.

const (
	maxMediaBodyBytes     = 32 << 20 // audio uploads exceed the JSON cap
	maxMediaResponseBytes = 64 << 20 // generated audio/image payloads
)

type mediaKind string

const (
	mediaEmbeddings     mediaKind = "embeddings"
	mediaImages         mediaKind = "images"
	mediaSpeech         mediaKind = "speech"
	mediaTranscriptions mediaKind = "transcriptions"
	mediaRerank         mediaKind = "rerank"
)

func (k mediaKind) binaryResponse() bool { return k == mediaSpeech }
func (k mediaKind) multipart() bool      { return k == mediaTranscriptions }

func (s *Server) mediaEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, mediaEmbeddings)
}
func (s *Server) mediaImages(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, mediaImages)
}
func (s *Server) mediaSpeech(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, mediaSpeech)
}
func (s *Server) mediaTranscriptions(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, mediaTranscriptions)
}
func (s *Server) mediaRerank(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, mediaRerank)
}

// mediaUpstreamPath resolves the per-kind upstream path for one
// provider — the seam where per-cloud media addressing plugs in.
func mediaUpstreamPath(p config.ProviderConfig, kind mediaKind) string {
	switch kind {
	case mediaEmbeddings:
		return p.EmbeddingsPath
	case mediaImages:
		return p.ImagesPath
	case mediaSpeech:
		return p.SpeechPath
	case mediaTranscriptions:
		return p.TranscriptionsPath
	case mediaRerank:
		return p.RerankPath
	}
	return ""
}

// resolveMediaUpstream resolves provider-native addressing for one
// candidate deployment, mirroring how the canonical adapter addresses
// chat/messages for the same provider type (deployment/resource/
// location semantics as applicable per cloud).
//
//   - openai_compatible: per-kind provider path fields with OpenAI
//     defaults (byte-identical to the original M5-1c behavior).
//   - azure_openai: per-deployment URLs of the form
//     /openai/deployments/{deployment}/{family}?api-version=…,
//     exactly like azureDeploymentPath for chat. Only families with a
//     documented Azure OpenAI equivalent are supported (embeddings,
//     images/generations, audio/speech, audio/transcriptions); rerank
//     has no Azure equivalent and is refused.
//   - vertex / bedrock / gemini / anthropic_compatible /
//     openai_responses / anything else: no byte-compatible native
//     media equivalent exists (Vertex speaks Gemini embedContent,
//     Bedrock speaks model-specific InvokeModel JSON — neither is the
//     OpenAI wire shape this passthrough preserves), so every kind is
//     refused rather than sending a mangled payload upstream.
//
// It returns ok=false for unsupported combinations; the caller skips
// the candidate and, when no candidate is supported, refuses with the
// same clear unsupported-capability 400 used elsewhere.
func resolveMediaUpstream(p config.ProviderConfig, kind mediaKind, deploymentModel string) (string, bool) {
	switch p.Type {
	case "openai_compatible":
		path := mediaUpstreamPath(p, kind)
		if path == "" {
			return "", false
		}
		return path, true
	case "azure_openai":
		return azureMediaPath(p, kind, deploymentModel)
	default:
		return "", false
	}
}

// azureMediaPath builds the Azure OpenAI per-deployment media endpoint
// with its api-version query. The model argument is the Azure
// deployment name (the dispatch layer has already substituted the
// deployment's upstream model, same as azureDeploymentPath for chat).
func azureMediaPath(p config.ProviderConfig, kind mediaKind, deploymentModel string) (string, bool) {
	dep := strings.TrimSpace(deploymentModel)
	if dep == "" {
		return "", false
	}
	version := p.APIVersion
	if version == "" {
		version = "2024-10-21"
	}
	base := "/openai/deployments/" + url.PathEscape(dep)
	suffix := "?api-version=" + url.QueryEscape(version)
	switch kind {
	case mediaEmbeddings:
		return base + "/embeddings" + suffix, true
	case mediaImages:
		return base + "/images/generations" + suffix, true
	case mediaSpeech:
		return base + "/audio/speech" + suffix, true
	case mediaTranscriptions:
		return base + "/audio/transcriptions" + suffix, true
	case mediaRerank:
		// Azure OpenAI has no rerank equivalent; refuse explicitly
		// rather than guessing a URL shape.
		return "", false
	}
	return "", false
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, kind mediaKind) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, false) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMediaBodyBytes+1))
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "request body read failed")
		return
	}
	if len(raw) > maxMediaBodyBytes {
		errorJSON(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds %d bytes", maxMediaBodyBytes))
		return
	}

	model, err := mediaRequestModel(r, raw, kind)
	if err != nil {
		// Gateway-side rejection before any upstream attempt: release
		// the admission budget hold (Wave C-4).
		s.budgetRelease(r)
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	req := router.Requirement{Model: model}
	_, candidates, _, resolveErr := s.candidatesForRequirement(req, "openai")
	if resolveErr != nil {
		// Gateway-side rejection before any upstream attempt: release
		// the admission budget hold (Wave C-4).
		s.budgetRelease(r)
		if strings.Contains(resolveErr.Error(), "disabled") {
			errorJSON(w, http.StatusNotFound, resolveErr.Error())
		} else {
			errorJSON(w, http.StatusBadRequest, resolveErr.Error())
		}
		return
	}
	if len(candidates) == 0 {
		// Gateway-side rejection before any upstream attempt: release
		// the admission budget hold (Wave C-4).
		s.budgetRelease(r)
		errorJSON(w, http.StatusServiceUnavailable, "no compatible healthy deployment")
		return
	}

	var lastStatus int
	var lastContentType string
	var lastBody []byte
	var lastErr string
	sawEligible := false
	sawSupported := false
	// attemptedUpstream tracks whether any upstream call was made: the
	// admission budget hold is released on tail returns only when the
	// gateway can prove zero upstream spend (Wave C-4). When an attempt
	// reached upstream, the documented TTL-expiry behavior applies.
	attemptedUpstream := false

	for i := range candidates {
		c := candidates[i]
		fresh, a, ok := s.currentRouteCandidate(c.Deployment.ID, req)
		if !ok {
			continue
		}
		c = fresh
		p, ok := s.providerConfigFor(c.Deployment.ProviderID)
		if !ok {
			continue
		}
		sawEligible = true
		upstreamPath, supported := resolveMediaUpstream(p, kind, c.Deployment.Model)
		if !supported {
			// Per-cloud media addressing: Azure deployment URLs where
			// a native equivalent exists, explicit refusal elsewhere
			// (Vertex/Bedrock wire shapes differ; Azure rerank has no
			// equivalent). Never mangle another protocol's payload
			// into a guessed URL.
			continue
		}
		sawSupported = true

		payload := raw
		forward := http.Header{}
		if kind.multipart() {
			rewritten, ct, rerr := rewriteMultipartModel(raw, r.Header.Get("Content-Type"), c.Deployment.Model)
			if rerr != nil {
				errorJSON(w, http.StatusBadRequest, rerr.Error())
				return
			}
			payload = rewritten
			forward.Set("Content-Type", ct)
		} else {
			patched, perr := patchJSONModel(raw, c.Deployment.Model)
			if perr != nil {
				errorJSON(w, http.StatusBadRequest, "request body must be valid JSON with a model field")
				return
			}
			payload = patched
			forward.Set("Content-Type", "application/json")
		}

		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID})
		start := time.Now()
		attemptedUpstream = true
		resp, derr := a.DoPath(r.Context(), http.MethodPost, upstreamPath, payload, false, forward)
		latency := time.Since(start)
		if derr != nil {
			lastErr = derr.Error()
			s.hm.RecordFailure(c.Deployment.ID, lastErr, latency)
			s.observeCompletedTelemetry(c.Deployment.ID, c.Deployment.ProviderID, false, 0, 0)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.hm.RecordSuccess(c.Deployment.ID, latency)
			s.observeCompletedTelemetry(c.Deployment.ID, c.Deployment.ProviderID, true, latency, 0)
			w.Header().Set("X-Gateway-Deployment", c.Deployment.ID)
			w.Header().Set("X-Gateway-Provider", c.Deployment.ProviderID)
			w.Header().Set("X-Gateway-Upstream-Model", c.Deployment.Model)
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			if kind.binaryResponse() {
				// Binary payloads (audio) stream through untouched.
				_, _ = io.Copy(w, io.LimitReader(resp.Body, maxMediaResponseBytes))
				resp.Body.Close()
			} else {
				body, rerr := readJSONLimited(resp.Body)
				resp.Body.Close()
				if rerr == nil {
					_, _ = w.Write(body)
					if kind == mediaEmbeddings {
						if in, _ := embeddingsUsage(body); in > 0 {
							s.recordUsage(r, c.Deployment.ID, int64(in), 0)
						}
					}
				}
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, LatencyMS: latency.Milliseconds(), StatusCode: resp.StatusCode})
			return
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		s.hm.RecordFailure(c.Deployment.ID, fmt.Sprintf("upstream http %d", resp.StatusCode), latency)
		s.observeCompletedTelemetry(c.Deployment.ID, c.Deployment.ProviderID, false, 0, 0)
		lastStatus, lastContentType, lastBody = resp.StatusCode, resp.Header.Get("Content-Type"), body
		lastErr = fmt.Sprintf("upstream http %d", resp.StatusCode)
	}

	if sawEligible && !sawSupported {
		// No upstream attempt was possible: release the admission
		// budget hold (Wave C-4).
		s.budgetRelease(r)
		errorJSON(w, http.StatusBadRequest,
			"media endpoints are supported only for openai_compatible and azure_openai providers in this release")
		return
	}
	if !sawEligible && lastStatus == 0 && lastErr == "" {
		// Every candidate fell out of eligibility between resolution
		// and dispatch (e.g. the model is not served after all), so no
		// upstream attempt was made: release the admission budget hold
		// (Wave C-4).
		s.budgetRelease(r)
		errorJSON(w, http.StatusServiceUnavailable, "no compatible healthy deployment")
		return
	}
	if lastStatus != 0 {
		writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
		return
	}
	if lastErr == "" {
		lastErr = "no compatible healthy deployment"
	}
	if !attemptedUpstream {
		// No upstream attempt was made: release the admission budget
		// hold (Wave C-4).
		s.budgetRelease(r)
	}
	errorJSON(w, http.StatusBadGateway, lastErr)
}

// mediaRequestModel extracts the routing model: the JSON "model"
// field, or the multipart form's model field for transcriptions.
func mediaRequestModel(r *http.Request, raw []byte, kind mediaKind) (string, error) {
	if kind.multipart() {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || params["boundary"] == "" {
			return "", fmt.Errorf("transcriptions require a multipart/form-data request")
		}
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("invalid multipart body: %w", err)
			}
			if part.FormName() == "model" {
				b, _ := io.ReadAll(io.LimitReader(part, 4096))
				part.Close()
				if m := strings.TrimSpace(string(b)); m != "" {
					return m, nil
				}
				return "", fmt.Errorf("model form field is empty")
			}
			part.Close()
		}
		return "", fmt.Errorf("model form field is required")
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	if strings.TrimSpace(probe.Model) == "" {
		return "", fmt.Errorf("model is required")
	}
	return probe.Model, nil
}

// rewriteMultipartModel rebuilds a multipart body with the model
// form field replaced by the deployment model. File parts and their
// bytes pass through untouched; part order is preserved.
func rewriteMultipartModel(raw []byte, contentType, model string) ([]byte, string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", fmt.Errorf("invalid multipart content type")
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	replaced := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("invalid multipart body: %w", err)
		}
		if part.FormName() == "model" && part.FileName() == "" {
			if err := mw.WriteField("model", model); err != nil {
				return nil, "", err
			}
			replaced = true
			part.Close()
			continue
		}
		var dst io.Writer
		if fn := part.FileName(); fn != "" {
			dst, err = mw.CreateFormFile(part.FormName(), fn)
		} else {
			dst, err = mw.CreateFormField(part.FormName())
		}
		if err != nil {
			return nil, "", err
		}
		if _, err := io.Copy(dst, part); err != nil {
			return nil, "", err
		}
		part.Close()
	}
	if !replaced {
		return nil, "", fmt.Errorf("model form field is required")
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// embeddingsUsage extracts prompt token usage from an embeddings
// response ({"usage":{"prompt_tokens":N,...}}).
func embeddingsUsage(body []byte) (int, error) {
	var probe struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, err
	}
	if probe.Usage.PromptTokens > 0 {
		return probe.Usage.PromptTokens, nil
	}
	return probe.Usage.TotalTokens, nil
}
