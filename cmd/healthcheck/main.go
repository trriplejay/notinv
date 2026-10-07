package main

import (
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

func main() {
	url, err := targetURL(os.Getenv("NOTINV_LISTEN"))
	if err != nil {
		os.Exit(1)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
