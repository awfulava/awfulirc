package awfulirc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
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

func ParseNetscapeCookieFile(r io.Reader, jar http.CookieJar) error {
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			continue
		}
		for i, f := range fields {
			fields[i] = strings.TrimSpace(f)
		}

		secure, err := strconv.ParseBool(fields[3])
		if err != nil {
			return err
		}

		expires, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return err
		}

		cookie := &http.Cookie{
			Domain:  fields[0],
			Path:    fields[2],
			Name:    fields[5],
			Secure:  secure,
			Expires: time.Unix(expires, 0),
			Value:   fields[6],
		}

		scheme := "http"
		if cookie.Secure {
			scheme = "https"
		}

		u := &url.URL{
			Scheme: scheme,
			Host:   cookie.Domain,
		}

		cookies := jar.Cookies(u)
		cookies = append(cookies, cookie)
		jar.SetCookies(u, cookies)

	}
	return s.Err()
}
