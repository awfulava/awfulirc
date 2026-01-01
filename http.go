package awfulirc

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"time"

	"golang.org/x/time/rate"
)

// LimitedHTTPClient performs throttled requests to somethingawful.com. The
// zero value is not valid for use.
type LimitedHTTPClient struct {
	client  *http.Client
	limiter *rate.Limiter
}

// NewLimitedHTTPClient returns a rate limited client with a cookie jar configured.
func NewLimitedHTTPClient() (*LimitedHTTPClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create cookie jar: %w", err)
	}
	return &LimitedHTTPClient{
		client: &http.Client{
			Jar: jar,
		},
		limiter: rate.NewLimiter(rate.Every(time.Second), 5),
	}, nil
}

// Do waits until the client is within rate limits and then performs the request.
func (c *LimitedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	r := c.limiter.Reserve()
	if !r.OK() {
		return nil, errors.New("invalid limiter configuration")
	}
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-time.After(r.Delay()):
		return c.client.Do(req)
	}
}
