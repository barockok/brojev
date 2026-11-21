package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/profile"
)

type ProfileParams struct {
	Action        string `json:"action,omitempty"` // list (default), current, add, use, remove, chrome-profiles
	Name          string `json:"name,omitempty"`
	Dir           string `json:"dir,omitempty"`
	ChromeProfile string `json:"chromeProfile,omitempty"`
	Attach        bool   `json:"attach,omitempty"`
	Quit          bool   `json:"quit,omitempty"`  // on use: also close the previous default profile's Chrome
	Force         bool   `json:"force,omitempty"` // on use with quit: close it even while other sessions work there
}

type ProfileInfo struct {
	profile.Profile
	Active    bool `json:"active,omitempty"` // the default of sessions that name no profile
	Connected bool `json:"connected,omitempty"`
	Sessions  int  `json:"sessions,omitempty"` // named sessions working on it
	Exists    bool `json:"exists"`
}

func (s *Server) profileTool(ctx context.Context, req Request) (any, error) {
	raw := req.Params
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
		if base := CDPURL(); base != "" {
			return map[string]any{"name": "external", "cdpUrl": chrome.RedactURL(base), "note": errExternalChrome.Error()}, nil
		}
		// With a name (a pinned client sends its pin): that profile.
		p, err := profile.Resolve(home, a.Name)
		if err != nil {
			return nil, err
		}
		active, _ := ActiveProfile()
		return s.info(p, active), nil
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
		return map[string]any{"added": p, "note": "use it by passing profile=" + p.Name + " on a call, or make it the default with: profile use " + p.Name}, nil
	case "remove":
		if err := cfg.Remove(a.Name); err != nil {
			return nil, err
		}
		if err := cfg.Save(home); err != nil {
			return nil, err
		}
		return s.profileList(cfg)
	case "use":
		if CDPURL() != "" {
			return nil, errExternalChrome
		}
		if a.Name == "" {
			return nil, errors.New("profile use: name required")
		}
		next, ok := cfg.Get(home, a.Name)
		if !ok {
			return nil, fmt.Errorf("no profile %q; add it first", a.Name)
		}
		// The default only decides where sessions that name no profile start:
		// every profile keeps its own Chrome, and running sessions stay put.
		var quit *link
		if prev, err := ActiveProfile(); a.Quit && err == nil && linkKey(prev) != linkKey(next) {
			s.mu.Lock()
			if ln := s.links[linkKey(prev)]; ln != nil && ln.b != nil {
				if n := s.linkSessionsLocked(ln, req.Session); n > 0 && !a.Force {
					s.mu.Unlock()
					return nil, fmt.Errorf("profile use: %d other brojev session(s) are working on profile %q and would lose their tabs; "+
						"drop quit to leave its Chrome running, or pass force=true to close it anyway", n, prev.Name)
				}
				quit = ln
			}
			s.mu.Unlock()
		}
		cfg.Active = a.Name
		if err := cfg.Save(home); err != nil {
			return nil, err
		}
		if quit != nil {
			s.mu.Lock()
			if quit.b != nil {
				_ = quit.b.Conn().Browser().Call(ctx, "Browser.close", nil, nil)
			}
			s.dropLocked(quit)
			s.mu.Unlock()
		}
		out := map[string]any{"active": next}
		if os.Getenv("BROJEV_PROFILE") != "" {
			out["warning"] = "BROJEV_PROFILE is set in the daemon environment and overrides the active profile"
		}
		if next.Attach {
			out["note"] = "attach mode: on first use, enable chrome://inspect/#remote-debugging in that Chrome and click Allow"
		} else {
			out["note"] = "sessions that name no profile now use it; sessions already running stay on their profile"
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
	ln := s.links[linkKey(p)]
	connected := ln != nil && ln.b != nil && ln.prof.ChromeProfile == p.ChromeProfile
	n := 0
	for _, sess := range s.sessions {
		if !sess.isDefault() && sess.prof.Name == p.Name {
			n++
		}
	}
	s.mu.Unlock()
	_, err := os.Stat(p.Dir)
	return ProfileInfo{Profile: p, Active: p.Name == active.Name, Connected: connected, Sessions: n, Exists: err == nil}
}

// liveBrowsers lists the profiles with a live Chrome connection.
func (s *Server) liveBrowsers() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, ln := range s.links {
		if ln.b != nil {
			out = append(out, map[string]any{"profile": ln.prof.Name, "dir": ln.prof.Dir, "sessions": s.linkSessionsLocked(ln, "")})
		}
	}
	return out
}
