package wazuh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Wazuh API error codes this client distinguishes.
const (
	apiErrAgentNotFound = 1701
	apiErrNameExists    = 1705
)

// HTTPManager talks to the Wazuh manager API (default port 55000). Client
// must trust the manager's certificate; credentials belong to an API user
// limited to agent create/read/delete.
type HTTPManager struct {
	BaseURL  string // such as https://wazuh-manager:55000
	Client   *http.Client
	Username string
	Password string

	mu    sync.Mutex
	token string
}

type apiResponse struct {
	Data struct {
		Token         string            `json:"token"`
		ID            string            `json:"id"`
		Key           string            `json:"key"`
		AffectedItems []json.RawMessage `json:"affected_items"`
		FailedItems   []struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		} `json:"failed_items"`
	} `json:"data"`
	Error  int    `json:"error"`
	Detail string `json:"detail"`
}

type apiError struct {
	status int
	code   int
	detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("wazuh API: HTTP %d, error %d: %s", e.status, e.code, e.detail)
}

func (m *HTTPManager) authenticate(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/security/user/authenticate", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(m.Username, m.Password)
	var resp apiResponse
	if err := m.send(req, &resp); err != nil {
		return "", fmt.Errorf("authenticating to the Wazuh API: %w", err)
	}
	if resp.Data.Token == "" {
		return "", errors.New("wazuh API returned no token")
	}
	return resp.Data.Token, nil
}

func (m *HTTPManager) send(req *http.Request, out *apiResponse) error {
	res, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	*out = apiResponse{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("wazuh API: HTTP %d: undecodable response", res.StatusCode)
		}
	}
	if res.StatusCode != http.StatusOK || out.Error != 0 && len(out.Data.FailedItems) == 0 {
		return &apiError{status: res.StatusCode, code: out.Error, detail: out.Detail}
	}
	return nil
}

// call sends an authenticated request, re-authenticating once on 401.
func (m *HTTPManager) call(ctx context.Context, method, path string, query url.Values, body any) (apiResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return apiResponse{}, err
		}
	}
	for attempt := 0; ; attempt++ {
		m.mu.Lock()
		token := m.token
		m.mu.Unlock()
		if token == "" {
			var err error
			if token, err = m.authenticate(ctx); err != nil {
				return apiResponse{}, err
			}
			m.mu.Lock()
			m.token = token
			m.mu.Unlock()
		}
		target := m.BaseURL + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return apiResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		var resp apiResponse
		err = m.send(req, &resp)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusUnauthorized && attempt == 0 {
			m.mu.Lock()
			m.token = ""
			m.mu.Unlock()
			continue
		}
		return resp, err
	}
}

func checkID(id string) error {
	if !agentIDRE.MatchString(id) {
		return fmt.Errorf("invalid agent ID %q", id)
	}
	return nil
}

// Register adds an agent through the manager's authd (POST /agents).
func (m *HTTPManager) Register(ctx context.Context, name string) (string, error) {
	resp, err := m.call(ctx, http.MethodPost, "/agents", nil, map[string]string{"name": name, "ip": "any"})
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == apiErrNameExists {
		return "", fmt.Errorf("%w: %s", ErrNameExists, apiErr.detail)
	}
	if err != nil {
		return "", err
	}
	return resp.Data.ID, nil
}

// Key reads an agent's key (GET /agents/{id}/key).
func (m *HTTPManager) Key(ctx context.Context, id string) (string, error) {
	if err := checkID(id); err != nil {
		return "", err
	}
	resp, err := m.call(ctx, http.MethodGet, "/agents/"+id+"/key", nil, nil)
	if err != nil {
		return "", err
	}
	var items []struct{ ID, Key string }
	if err := decodeItems(resp, &items); err != nil {
		return "", err
	}
	if len(items) != 1 || items[0].ID != id {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return items[0].Key, nil
}

func decodeItems(resp apiResponse, out any) error {
	for _, failed := range resp.Data.FailedItems {
		if failed.Error.Code == apiErrAgentNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("wazuh API: error %d", failed.Error.Code)
	}
	raw, err := json.Marshal(resp.Data.AffectedItems)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (m *HTTPManager) agents(ctx context.Context, query url.Values) (Agent, error) {
	query.Set("select", "id,name,status,dateAdd")
	resp, err := m.call(ctx, http.MethodGet, "/agents", query, nil)
	if err != nil {
		return Agent{}, err
	}
	var items []struct {
		ID, Name, Status string
		DateAdd          string `json:"dateAdd"`
	}
	if err := decodeItems(resp, &items); err != nil {
		return Agent{}, err
	}
	switch len(items) {
	case 0:
		return Agent{}, ErrNotFound
	case 1:
	default:
		return Agent{}, fmt.Errorf("wazuh API returned %d agents for %s", len(items), query.Encode())
	}
	added, err := time.Parse(time.RFC3339, items[0].DateAdd)
	if err != nil {
		return Agent{}, fmt.Errorf("agent %s has invalid dateAdd %q", items[0].ID, items[0].DateAdd)
	}
	return Agent{ID: items[0].ID, Name: items[0].Name, Status: items[0].Status, Added: added}, nil
}

func (m *HTTPManager) Agent(ctx context.Context, id string) (Agent, error) {
	if err := checkID(id); err != nil {
		return Agent{}, err
	}
	agent, err := m.agents(ctx, url.Values{"agents_list": {id}})
	if err == nil && agent.ID != id {
		return Agent{}, fmt.Errorf("wazuh API returned agent %s for %s", agent.ID, id)
	}
	return agent, err
}

func (m *HTTPManager) AgentByName(ctx context.Context, name string) (Agent, error) {
	agent, err := m.agents(ctx, url.Values{"name": {name}})
	if err == nil && agent.Name != name {
		return Agent{}, fmt.Errorf("wazuh API returned agent %q for %q", agent.Name, name)
	}
	return agent, err
}

// Delete removes one agent whatever its status (DELETE /agents).
func (m *HTTPManager) Delete(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	resp, err := m.call(ctx, http.MethodDelete, "/agents", url.Values{"agents_list": {id}, "status": {"all"}, "older_than": {"0s"}}, nil)
	if err != nil {
		return err
	}
	var deleted []string
	switch err := decodeItems(resp, &deleted); {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if len(deleted) != 1 || strings.TrimSpace(deleted[0]) != id {
		return fmt.Errorf("wazuh API deleted %v, not %s", deleted, id)
	}
	return nil
}
