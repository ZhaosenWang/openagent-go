package wasmhost

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// NewHostAPI constructs a HostAPI with the given keyring and sensible
// defaults for HTTP (net/http) and logging (standard log adapter).
func NewHostAPI(kr Keyring) *HostAPI {
	return &HostAPI{
		Keyring: kr,
		HTTP:    NewHTTPClient(),
		Logger:  &logAdapter{},
	}
}

// NewHTTPClient returns an HTTPClient backed by net/http's default client.
// Exported so non-HostAPI callers (e.g. the CLI plugin runtime) share one
// implementation instead of each vendoring its own net/http wrapper.
func NewHTTPClient() HTTPClient {
	return &defaultHTTPClient{client: http.DefaultClient}
}

// defaultHTTPClient implements HTTPClient via net/http.
type defaultHTTPClient struct{ client *http.Client }

func (c *defaultHTTPClient) Do(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// logAdapter implements Logger by forwarding to the standard log package.
type logAdapter struct{}

func (l *logAdapter) Info(msg string)  { slog.Info(msg, "source", "plugin") }
func (l *logAdapter) Warn(msg string)  { slog.Warn(msg, "source", "plugin") }
func (l *logAdapter) Error(msg string) { slog.Error(msg, "source", "plugin") }
