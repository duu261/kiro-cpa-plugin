package main

import "testing"

// The exact production shape: plugins.configs.kiro-ha with enabled/priority
// next to the new keys. YAML booleans and numbers decode into the string
// fields, and a later reconfigure without the keys keeps them.
func TestConfigurePluginReadsCacheMeterSettings(t *testing.T) {
	original := pluginSettings
	defer func() { pluginSettings = original }()
	pluginSettings = testPluginSettings

	configurePlugin(configureRequest("plugins:\n  configs:\n    kiro-ha:\n      enabled: true\n      priority: 1\n      cache_metering: true\n      cache_read_ratio: 0.95\n"))
	cfg := pluginSettings.cacheMeterConfig()
	if !cfg.Enabled || cfg.ReadRatio != 0.95 {
		t.Fatalf("cache meter config = %+v from %#v", cfg, pluginSettings)
	}
	if pluginSettings.MinTokenInterval != "3s" || pluginSettings.SuspendCooldown != "2h" {
		t.Fatalf("cache keys reset limiter settings: %#v", pluginSettings)
	}

	configurePlugin(configureRequest("plugins:\n  configs:\n    kiro-ha:\n      min_token_interval: 4s\n"))
	if cfg := pluginSettings.cacheMeterConfig(); !cfg.Enabled || cfg.ReadRatio != 0.95 {
		t.Fatalf("unrelated reconfigure dropped cache settings: %+v", cfg)
	}

	configurePlugin(configureRequest("plugins:\n  configs:\n    kiro-ha:\n      cache_metering: false\n"))
	if pluginSettings.cacheMeterConfig().Enabled {
		t.Fatal("cache_metering: false did not turn the meter off")
	}
}

func TestCacheMeterDefaultsOffAndRejectsBadRatio(t *testing.T) {
	if cfg := defaultPluginSettings().cacheMeterConfig(); cfg.Enabled || cfg.ReadRatio != 0.8 {
		t.Fatalf("defaults = %+v", cfg)
	}
	for _, bad := range []string{"0", "-0.2", "1.5", "abc"} {
		s := defaultPluginSettings()
		s.CacheReadRatio = bad
		if cfg := s.cacheMeterConfig(); cfg.ReadRatio != 0.8 {
			t.Fatalf("ratio %q accepted as %v", bad, cfg.ReadRatio)
		}
	}
	s := defaultPluginSettings()
	s.CacheMetering = "yes please"
	if s.cacheMeterConfig().Enabled {
		t.Fatal("unparseable cache_metering turned the meter on")
	}
}
