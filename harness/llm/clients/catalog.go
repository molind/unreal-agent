// Package clients contains shared, bounded model-catalog HTTP support.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GetCatalog never follows redirects or includes response bodies/URLs in errors.
// In particular a failed discovery request must not expose authentication data.
func GetCatalog(ctx context.Context, endpoint string, headers http.Header, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid model catalog endpoint")
	}
	req.Header = headers.Clone()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("model catalog request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model catalog returned HTTP %d", resp.StatusCode)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errors.New("cannot read model catalog")
	}
	if len(data) > limit {
		return errors.New("model catalog exceeds size limit")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid model catalog response")
	}
	return nil
}
