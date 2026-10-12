package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/barockok/brojev/internal/profile"
)

type ProfileParams struct {
	Action        string `json:"action,omitempty"` // list (default), current, add, use, remove, chrome-profiles
	Name          string `json:"name,omitempty"`
	Dir           string `json:"dir,omitempty"`
	ChromeProfile string `json:"chromeProfile,omitempty"`
	Attach        bool   `json:"attach,omitempty"`
	Quit          bool   `json:"quit,omitempty"` // on use: also close the previous profile's Chrome
}

type ProfileInfo struct {
	profile.Profile
	Active    bool `json:"active,omitempty"`
	Connected bool `json:"connected,omitempty"`
	Exists    bool `json:"exists"`
}

func (s *Server) profileTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a ProfileParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("profile: bad params: %w", err)
		}
	}
	home := Home()
	cfg, err := profile.Load(home)
	if err != nil {
		return nil, err
	}
	switch a.Action {
	case "", "list":
		return s.profileList(cfg)
	case "current":
		p, err := ActiveProfile()
		if err != nil {
			return nil, err
		}
		return s.info(p, p), nil
	case "chrome-profiles":
		dir := a.Dir
		if dir == "" && a.Name != "" {
			p, ok := cfg.Get(home, a.Name)
			if !ok {
				return nil, fmt.Errorf("no profile %q", a.Name)
			}
			dir = p.Dir
		}
		if dir == "" {
			dir = profile.ChromeDataDir()
		}
		subs, err := profile.ChromeProfiles(dir)
		if err != nil {
			return nil, fmt.Errorf("no Chrome profiles found in %s (%v)", dir, err)
		}
		return map[string]any{"dir": dir, "chromeProfiles": subs}, nil
	case "add":
		p, err := cfg.Add(home, profile.Profile{Name: a.Name, Dir: a.Dir, ChromeProfile: a.ChromeProfile, Attach: a.Attach})
		if err != nil {
			return nil, err
		}
		if err := cfg.Save(home); err != nil {
			return nil, err
		}
		return map[string]any{"added": p, "note": "switch to it with: profile use " + p.Name}, nil
	case "remove":
		if err := cfg.Remove(a.Name); err != nil {
			return nil, err
		}
		if err := cfg.Save(home); err != nil {
			return nil, err
		}
		return s.profileList(cfg)
	case "use":
		if a.Name == "" {
			return nil, errors.New("profile use: name required")
		}
		next, ok := cfg.Get(home, a.Name)
		if !ok {
			return nil, fmt.Errorf("no profile %q; add it first", a.Name)
		}
		cfg.Active = a.Name
		if err := cfg.Save(home); err != nil {
			return nil, err
		}
		s.mu.Lock()
		prev := s.prof
		if s.browser != nil && prev != next {
			if a.Quit {
				_ = s.browser.Conn().Browser().Call(ctx, "Browser.close", nil, nil)
			}
			s.dropLocked()
		}
		s.mu.Unlock()
		out := map[string]any{"active": next}
		if os.Getenv("BROJEV_PROFILE") != "" {
			out["warning"] = "BROJEV_PROFILE is set in the daemon environment and overrides the active profile"
		}
		if next.Attach {
			out["note"] = "attach mode: on first use, enable chrome://inspect/#remote-debugging in that Chrome and click Allow"
		} else {
			out["note"] = "the next browser action launches or reconnects Chrome on this profile"
		}
		return out, nil
	}
	return nil, fmt.Errorf("profile: unknown action %q (list, current, add, use, remove, chrome-profiles)", a.Action)
}

func (s *Server) profileList(cfg *profile.Config) (any, error) {
	active, err := ActiveProfile()
	if err != nil {
		active = profile.Profile{}
	}
	var out []ProfileInfo
	for _, p := range cfg.All(Home()) {
		out = append(out, s.info(p, active))
	}
	if active.Name == "env:BROJEV_PROFILE" {
		out = append(out, s.info(active, active))
	}
	return out, nil
}

func (s *Server) info(p, active profile.Profile) ProfileInfo {
	s.mu.Lock()
	connected := s.browser != nil && s.prof.Dir == p.Dir && s.prof.ChromeProfile == p.ChromeProfile
	s.mu.Unlock()
	_, err := os.Stat(p.Dir)
	return ProfileInfo{Profile: p, Active: p.Name == active.Name, Connected: connected, Exists: err == nil}
}
