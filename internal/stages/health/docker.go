package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// docker is the two Docker Engine API calls this stage needs, over the unix socket: inspect and restart.
type docker struct{ http *http.Client }

func newDocker(socket string) *docker {
	return &docker{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}}
}

type containerState struct {
	Status string `json:"Status"` // running, exited, restarting, paused, ...
	Health *struct {
		Status string `json:"Status"` // starting, healthy, unhealthy
	} `json:"Health"` // nil when the image has no HEALTHCHECK
}

// judge counts only a running container that Docker calls unhealthy as down. A stopped, paused or restarting one
// is "wrong": this stage never starts what the owner stopped, and never races Docker's own restart policy.
func (s containerState) judge() (string, fault) {
	health := "no healthcheck"
	if s.Health != nil {
		health = s.Health.Status
	}
	detail := s.Status + ", " + health
	switch {
	case s.Status != "running":
		return detail, wrong
	case health == "unhealthy":
		return detail, down
	default:
		return detail, passed
	}
}

func (d *docker) inspect(ctx context.Context, name string) (containerState, error) {
	var out struct {
		State containerState `json:"State"`
	}
	err := d.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", &out)
	return out.State, err
}

// restart stops the container (SIGKILL after 30 s) and starts it again; Docker answers once it has started.
func (d *docker) restart(ctx context.Context, name string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/restart?t=30", nil)
}

func (d *docker) call(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("docker %s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
