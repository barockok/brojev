package page

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// Emulation changes how this tab presents itself. Zero fields are left as is;
// Reset clears every override first.
type Emulation struct {
	Reset       bool     `json:"reset,omitempty"`
	Width       int      `json:"width,omitempty"`
	Height      int      `json:"height,omitempty"`
	Scale       float64  `json:"scale,omitempty"` // device pixel ratio
	Mobile      bool     `json:"mobile,omitempty"`
	Touch       bool     `json:"touch,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
	Timezone    string   `json:"timezone,omitempty"` // e.g. Europe/Zurich
	Locale      string   `json:"locale,omitempty"`   // e.g. de-CH
	Offline     *bool    `json:"offline,omitempty"`
	ColorScheme string   `json:"colorScheme,omitempty"` // light | dark
}

// Devices are viewport presets (CSS px, DPR, mobile, touch).
var Devices = map[string]Emulation{
	"iphone-15":  {Width: 393, Height: 852, Scale: 3, Mobile: true, Touch: true},
	"pixel-8":    {Width: 412, Height: 915, Scale: 2.625, Mobile: true, Touch: true},
	"ipad":       {Width: 820, Height: 1180, Scale: 2, Mobile: true, Touch: true},
	"desktop":    {Width: 1440, Height: 900, Scale: 2},
	"desktop-hd": {Width: 1920, Height: 1080, Scale: 1},
}

func (p *Page) call(ctx context.Context, method string, params map[string]any) error {
	return p.S.Call(ctx, method, params, nil)
}

func (p *Page) Emulate(ctx context.Context, e Emulation) error {
	if e.Reset {
		for _, m := range []string{"Emulation.clearDeviceMetricsOverride", "Emulation.clearGeolocationOverride"} {
			if err := p.call(ctx, m, nil); err != nil {
				return err
			}
		}
		_ = p.call(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": false})
		_ = p.call(ctx, "Emulation.setTimezoneOverride", map[string]any{"timezoneId": ""})
		_ = p.call(ctx, "Emulation.setLocaleOverride", map[string]any{})
		_ = p.call(ctx, "Emulation.setEmulatedMedia", map[string]any{"features": []any{}})
		_ = p.call(ctx, "Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	}
	if e.Width > 0 || e.Height > 0 {
		if e.Width <= 0 || e.Height <= 0 {
			return errors.New("emulate: width and height go together")
		}
		scale := e.Scale
		if scale == 0 {
			scale = 1
		}
		if err := p.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
			"width": e.Width, "height": e.Height, "deviceScaleFactor": scale, "mobile": e.Mobile,
		}); err != nil {
			return err
		}
		if err := p.call(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": e.Touch, "maxTouchPoints": 5}); err != nil {
			return err
		}
	}
	if (e.Latitude == nil) != (e.Longitude == nil) {
		return errors.New("emulate: latitude and longitude go together")
	}
	if e.Latitude != nil {
		if err := p.call(ctx, "Emulation.setGeolocationOverride", map[string]any{"latitude": *e.Latitude, "longitude": *e.Longitude, "accuracy": 50}); err != nil {
			return err
		}
		if origin := p.origin(ctx); origin != "" {
			_ = p.S.Conn().Browser().Call(ctx, "Browser.grantPermissions", map[string]any{"permissions": []string{"geolocation"}, "origin": origin}, nil)
		}
	}
	if e.Timezone != "" {
		if err := p.call(ctx, "Emulation.setTimezoneOverride", map[string]any{"timezoneId": e.Timezone}); err != nil {
			return err
		}
	}
	if e.Locale != "" {
		if err := p.call(ctx, "Emulation.setLocaleOverride", map[string]any{"locale": e.Locale}); err != nil {
			return err
		}
	}
	if e.ColorScheme != "" {
		if err := p.call(ctx, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": e.ColorScheme}}}); err != nil {
			return err
		}
	}
	if e.Offline != nil {
		if err := p.call(ctx, "Network.enable", nil); err != nil {
			return err
		}
		if err := p.call(ctx, "Network.emulateNetworkConditions", map[string]any{"offline": *e.Offline, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1}); err != nil {
			return err
		}
	}
	return nil
}

// Grant pre-grants browser permissions (geolocation, notifications,
// clipboardReadWrite, camera, microphone...) for the current origin, since
// permission bubbles are browser UI that CDP cannot click.
func (p *Page) Grant(ctx context.Context, perms []string) (string, error) {
	origin := p.origin(ctx)
	if origin == "" {
		return "", errors.New("grant: current page has no origin")
	}
	return origin, p.S.Conn().Browser().Call(ctx, "Browser.grantPermissions", map[string]any{"permissions": perms, "origin": origin}, nil)
}

func (p *Page) origin(ctx context.Context) string {
	info, err := p.info(ctx)
	if err != nil {
		return ""
	}
	u, err := url.Parse(info.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// State is a portable login state: all cookies plus localStorage of the
// current origin.
type State struct {
	Cookies      []map[string]any             `json:"cookies"`
	LocalStorage map[string]map[string]string `json:"localStorage,omitempty"`
}

var cookieFields = []string{"name", "value", "domain", "path", "secure", "httpOnly", "sameSite", "expires", "priority", "sourceScheme", "sourcePort", "partitionKey"}

func (p *Page) SaveState(ctx context.Context, path string) (int, error) {
	var res struct {
		Cookies []map[string]any `json:"cookies"`
	}
	if err := p.S.Conn().Browser().Call(ctx, "Storage.getCookies", map[string]any{}, &res); err != nil {
		return 0, err
	}
	st := State{Cookies: res.Cookies, LocalStorage: map[string]map[string]string{}}
	if origin := p.origin(ctx); origin != "" {
		if ls, err := p.LocalStorage(ctx); err == nil {
			st.LocalStorage[origin] = ls
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(st.Cookies), os.WriteFile(path, b, 0o600)
}

// LoadState restores cookies, and localStorage for the current origin if
// the file has it.
func (p *Page) LoadState(ctx context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return "", err
	}
	cookies := CleanCookies(st.Cookies)
	if len(cookies) > 0 {
		if err := p.S.Conn().Browser().Call(ctx, "Storage.setCookies", map[string]any{"cookies": cookies}, nil); err != nil {
			return "", err
		}
	}
	note := fmt.Sprintf("restored %d cookies", len(cookies))
	if origin := p.origin(ctx); origin != "" {
		if ls, ok := st.LocalStorage[origin]; ok {
			if err := p.SetLocalStorage(ctx, ls); err != nil {
				return note, err
			}
			note += fmt.Sprintf(" and %d localStorage keys for %s", len(ls), origin)
		}
	}
	return note, nil
}

// CleanCookies keeps the fields Storage.setCookies accepts from cookies read
// with Storage.getCookies, and drops expires from session cookies.
func CleanCookies(in []map[string]any) []map[string]any {
	var out []map[string]any
	for _, c := range in {
		clean := map[string]any{}
		for _, f := range cookieFields {
			if v, ok := c[f]; ok {
				clean[f] = v
			}
		}
		if s, _ := c["session"].(bool); s {
			delete(clean, "expires")
		}
		out = append(out, clean)
	}
	return out
}

// Origin is the current page's scheme://host ("" for about:blank, data: ...).
func (p *Page) Origin(ctx context.Context) string { return p.origin(ctx) }

// CurrentURL is the current page's location.href ("" if it cannot be read).
func (p *Page) CurrentURL(ctx context.Context) string {
	info, err := p.info(ctx)
	if err != nil {
		return ""
	}
	return info.URL
}

// LocalStorage reads localStorage of the current origin.
func (p *Page) LocalStorage(ctx context.Context) (map[string]string, error) {
	v, err := p.Eval(ctx, "Object.fromEntries(Object.entries(localStorage))")
	if err != nil {
		return nil, err
	}
	var ls map[string]string
	return ls, json.Unmarshal(v, &ls)
}

// SetLocalStorage writes keys into localStorage of the current origin.
func (p *Page) SetLocalStorage(ctx context.Context, ls map[string]string) error {
	b, _ := json.Marshal(ls)
	_, err := p.Eval(ctx, fmt.Sprintf("(() => { const d = %s; for (const k in d) localStorage.setItem(k, d[k]); return Object.keys(d).length })()", b))
	return err
}
