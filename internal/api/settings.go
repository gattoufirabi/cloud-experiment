package api

import (
	"context"
	"sync"
	"time"
)

// Platform settings: the admin panel can override some environment settings without a
// restart. A NULL column means "use the environment value". Read through a short cache.

type platformSettings struct {
	SignupEnabled                bool
	SignupDailyLimit             int
	SignupTrialDays              int
	SignupAllowed, SignupBlocked []string
	BannerMessage, BannerLevel   string
	DeploysPaused                bool
	// which values come from the database (the rest from the environment)
	Overridden map[string]bool
}

type settingsCache struct {
	mu   sync.Mutex
	at   time.Time
	data platformSettings
}

func (s *Server) settings(ctx context.Context) platformSettings {
	s.settingsCache.mu.Lock()
	defer s.settingsCache.mu.Unlock()
	if time.Since(s.settingsCache.at) < 10*time.Second {
		return s.settingsCache.data
	}
	ps := platformSettings{
		SignupEnabled: s.cfg.SignupEnabled, SignupDailyLimit: s.cfg.SignupDailyLimit, SignupTrialDays: s.cfg.SignupTrialDays,
		SignupAllowed: s.cfg.SignupAllowedDomains, SignupBlocked: s.cfg.SignupBlockedDomains, BannerLevel: "info",
		Overridden: map[string]bool{},
	}
	var enabled *bool
	var limit, days *int
	var allowed, blocked []string
	err := s.pool.QueryRow(ctx, `
		SELECT signup_enabled, signup_daily_limit, signup_trial_days, signup_allowed_domains, signup_blocked_domains,
		       banner_message, banner_level, deploys_paused FROM platform_settings WHERE id = 1`).
		Scan(&enabled, &limit, &days, &allowed, &blocked, &ps.BannerMessage, &ps.BannerLevel, &ps.DeploysPaused)
	if err == nil {
		if enabled != nil {
			ps.SignupEnabled, ps.Overridden["signup_enabled"] = *enabled, true
		}
		if limit != nil {
			ps.SignupDailyLimit, ps.Overridden["signup_daily_limit"] = *limit, true
		}
		if days != nil {
			ps.SignupTrialDays, ps.Overridden["signup_trial_days"] = *days, true
		}
		if allowed != nil {
			ps.SignupAllowed, ps.Overridden["signup_allowed_domains"] = allowed, true
		}
		if blocked != nil {
			ps.SignupBlocked, ps.Overridden["signup_blocked_domains"] = blocked, true
		}
	} else {
		s.log.Warn("platform settings", "err", err)
	}
	s.settingsCache.data, s.settingsCache.at = ps, time.Now()
	return ps
}

func (s *Server) dropSettingsCache() {
	s.settingsCache.mu.Lock()
	s.settingsCache.at = time.Time{}
	s.settingsCache.mu.Unlock()
}
