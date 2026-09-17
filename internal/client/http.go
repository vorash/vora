package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// HTTPClient talks to vora-api over HTTP/JSON with a bearer API key.
type HTTPClient struct {
	base string
	key  string
	hc   *http.Client
}

// DialAPI returns a client for a vora-api base URL (e.g. http://host:9300).
func DialAPI(baseURL, apiKey string) (*HTTPClient, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("client: empty API URL")
	}
	return &HTTPClient{
		base: strings.TrimSuffix(baseURL, "/"),
		key:  apiKey,
		hc:   &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// Close implements API.
func (c *HTTPClient) Close() error { return nil }

func (c *HTTPClient) do(ctx context.Context, method, path string, body, out proto.Message) error {
	var r io.Reader
	if body != nil {
		data, err := protojson.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeHTTPError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, out)
}

// CreateSandbox creates and starts a sandbox through vora-api.
func (c *HTTPClient) CreateSandbox(ctx context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error) {
	sb := &v1.Sandbox{}
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", spec, sb); err != nil {
		return nil, err
	}
	return sb, nil
}

// StartSandbox returns the now-running sandbox (the API starts it on create).
func (c *HTTPClient) StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error) {
	return c.GetSandbox(ctx, id)
}

// GetSandbox fetches a sandbox.
func (c *HTTPClient) GetSandbox(ctx context.Context, id string) (*v1.Sandbox, error) {
	sb := &v1.Sandbox{}
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id, nil, sb); err != nil {
		return nil, err
	}
	return sb, nil
}

// ListSandboxes lists sandboxes.
func (c *HTTPClient) ListSandboxes(ctx context.Context, includeDestroyed bool) ([]*v1.Sandbox, error) {
	path := "/v1/sandboxes"
	if includeDestroyed {
		path += "?include_destroyed=true"
	}
	resp := &v1.ListSandboxesResponse{}
	if err := c.do(ctx, http.MethodGet, path, nil, resp); err != nil {
		return nil, err
	}
	return resp.GetSandboxes(), nil
}

// DestroySandbox destroys a sandbox.
func (c *HTTPClient) DestroySandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
}

// StatsSandbox returns resource usage.
func (c *HTTPClient) StatsSandbox(ctx context.Context, id string) (*v1.Stats, error) {
	st := &v1.Stats{}
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/stats", nil, st); err != nil {
		return nil, err
	}
	return st, nil
}

// Events returns lifecycle events.
func (c *HTTPClient) Events(ctx context.Context, id string) ([]*v1.SandboxEvent, error) {
	resp := &v1.GetSandboxEventsResponse{}
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/events", nil, resp); err != nil {
		return nil, err
	}
	return resp.GetEvents(), nil
}

// Execs returns execution audit records.
func (c *HTTPClient) Execs(ctx context.Context, id string) ([]*v1.SandboxExec, error) {
	resp := &v1.GetSandboxExecsResponse{}
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/execs", nil, resp); err != nil {
		return nil, err
	}
	return resp.GetExecs(), nil
}

// Exec starts a command and returns an NDJSON-backed stream.
func (c *HTTPClient) Exec(ctx context.Context, req *v1.ExecSandboxRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error) {
	data, err := protojson.Marshal(req.GetExec())
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/v1/sandboxes/"+req.GetSandboxId()+"/exec", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeHTTPError(resp)
	}
	return &httpExecStream{body: resp.Body}, nil
}

// httpExecStream adapts the NDJSON response to grpc.ServerStreamingClient.
type httpExecStream struct {
	grpc.ClientStream
	body io.ReadCloser
	sc   *bufio.Scanner
}

func (s *httpExecStream) Recv() (*v1.ExecChunk, error) {
	if s.sc == nil {
		s.sc = bufio.NewScanner(s.body)
		s.sc.Buffer(make([]byte, 1<<20), 1<<20)
	}
	for s.sc.Scan() {
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		chunk := &v1.ExecChunk{}
		if err := protojson.Unmarshal(line, chunk); err != nil {
			return nil, err
		}
		return chunk, nil
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func decodeHTTPError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		msg = body.Error
	}
	return fmt.Errorf("api: %s", msg)
}

// ---------------------------------------------------------------------------
// Control-plane resources (runners, keys)
// ---------------------------------------------------------------------------

// RunnerInfo is a registered runner as reported by vora-api.
type RunnerInfo struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Backends      []string  `json:"backends"`
	Running       int32     `json:"sandboxes_running"`
	Version       string    `json:"version"`
	Healthy       bool      `json:"healthy"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// KeyInfo is an API key (never the plaintext).
type KeyInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TenantID  string    `json:"tenant_id"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// ListRunners returns the registered runners.
func (c *HTTPClient) ListRunners(ctx context.Context) ([]RunnerInfo, error) {
	var out struct {
		Runners []RunnerInfo `json:"runners"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runners", nil, &out); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// CreateKey creates an API key and returns the plaintext (shown once).
func (c *HTTPClient) CreateKey(ctx context.Context, name, tenant string, scopes []string) (string, KeyInfo, error) {
	req := map[string]any{"name": name, "tenant_id": tenant, "scopes": scopes}
	var out struct {
		Key    string  `json:"key"`
		APIKey KeyInfo `json:"api_key"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/keys", req, &out); err != nil {
		return "", KeyInfo{}, err
	}
	return out.Key, out.APIKey, nil
}

// ListKeys returns API keys for a tenant (empty means the caller's tenant).
func (c *HTTPClient) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	var out struct {
		Keys []KeyInfo `json:"keys"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/keys", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// RevokeKey revokes an API key.
func (c *HTTPClient) RevokeKey(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/keys/"+id, nil, nil)
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeHTTPError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}