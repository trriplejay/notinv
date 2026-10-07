// Command healthcheck probes the notinv service's /healthz endpoint on the
// container-local loopback, for use as the Dockerfile HEALTHCHECK. It exits 0
// when /healthz returns 200 and non-zero otherwise.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// targetURL derives the /healthz probe URL from a NOTINV_LISTEN value: it
// applies the ":8080" default when listen is empty, extracts the port via
// net.SplitHostPort (surfacing its error on a malformed value), and targets
// the loopback interface on that port rather than whatever host was
// configured to listen on.
func targetURL(listen string) (string, error) {
	if listen == "" {
		listen = ":8080"
	}

	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}

	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz", nil
}

// probe checks that the health endpoint responds with HTTP 200.
func probe(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url) //nolint:gosec // G704 false positive: targetURL always targets the 127.0.0.1 loopback (only the port derives from NOTINV_LISTEN), so this is a container-local healthcheck probe, not an SSRF vector
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func main() {
	url, err := targetURL(os.Getenv("NOTINV_LISTEN"))
	if err != nil {
		os.Exit(1)
	}
	if err := probe(url, 2*time.Second); err != nil {
		os.Exit(1)
	}
}
