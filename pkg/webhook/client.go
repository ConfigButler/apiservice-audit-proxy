package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const maxErrorResponseBody = 4096

// Sender posts audit EventList payloads to a webhook backend.
type Sender interface {
	Send(context.Context, auditv1.EventList) error
}

// Client is a kubeconfig-backed audit webhook sender.
type Client struct {
	endpoint *url.URL
	client   *http.Client
}

// NewClientFromKubeconfig builds an HTTP client from a kubeconfig-style webhook
// configuration, including mTLS and CA trust.
func NewClientFromKubeconfig(path string, timeout time.Duration) (*Client, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}

	transport, err := rest.TransportFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build transport: %w", err)
	}

	endpoint, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("parse webhook host: %w", err)
	}

	return &Client{
		endpoint: endpoint,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

// Send posts one EventList to the configured webhook endpoint.
func (c *Client) Send(ctx context.Context, eventList auditv1.EventList) error {
	body, err := json.Marshal(eventList)
	if err != nil {
		return fmt.Errorf("marshal event list: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook payload: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBody))
	return fmt.Errorf("webhook responded with %s: %s", resp.Status, string(responseBody))
}
