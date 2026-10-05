package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// DefaultAssetRule returns a permissive-but-safe rule set used by the local
// demo. Callers should override thresholds to match their own risk model.
func DefaultAssetRule() AssetRule {
	return AssetRule{
		MinSources:              3,
		TrimLow:                 1,
		TrimHigh:                1,
		MaxDeviationBasisPoints: 200, // 2%
		HardFloorBps:            500, // last price * 0.95
		HardCeilingBps:          500, // last price * 1.05
		MinPositivePrice:        1,
	}
}

func DefaultConfig(version int64, sources map[string]SourceConfig, assets map[string]AssetRule) Config {
	return Config{
		Version:            version,
		Sources:            sources,
		Assets:             assets,
		SubmissionTTL:      30 * time.Second,
		ClockSkew:          2 * time.Second,
		MaxQuoteStaleness:  2 * time.Minute,
		FallbackStaleness:  10 * time.Minute,
		OnRestartOpenRound: "resume",
	}
}

func (c Config) Validate() error {
	if c.Version <= 0 {
		return fmt.Errorf("config version must be positive, got %d", c.Version)
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("config v%d has no sources", c.Version)
	}
	if len(c.Assets) == 0 {
		return fmt.Errorf("config v%d has no assets", c.Version)
	}
	ids := map[string]bool{}
	for id, s := range c.Sources {
		if id != s.ID {
			return fmt.Errorf("config v%d source key %q != id %q", c.Version, id, s.ID)
		}
		if s.Key == "" {
			return fmt.Errorf("config v%d source %q has empty key", c.Version, id)
		}
		if ids[id] {
			return fmt.Errorf("config v%d duplicate source %q", c.Version, id)
		}
		ids[id] = true
	}
	for asset, r := range c.Assets {
		if r.MinSources <= 0 {
			return fmt.Errorf("config v%d asset %q min_sources must be positive", c.Version, asset)
		}
		if r.MaxDeviationBasisPoints <= 0 {
			return fmt.Errorf("config v%d asset %q max_deviation_bp must be positive", c.Version, asset)
		}
		if r.HardFloorBps < 0 || r.HardCeilingBps < 0 {
			return fmt.Errorf("config v%d asset %q hard band bps must be non-negative", c.Version, asset)
		}
	}
	if c.SubmissionTTL <= 0 || c.MaxQuoteStaleness <= 0 || c.FallbackStaleness <= 0 {
		return fmt.Errorf("config v%d TTL/staleness durations must be positive", c.Version)
	}
	if c.OnRestartOpenRound != "resume" && c.OnRestartOpenRound != "void" {
		return fmt.Errorf("config v%d on_restart_open_round must be resume or void", c.Version)
	}
	return nil
}

func (c Config) Source(id string) (SourceConfig, bool) {
	s, ok := c.Sources[id]
	return s, ok && s.Active
}

func LoadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}
