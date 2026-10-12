// Package profile manages named Chrome profiles brojev can drive, stored in
// <home>/config.json. Built-ins: "default" (dedicated <home>/profile) and
// "chrome" (the user's real Chrome data dir, attach mode).
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

type Profile struct {
	Name string `json:"name"`
	// Dir is the Chrome user data dir (--user-data-dir).
	Dir string `json:"dir"`
	// ChromeProfile selects a person inside Dir (--profile-directory),
	// e.g. "Default" or "Profile 3". Empty uses Chrome's last used one.
	ChromeProfile string `json:"chromeProfile,omitempty"`
	// Attach connects through chrome://inspect remote debugging instead of
	// a debug-port launch. Required for Chrome's default data dir.
	Attach  bool `json:"attach,omitempty"`
	Builtin bool `json:"builtin,omitempty"`
}

type Config struct {
	Active   string             `json:"active,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ChromeDataDir is the user's real Chrome data dir on this OS.
func ChromeDataDir() string {
	h, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(h, "Library", "Application Support", "Google", "Chrome")
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "User Data")
	}
	return filepath.Join(h, ".config", "google-chrome")
}

func builtins(home string) map[string]Profile {
	return map[string]Profile{
		"default": {Name: "default", Dir: filepath.Join(home, "profile"), Builtin: true},
		"chrome":  {Name: "chrome", Dir: ChromeDataDir(), Attach: true, Builtin: true},
	}
}

func path(home string) string { return filepath.Join(home, "config.json") }

func Load(home string) (*Config, error) {
	c := &Config{Profiles: map[string]Profile{}}
	b, err := os.ReadFile(path(home))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("profile: %s: %w", path(home), err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	return c, nil
}

func (c *Config) Save(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := path(home) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(home))
}

// All returns built-ins merged with user profiles (a user profile may
// override a built-in's chromeProfile/attach but not its dir), sorted.
func (c *Config) All(home string) []Profile {
	m := builtins(home)
	for k, p := range c.Profiles {
		if b, ok := m[k]; ok { // overrides of a built-in: sub-profile and attach only
			b.ChromeProfile = p.ChromeProfile
			if k != "chrome" { // the real Chrome dir always needs attach
				b.Attach = p.Attach
			}
			m[k] = b
			continue
		}
		p.Name = k
		m[k] = p
	}
	out := make([]Profile, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *Config) Get(home, name string) (Profile, bool) {
	for _, p := range c.All(home) {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Active resolves the profile to drive: $BROJEV_PROFILE (a dir, ad hoc)
// wins, then the configured active name, then "default".
func Active(home string) (Profile, error) {
	if d := os.Getenv("BROJEV_PROFILE"); d != "" {
		return Profile{Name: "env:BROJEV_PROFILE", Dir: d, ChromeProfile: os.Getenv("BROJEV_CHROME_PROFILE"), Attach: os.Getenv("BROJEV_ATTACH") == "1"}, nil
	}
	c, err := Load(home)
	if err != nil {
		return Profile{}, err
	}
	name := c.Active
	if name == "" {
		name = "default"
	}
	p, ok := c.Get(home, name)
	if !ok {
		return Profile{}, fmt.Errorf("active profile %q no longer exists; run: brojev profile use default", name)
	}
	if os.Getenv("BROJEV_ATTACH") == "1" {
		p.Attach = true
	}
	return p, nil
}

// Add creates or updates a named profile. An empty dir means a new
// dedicated data dir at <home>/profiles/<name>.
func (c *Config) Add(home string, p Profile) (Profile, error) {
	if !nameRe.MatchString(p.Name) {
		return Profile{}, fmt.Errorf("profile name %q: use lowercase letters, digits, - or _ (max 32)", p.Name)
	}
	if b, ok := builtins(home)[p.Name]; ok {
		if p.Dir != "" && p.Dir != b.Dir {
			return Profile{}, fmt.Errorf("%q is built in; its dir cannot change (add a new name instead)", p.Name)
		}
		c.Profiles[p.Name] = Profile{ChromeProfile: p.ChromeProfile, Attach: p.Attach}
		got, _ := c.Get(home, p.Name)
		return got, nil
	}
	if p.Dir == "" {
		p.Dir = filepath.Join(home, "profiles", p.Name)
	}
	abs, err := filepath.Abs(expand(p.Dir))
	if err != nil {
		return Profile{}, err
	}
	p.Dir = abs
	if filepath.Clean(abs) == filepath.Clean(ChromeDataDir()) {
		p.Attach = true // Chrome 136+ ignores --remote-debugging-port here
	}
	if p.ChromeProfile != "" {
		if subs, err := ChromeProfiles(abs); err == nil && len(subs) > 0 && !hasSub(subs, p.ChromeProfile) {
			return Profile{}, fmt.Errorf("no Chrome profile %q in %s; known: %s", p.ChromeProfile, abs, describeSubs(subs))
		}
	}
	p.Builtin = false
	stored := p
	stored.Name = ""
	c.Profiles[p.Name] = stored
	return p, nil
}

func (c *Config) Remove(name string) error {
	if name == "default" || name == "chrome" {
		if _, ok := c.Profiles[name]; ok {
			delete(c.Profiles, name) // drop overrides only
			return nil
		}
		return fmt.Errorf("%q is built in and cannot be removed", name)
	}
	if _, ok := c.Profiles[name]; !ok {
		return fmt.Errorf("no profile %q", name)
	}
	delete(c.Profiles, name)
	if c.Active == name {
		c.Active = ""
	}
	return nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// Sub is a person inside a Chrome data dir.
type Sub struct {
	Directory string `json:"directory"` // "Default", "Profile 3"
	Name      string `json:"name"`      // display name
	Email     string `json:"email,omitempty"`
	LastUsed  bool   `json:"lastUsed,omitempty"`
}

// ChromeProfiles lists the people in a Chrome data dir from its Local State.
func ChromeProfiles(dir string) ([]Sub, error) {
	b, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		return nil, err
	}
	var ls struct {
		Profile struct {
			LastUsed  string `json:"last_used"`
			InfoCache map[string]struct {
				Name     string `json:"name"`
				UserName string `json:"user_name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, err
	}
	var out []Sub
	for d, v := range ls.Profile.InfoCache {
		out = append(out, Sub{Directory: d, Name: v.Name, Email: v.UserName, LastUsed: d == ls.Profile.LastUsed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Directory < out[j].Directory })
	return out, nil
}

func hasSub(subs []Sub, d string) bool {
	for _, s := range subs {
		if s.Directory == d {
			return true
		}
	}
	return false
}

func describeSubs(subs []Sub) string {
	var parts []string
	for _, s := range subs {
		parts = append(parts, fmt.Sprintf("%q (%s)", s.Directory, s.Name))
	}
	return strings.Join(parts, ", ")
}
