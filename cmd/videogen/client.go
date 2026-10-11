package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

const maxAPIResponseBytes = 2 << 20

type apiClient struct {
	baseURL      string
	token        string
	http         *http.Client
	downloadHTTP *http.Client
}

type providerList struct {
	Providers []struct {
		ID string `json:"id"`
	} `json:"providers"`
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func newAPIClient(baseURL, token string, client *http.Client) (*apiClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("base URL must be an absolute http:// or https:// URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("base URL must not contain user info, query parameters, or a fragment")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("refusing to send a bearer token over non-loopback HTTP; use HTTPS")
		}
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("bearer token is required")
	}
	downloadClient := client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
		// Large video downloads can legitimately take longer than JSON API calls.
		// The request context still allows callers to cancel an in-progress transfer.
		downloadClient = &http.Client{}
	}
	return &apiClient{baseURL: baseURL, token: token, http: client, downloadHTTP: downloadClient}, nil
}

func (c *apiClient) Providers(ctx context.Context) (providerList, error) {
	var result providerList
	err := c.request(ctx, http.MethodGet, "/v1/video/providers", nil, &result)
	return result, err
}

func (c *apiClient) CreateJob(ctx context.Context, req video.VideoRequest) (video.VideoJob, error) {
	var result video.VideoJob
	err := c.request(ctx, http.MethodPost, "/v1/video/jobs", req, &result)
	return result, err
}

func (c *apiClient) DownloadAsset(ctx context.Context, jobID, assetID, outPath string) error {
	jobID = strings.TrimSpace(jobID)
	assetID = strings.TrimSpace(assetID)
	outPath = strings.TrimSpace(outPath)
	if jobID == "" || assetID == "" {
		return errors.New("job ID and asset ID are required")
	}
	if outPath == "" {
		return errors.New("output path is required")
	}
	absolutePath, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	directory := filepath.Dir(absolutePath)
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return fmt.Errorf("output directory does not exist: %s", directory)
	}
	if _, err := os.Stat(absolutePath); err == nil {
		return fmt.Errorf("output file already exists: %s", absolutePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check output path: %w", err)
	}
	endpoint := c.baseURL + "/v1/video/jobs/" + url.PathEscape(jobID) + "/assets/" + url.PathEscape(assetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build asset download request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream, video/*")
	req.Header.Set("Authorization", "Bearer "+c.token)
	client := c.downloadHTTP
	if client == nil {
		client = c.http
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Video Gateway asset download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
		if readErr == nil && len(data) <= maxAPIResponseBytes {
			var apiErr apiError
			if json.Unmarshal(data, &apiErr) == nil && strings.TrimSpace(apiErr.Message) != "" {
				if apiErr.Code != "" {
					return fmt.Errorf("Video Gateway HTTP %d (%s): %s", resp.StatusCode, apiErr.Code, apiErr.Message)
				}
				return fmt.Errorf("Video Gateway HTTP %d: %s", resp.StatusCode, apiErr.Message)
			}
		}
		return fmt.Errorf("Video Gateway HTTP %d while downloading asset", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(directory, ".nexaroute-download-*")
	if err != nil {
		return fmt.Errorf("create temporary download file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary download file: %w", err)
	}
	n, copyErr := io.Copy(tmp, resp.Body)
	if copyErr == nil && n == 0 {
		copyErr = errors.New("downloaded asset is empty")
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("write downloaded asset: %w", copyErr)
	}
	if _, err := os.Stat(absolutePath); err == nil {
		return fmt.Errorf("output file appeared during download; refusing to overwrite: %s", absolutePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recheck output path: %w", err)
	}
	if err := os.Rename(tmpName, absolutePath); err != nil {
		return fmt.Errorf("publish downloaded asset: %w", err)
	}
	return nil
}

func (c *apiClient) GetJob(ctx context.Context, id string) (video.VideoJob, error) {
	if strings.TrimSpace(id) == "" {
		return video.VideoJob{}, errors.New("job ID is required")
	}
	var result video.VideoJob
	err := c.request(ctx, http.MethodGet, "/v1/video/jobs/"+url.PathEscape(id), nil, &result)
	return result, err
}

func (c *apiClient) CancelJob(ctx context.Context, id string) (video.VideoJob, error) {
	if strings.TrimSpace(id) == "" {
		return video.VideoJob{}, errors.New("job ID is required")
	}
	var result video.VideoJob
	err := c.request(ctx, http.MethodPost, "/v1/video/jobs/"+url.PathEscape(id)+"/cancel", nil, &result)
	return result, err
}

func (c *apiClient) request(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Video Gateway request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Video Gateway response: %w", err)
	}
	if len(data) > maxAPIResponseBytes {
		return errors.New("Video Gateway response exceeds 2 MiB limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		if json.Unmarshal(data, &apiErr) == nil && strings.TrimSpace(apiErr.Message) != "" {
			if apiErr.Code != "" {
				return fmt.Errorf("Video Gateway HTTP %d (%s): %s", resp.StatusCode, apiErr.Code, apiErr.Message)
			}
			return fmt.Errorf("Video Gateway HTTP %d: %s", resp.StatusCode, apiErr.Message)
		}
		return fmt.Errorf("Video Gateway HTTP %d", resp.StatusCode)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode Video Gateway response: %w", err)
	}
	return nil
}
