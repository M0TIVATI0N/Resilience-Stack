// Package docker is a minimal client for the Docker Engine API over its unix
// socket. Using the API directly keeps the agent free of third-party modules.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrNotFound means the requested container does not exist.
var ErrNotFound = errors.New("container not found")

// Container is the subset of the inspect response the agent uses.
type Container struct {
	Name   string
	Image  string
	Status string
	Health string
	// OOMKilled reports whether the kernel killed the container for memory.
	OOMKilled bool
	// Raw is the full inspect document, used to clone a container's settings.
	Raw map[string]any
}

// Event is one message from the Docker event stream.
type Event struct {
	Action string
	Name   string
	Labels map[string]string
}

// Client talks to the local Docker daemon.
type Client struct {
	http *http.Client
}

// New creates a client for the given socket path.
func New(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	target := "http://docker" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// call performs a request and treats the listed status codes as success.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any, ok ...int) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	for _, code := range ok {
		if resp.StatusCode != code {
			continue
		}
		if out != nil && code != http.StatusNoContent && code != http.StatusNotModified {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(detail)))
}

// Inspect returns a container's current state.
func (c *Client) Inspect(ctx context.Context, name string) (Container, error) {
	var raw map[string]any
	if err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &raw, http.StatusOK); err != nil {
		return Container{}, err
	}
	info := Container{Name: strings.TrimPrefix(asString(raw["Name"]), "/"), Raw: raw}
	if cfg, ok := raw["Config"].(map[string]any); ok {
		info.Image = asString(cfg["Image"])
	}
	if st, ok := raw["State"].(map[string]any); ok {
		info.Status = asString(st["Status"])
		info.OOMKilled, _ = st["OOMKilled"].(bool)
		if health, ok := st["Health"].(map[string]any); ok {
			info.Health = asString(health["Status"])
		}
	}
	return info, nil
}

// Start starts a container; starting a running container is not an error.
func (c *Client) Start(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil, nil,
		http.StatusNoContent, http.StatusNotModified)
}

// Stop gracefully stops a container; stopping a stopped one is not an error.
func (c *Client) Stop(ctx context.Context, name string, timeoutSeconds int) error {
	query := url.Values{"t": {strconv.Itoa(timeoutSeconds)}}
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop", query, nil, nil,
		http.StatusNoContent, http.StatusNotModified)
}

// Remove force-removes a container; a missing container is not an error.
func (c *Client) Remove(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil,
		http.StatusNoContent)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Create makes a new container from an Engine API creation document.
func (c *Client) Create(ctx context.Context, name string, spec map[string]any) error {
	return c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, spec, nil, http.StatusCreated)
}

// Signal sends a signal to a container's main process.
func (c *Client) Signal(ctx context.Context, name, signal string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/kill", url.Values{"signal": {signal}}, nil, nil,
		http.StatusNoContent)
}

// Exec runs a command inside a running container and returns its exit code and
// combined output.
func (c *Client) Exec(ctx context.Context, name string, cmd ...string) (int, string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	spec := map[string]any{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true}
	if err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", nil, spec, &created, http.StatusCreated); err != nil {
		return 0, "", err
	}

	resp, err := c.do(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return 0, "", fmt.Errorf("docker exec start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("docker exec start: %s", resp.Status)
	}
	output, err := demux(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("read exec output: %w", err)
	}

	var result struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := c.call(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &result, http.StatusOK); err != nil {
		return 0, "", err
	}
	return result.ExitCode, output, nil
}

// demux flattens Docker's multiplexed stdout/stderr stream.
func demux(r io.Reader) (string, error) {
	var out strings.Builder
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) {
				return out.String(), nil
			}
			return out.String(), err
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(&out, r, size); err != nil {
			return out.String(), err
		}
	}
}

// Events streams container events carrying the label until ctx ends or the
// connection drops. It always returns a non-nil error.
func (c *Client) Events(ctx context.Context, label string, actions []string, handle func(Event)) error {
	filters, err := json.Marshal(map[string][]string{
		"type":  {"container"},
		"event": actions,
		"label": {label},
	})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodGet, "/events", url.Values{"filters": {string(filters)}}, nil)
	if err != nil {
		return fmt.Errorf("docker events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker events: %s", resp.Status)
	}

	decoder := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var msg struct {
			Action string `json:"Action"`
			Actor  struct {
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
		}
		if err := decoder.Decode(&msg); err != nil {
			return fmt.Errorf("docker events stream: %w", err)
		}
		handle(Event{Action: msg.Action, Name: msg.Actor.Attributes["name"], Labels: msg.Actor.Attributes})
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
