package main

import (
	"strconv"
	"strings"
	"time"

	kiroauth "github.com/nguyenha935/kiro-cpa-plugin/internal/auth/kiro"
	"github.com/nguyenha935/kiro-cpa-plugin/internal/cachemeter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pluginSettingsData struct {
	MinTokenInterval string `yaml:"min_token_interval"`
	MaxTokenInterval string `yaml:"max_token_interval"`
	SuspendCooldown  string `yaml:"suspend_cooldown"`
	// CacheMetering and CacheReadRatio are strings so that an absent key stays
	// distinguishable from false/0 under the merge rule below.
	CacheMetering  string `yaml:"cache_metering"`
	CacheReadRatio string `yaml:"cache_read_ratio"`
}

func (s pluginSettingsData) isZero() bool {
	return s.MinTokenInterval == "" && s.MaxTokenInterval == "" && s.SuspendCooldown == "" &&
		s.CacheMetering == "" && s.CacheReadRatio == ""
}

func defaultPluginSettings() pluginSettingsData {
	return pluginSettingsData{
		MinTokenInterval: kiroauth.DefaultMinTokenInterval.String(),
		MaxTokenInterval: kiroauth.DefaultMaxTokenInterval.String(),
		SuspendCooldown:  kiroauth.DefaultSuspendCooldown.String(),
		CacheMetering:    "false",
		CacheReadRatio:   strconv.FormatFloat(cachemeter.DefaultReadRatio, 'f', -1, 64),
	}
}

func (s pluginSettingsData) normalized() pluginSettingsData {
	defaults := defaultPluginSettings()
	minInterval, minErr := time.ParseDuration(s.MinTokenInterval)
	if minErr != nil || minInterval <= 0 {
		s.MinTokenInterval = defaults.MinTokenInterval
		minInterval = kiroauth.DefaultMinTokenInterval
	}
	maxInterval, maxErr := time.ParseDuration(s.MaxTokenInterval)
	if maxErr != nil || maxInterval < minInterval {
		s.MaxTokenInterval = s.MinTokenInterval
	}
	suspend, suspendErr := time.ParseDuration(s.SuspendCooldown)
	if suspendErr != nil || suspend <= 0 {
		s.SuspendCooldown = defaults.SuspendCooldown
	}
	if enabled, err := strconv.ParseBool(strings.TrimSpace(s.CacheMetering)); err != nil {
		s.CacheMetering = defaults.CacheMetering
	} else {
		s.CacheMetering = strconv.FormatBool(enabled)
	}
	if ratio, err := strconv.ParseFloat(strings.TrimSpace(s.CacheReadRatio), 64); err != nil || ratio <= 0 || ratio > 1 {
		s.CacheReadRatio = defaults.CacheReadRatio
	} else {
		s.CacheReadRatio = strconv.FormatFloat(ratio, 'f', -1, 64)
	}
	return s
}

func (s pluginSettingsData) rateLimiterConfig() kiroauth.RateLimiterConfig {
	s = s.normalized()
	minInterval, _ := time.ParseDuration(s.MinTokenInterval)
	maxInterval, _ := time.ParseDuration(s.MaxTokenInterval)
	suspendCooldown, _ := time.ParseDuration(s.SuspendCooldown)
	return kiroauth.RateLimiterConfig{
		MinTokenInterval: minInterval,
		MaxTokenInterval: maxInterval,
		SuspendCooldown:  suspendCooldown,
	}
}

func (s pluginSettingsData) cacheMeterConfig() cachemeter.Config {
	s = s.normalized()
	enabled, _ := strconv.ParseBool(s.CacheMetering)
	ratio, _ := strconv.ParseFloat(s.CacheReadRatio, 64)
	return cachemeter.Config{Enabled: enabled, ReadRatio: ratio}
}

func pluginConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "min_token_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Minimum delay between requests for one credential. Default: 1s."},
		{Name: "max_token_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum jittered delay between requests for one credential. Default: 2s."},
		{Name: "suspend_cooldown", Type: pluginapi.ConfigFieldTypeString, Description: "How long a credential stays out of rotation after Kiro reports it suspended. Default: 1h."},
		{Name: "cache_metering", Type: pluginapi.ConfigFieldTypeString, Description: "Report Anthropic-style simulated prompt-cache usage for Claude models (true/false). Kiro is called the same way either way. Default: false."},
		{Name: "cache_read_ratio", Type: pluginapi.ConfigFieldTypeString, Description: "Highest share of the prompt reported as cache read when cache_metering is on, 0 to 1. Default: 0.8."},
	}
}
