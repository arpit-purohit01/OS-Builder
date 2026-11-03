// Package agent is the on-device Foundry agent: enrollment, heartbeats,
// policy, TUF-verified updates and attestation (docs/context/fleet-and-device-agent.md).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"foundry.dev/foundry/internal/sdnotify"
)

// Config is /usr/lib/foundry/agent.json (image) merged with /etc/foundry/agent.json.
type Config struct {
	Tenant      string `json:"tenant"`
	FleetURL    string `json:"fleetUrl"`
	UpdateURL   string `json:"updateUrl"`
	OSID        string `json:"osId"`
	LabelPrefix string `json:"labelPrefix"`
	Enroll      bool   `json:"enroll"`
	Group       string `json:"group"`
	AutoUpdates bool   `json:"autoUpdates"`
	StateDir    string `json:"stateDir"`
}

// LoadConfig reads the image config and the optional local override.
func LoadConfig(paths ...string) (Config, error) {
	var c Config
	found := false
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", p, err)
		}
		found = true
	}
	if !found {
		return c, fmt.Errorf("no agent config found in %v", paths)
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/foundry-agent"
	}
	return c, nil
}

// Run is the main loop.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	sdnotify.Notify("READY=1\nSTATUS=idle")
	wd := sdnotify.WatchdogInterval()
	if wd == 0 {
		wd = time.Minute
	}
	t := time.NewTicker(wd)
	defer t.Stop()
	log.Info("agent started", "tenant", cfg.Tenant, "osId", cfg.OSID, "enroll", cfg.Enroll)
	for {
		select {
		case <-ctx.Done():
			sdnotify.Notify("STOPPING=1")
			return nil
		case <-t.C:
			sdnotify.Notify("WATCHDOG=1")
		}
	}
}
