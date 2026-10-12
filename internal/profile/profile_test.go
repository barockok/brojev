package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAddUseRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BROJEV_PROFILE", "")
	t.Setenv("BROJEV_ATTACH", "")

	p, err := Active(home)
	if err != nil || p.Name != "default" || p.Dir != filepath.Join(home, "profile") || p.Attach {
		t.Fatalf("default active: %+v %v", p, err)
	}
	c, _ := Load(home)
	if ch, ok := c.Get(home, "chrome"); !ok || !ch.Attach || ch.Dir != ChromeDataDir() {
		t.Fatalf("chrome built-in: %+v", ch)
	}

	work, err := c.Add(home, Profile{Name: "work"})
	if err != nil || work.Dir != filepath.Join(home, "profiles", "work") {
		t.Fatalf("add: %+v %v", work, err)
	}
	c.Active = "work"
	if err := c.Save(home); err != nil {
		t.Fatal(err)
	}
	if p, _ := Active(home); p.Name != "work" || p.Dir != work.Dir {
		t.Fatalf("active after use: %+v", p)
	}
	if fi, err := os.Stat(filepath.Join(home, "config.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perms: %v %v", fi.Mode(), err)
	}

	c, _ = Load(home)
	if err := c.Remove("work"); err != nil || c.Active != "" {
		t.Fatalf("remove: %v active=%q", err, c.Active)
	}
	if err := c.Remove("default"); err == nil {
		t.Fatal("built-in must not be removable")
	}
	if _, err := c.Add(home, Profile{Name: "Bad Name"}); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := c.Add(home, Profile{Name: "default", Dir: "/elsewhere"}); err == nil {
		t.Fatal("built-in dir change accepted")
	}
}

func TestRealChromeDirForcesAttach(t *testing.T) {
	home := t.TempDir()
	c, _ := Load(home)
	p, err := c.Add(home, Profile{Name: "mine", Dir: ChromeDataDir()})
	if err != nil && !strings.Contains(err.Error(), "no Chrome profile") {
		t.Fatal(err)
	}
	if err == nil && !p.Attach {
		t.Fatal("real Chrome dir must use attach")
	}
}

func TestChromeSubProfiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Local State"), []byte(`{"profile":{"last_used":"Profile 3","info_cache":{
		"Default":{"name":"Personal","user_name":"me@example.com"},"Profile 3":{"name":"Work","user_name":"me@work.com"}}}}`), 0o600)
	subs, err := ChromeProfiles(dir)
	if err != nil || len(subs) != 2 || subs[1].Directory != "Profile 3" || !subs[1].LastUsed || subs[0].Email != "me@example.com" {
		t.Fatalf("subs: %+v %v", subs, err)
	}
	home := t.TempDir()
	c, _ := Load(home)
	if _, err := c.Add(home, Profile{Name: "w", Dir: dir, ChromeProfile: "Profile 9"}); err == nil || !strings.Contains(err.Error(), `"Profile 3" (Work)`) {
		t.Fatalf("unknown sub-profile should list known ones: %v", err)
	}
	if p, err := c.Add(home, Profile{Name: "w", Dir: dir, ChromeProfile: "Profile 3"}); err != nil || p.ChromeProfile != "Profile 3" {
		t.Fatalf("valid sub-profile: %+v %v", p, err)
	}
}

func TestEnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BROJEV_PROFILE", "/tmp/x")
	t.Setenv("BROJEV_CHROME_PROFILE", "Profile 2")
	t.Setenv("BROJEV_ATTACH", "1")
	p, err := Active(home)
	if err != nil || p.Dir != "/tmp/x" || p.ChromeProfile != "Profile 2" || !p.Attach {
		t.Fatalf("env override: %+v %v", p, err)
	}
}
