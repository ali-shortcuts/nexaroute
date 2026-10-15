package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Media passthrough endpoints (M5-1c): embeddings, image
// generations, text-to-speech, transcriptions and rerank. These are
// OpenAI-wire APIs with no canonical-IR translation — the gateway
// routes them by model to a deployment of an openai_compatible
// provider and passes the payload through with the deployment's
// model substituted, mirroring chat's failover, health recording and
// error semantics. Per-cloud media addressing belongs at the
// mediaUpstreamPath seam (documented follow-ups).

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
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	req := router.Requirement{Model: model}
	_, candidates, _, resolveErr := s.candidatesForRequirement(req, "openai")
	if resolveErr != nil {
		if strings.Contains(resolveErr.Error(), "disabled") {
			errorJSON(w, http.StatusNotFound, resolveErr.Error())
		} else {
			errorJSON(w, http.StatusBadRequest, resolveErr.Error())
		}
		return
	}
	if len(candidates) == 0 {
		errorJSON(w, http.StatusServiceUnavailable, "no compatible healthy deployment")
		return
	}

	var lastStatus int
	var lastContentType string
	var lastBody []byte
	var lastErr string
	sawEligible := false
	sawSupported := false

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
		if c.Deployment.ProviderType != "openai_compatible" {
			// Passthrough is defined for the OpenAI wire family only;
			// never mangle another protocol's payload into it.
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
		resp, derr := a.DoPath(r.Context(), http.MethodPost, mediaUpstreamPath(p, kind), payload, false, forward)
		latency := time.Since(start)
		if derr != nil {
			lastErr = derr.Error()
			s.hm.RecordFailure(c.Deployment.ID, lastErr, latency)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.hm.RecordSuccess(c.Deployment.ID, latency)
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
		lastStatus, lastContentType, lastBody = resp.StatusCode, resp.Header.Get("Content-Type"), body
		lastErr = fmt.Sprintf("upstream http %d", resp.StatusCode)
	}

	if sawEligible && !sawSupported {
		errorJSON(w, http.StatusBadRequest,
			"media endpoints are supported only for openai_compatible providers in this release")
		return
	}
	if !sawEligible && lastStatus == 0 && lastErr == "" {
		// Every candidate fell out of eligibility between resolution
		// and dispatch (e.g. the model is not served after all).
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
