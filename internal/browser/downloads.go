package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cbrowser "github.com/chromedp/cdproto/browser"
	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
)

type Download struct {
	GUID     string    `json:"guid"`
	URL      string    `json:"url"`
	Name     string    `json:"name"`
	Path     string    `json:"path,omitempty"`
	State    string    `json:"state"` // inProgress, completed, canceled
	Received int64     `json:"receivedBytes"`
	Total    int64     `json:"totalBytes"`
	Started  time.Time `json:"started"`
}

type downloads struct {
	dir  string
	mu   sync.Mutex
	list []*Download
}

// EnableDownloads saves downloads into dir under their suggested names
// instead of showing Chrome's Save As UI (which CDP cannot drive).
func (b *Browser) EnableDownloads(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	yes := true
	if _, err := cdproto.Call(ctx, b.conn.Browser(), cbrowser.SetDownloadBehavior, cbrowser.SetDownloadBehaviorParams{
		Behavior: cbrowser.SetDownloadBehaviorBehaviorAllowAndName, DownloadPath: dir, EventsEnabled: &yes,
	}); err != nil {
		return err
	}
	d := &downloads{dir: dir}
	b.mu.Lock()
	b.dl = d
	b.mu.Unlock()

	begin, _ := b.conn.Browser().Subscribe("Browser.downloadWillBegin")
	progress, _ := b.conn.Browser().Subscribe("Browser.downloadProgress")
	go func() {
		for v := range begin {
			var ev cbrowser.EventDownloadWillBegin
			if jsonv2.Unmarshal(v, &ev) != nil {
				continue
			}
			d.mu.Lock()
			d.list = append(d.list, &Download{GUID: ev.GUID, URL: ev.URL, Name: ev.SuggestedFilename, State: "inProgress", Started: time.Now()})
			d.mu.Unlock()
		}
	}()
	go func() {
		for v := range progress {
			var ev cbrowser.EventDownloadProgress
			if jsonv2.Unmarshal(v, &ev) != nil {
				continue
			}
			d.update(ev)
		}
	}()
	return nil
}

func (d *downloads) update(ev cbrowser.EventDownloadProgress) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, x := range d.list {
		if x.GUID != ev.GUID {
			continue
		}
		x.Received, x.Total, x.State = int64(ev.ReceivedBytes), int64(ev.TotalBytes), string(ev.State)
		if ev.State == cbrowser.DownloadProgressStateCompleted && x.Path == "" {
			// allowAndName saves as <guid>; give it its real name.
			src := filepath.Join(d.dir, ev.GUID)
			dst := uniquePath(filepath.Join(d.dir, safeName(x.Name)))
			if err := os.Rename(src, dst); err == nil {
				x.Path = dst
			} else {
				x.Path = src
			}
		}
	}
}

func safeName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, "\\", "/"))
	if s == "" || s == "." || s == "/" {
		s = "download"
	}
	return s
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// Downloads lists downloads since the daemon connected, newest last.
func (b *Browser) Downloads() []Download {
	b.mu.Lock()
	d := b.dl
	b.mu.Unlock()
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Download, len(d.list))
	for i, x := range d.list {
		out[i] = *x
	}
	return out
}
