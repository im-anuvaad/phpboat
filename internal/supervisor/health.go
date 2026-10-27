package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/im-anuvaad/phpboat/internal/app"
)

// Pinger runs a GET through the app's handler (phpserve.Site.Ping).
type Pinger interface {
	Ping(ctx context.Context, host, path string) (int, error)
}

// healthCheck waits until php-fpm has opened its socket and GET health_path
// through FastCGI answers 2xx or 3xx, or until the policy timeout.
func healthCheck(ctx context.Context, site Pinger, sock string, spec app.Spec, p Policy) error {
	ctx, cancel := context.WithTimeout(ctx, p.HealthTimeout)
	defer cancel()

	host := spec.Domain
	if host == "" {
		host = spec.Name + ".invalid"
	}
	var last error
	deadline, _ := ctx.Deadline()
	t := time.NewTicker(p.HealthInterval)
	defer t.Stop()
	for {
		err := probe(ctx, site, sock, host, spec.HealthPath)
		if err == nil {
			return nil
		}
		// A probe cut short by the deadline says nothing about the app;
		// keep the previous result (e.g. "GET /healthz returned 404").
		if last == nil || time.Now().Before(deadline) {
			last = err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("not healthy within %s: %w", p.HealthTimeout, last)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

func probe(ctx context.Context, site Pinger, sock, host, path string) error {
	if _, err := os.Stat(sock); err != nil {
		return errors.New("php-fpm has not opened its socket")
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	code, err := site.Ping(pctx, host, path)
	switch {
	case err != nil:
		return fmt.Errorf("GET %s: %w", path, err)
	case code == http.StatusServiceUnavailable:
		return errors.New("php-fpm is not accepting connections")
	case code == http.StatusBadGateway:
		return fmt.Errorf("GET %s: php-fpm failed the request (see the app log)", path)
	case code < 200 || code >= 400:
		return fmt.Errorf("GET %s returned %d", path, code)
	}
	return nil
}
