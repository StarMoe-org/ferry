// Package kube is a minimal Kubernetes REST client: bearer token, custom CA, JSON in and out.
// Ferry only lists and patches a handful of resource types, which doesn't justify client-go.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Config is how to reach one cluster.
type Config struct {
	Name      string `json:"name"`
	Server    string `json:"server,omitempty"`
	CAData    string `json:"caData,omitempty"` // base64 of a PEM bundle
	CAFile    string `json:"caFile,omitempty"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
	// InCluster uses the pod's own ServiceAccount and the cluster it runs in.
	InCluster bool `json:"inCluster,omitempty"`
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type Client struct {
	Name      string
	server    string
	hc        *http.Client
	token     string
	tokenFile string

	mu      sync.Mutex
	fileTok string
	readAt  time.Time
}

func New(c Config) (*Client, error) {
	if c.InCluster {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("inCluster: not running in a cluster")
		}
		c.Server = "https://" + net.JoinHostPort(host, port)
		if c.CAFile == "" && c.CAData == "" {
			c.CAFile = saDir + "/ca.crt"
		}
		if c.Token == "" && c.TokenFile == "" {
			c.TokenFile = saDir + "/token"
		}
	}
	if c.Server == "" {
		return nil, fmt.Errorf("cluster %q: server is required", c.Name)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	var pem []byte
	switch {
	case c.CAData != "":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.CAData))
		if err != nil {
			return nil, fmt.Errorf("cluster %q: caData: %w", c.Name, err)
		}
		pem = b
	case c.CAFile != "":
		b, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", c.Name, err)
		}
		pem = b
	}
	if pem != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cluster %q: no certificates in the CA bundle", c.Name)
		}
		tlsConf.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	return &Client{
		Name:      c.Name,
		server:    strings.TrimRight(c.Server, "/"),
		hc:        &http.Client{Transport: tr, Timeout: 30 * time.Second},
		token:     c.Token,
		tokenFile: c.TokenFile,
	}, nil
}

func (c *Client) bearer() string {
	if c.tokenFile == "" {
		return c.token
	}
	// Projected ServiceAccount tokens rotate; re-read now and then
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.readAt) > time.Minute {
		if b, err := os.ReadFile(c.tokenFile); err == nil {
			c.fileTok, c.readAt = strings.TrimSpace(string(b)), time.Now()
		}
	}
	return c.fileTok
}

// StatusError is a non-2xx answer from the API server.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

func IsConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}

func IsForbidden(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

func IsUnprocessable(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusUnprocessableEntity
}

// ServerHost returns the API server host[:port] with scheme and any userinfo stripped.
func (c *Client) ServerHost() string {
	u, err := url.Parse(c.server)
	if err != nil || u.Host == "" {
		return c.server
	}
	return u.Host
}

// ValidName reports whether s is a valid Kubernetes resource or namespace name
// (RFC 1123 DNS subdomain: lowercase alphanumeric, '-' or '.', 1..253 chars),
// safe to interpolate into API URL paths.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 253 || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if i == 0 || i == len(s)-1 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && ch != '-' && ch != '.' {
			return false
		}
	}
	return true
}

// Do sends body (marshalled unless it is already []byte) and decodes the answer into out, if given.
func (c *Client) Do(ctx context.Context, method, path string, body any, contentType string, out any) error {
	var rd io.Reader
	if body != nil {
		b, ok := body.([]byte)
		if !ok {
			var err error
			if b, err = json.Marshal(body); err != nil {
				return err
			}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, rd)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if t := c.bearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			msg = st.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &StatusError{Code: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, "", out)
}

func (c *Client) Patch(ctx context.Context, path, patchType string, body, out any) error {
	return c.Do(ctx, http.MethodPatch, path, body, patchType, out)
}

// Version is the API server's git version, for example v1.34.1+k3s1.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	err := c.Get(ctx, "/version", &v)
	return v.GitVersion, err
}
