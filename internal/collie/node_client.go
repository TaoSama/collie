package collie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// nodeStatusError is a node's answer other than 200. An older node may
// return 404 for a route it does not have.
type nodeStatusError struct {
	Code int
	msg  string
}

func (e *nodeStatusError) Error() string { return e.msg }

// nodeJSON calls a node's own API through the tunnel.
func nodeJSON(ctx context.Context, client *http.Client, method, p string, in, out any) error {
	return nodeJSONWithStatusError(ctx, client, method, p, in, out, func(resp *http.Response) error {
		body := strings.TrimSpace(nodeErrorBody(resp, 512))
		return &nodeStatusError{Code: resp.StatusCode, msg: fmt.Sprintf("node %s %s: %s %s", method, p, resp.Status, body)}
	})
}

func nodeJSONWithStatusError(ctx context.Context, client *http.Client, method, p string, in, out any, statusError func(*http.Response) error) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node"+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// nodeErrorBody keeps diagnostic bytes received before a read error.
func nodeErrorBody(resp *http.Response, limit int64) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil && len(body) == 0 {
		return ""
	}
	return string(body)
}
