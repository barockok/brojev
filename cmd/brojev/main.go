// Command brojev is a fast browser agent for Claude Code.
//
//	brojev daemon [--headless]   run brojevd in the foreground
//	brojev doctor                check Chrome, daemon, connection, keys
//	brojev status | tabs         inspect the browser
//	brojev open <url>            open a tab and wait for load
//	brojev close <targetId>      close a tab
//	brojev stop                  stop brojevd (Chrome keeps running)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/barockok/brojev/internal/version"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/daemon"
	"github.com/barockok/brojev/internal/mcpserver"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return
	case "daemon":
		err = runDaemon(args)
	case "doctor":
		err = doctor()
	case "status":
		err = rpcPrint(cmd, nil)
	case "open":
		if len(args) != 1 {
			usage()
		}
		err = rpcPrint("open", map[string]string{"url": args[0]})
	case "close":
		if len(args) != 1 {
			usage()
		}
		err = rpcPrint("close", map[string]string{"targetId": args[0]})
	case "run":
		err = runGoal(args)
	case "mcp":
		daemon.LoadEnvFile()
		err = mcpserver.Run(context.Background())
	case "bench":
		err = runBench(args)
	case "profile", "profiles":
		err = runProfile(args)
	case "stop":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = daemon.Call(ctx, "shutdown", nil, nil)
	default:
		params, perr := toolParams(cmd, args)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "brojev:", perr)
			usage()
		}
		err = rpcTool(cmd, params)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brojev:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: brojev <command>
  mcp                                (MCP stdio server for Claude Code)
  daemon [--headless] | doctor | status | stop | version
  run [--url U] [--value k=v]... [--file f]... [--expect text] [--json] <goal>
  bench [--out results.json] [--only name] [--repeat n] tasks.json
  profile [list|current|chrome-profiles [name]|add <name> [--dir D] [--chrome-profile P] [--attach]|use <name> [--quit]|remove <name>]
  navigate <url> | back | forward | reload | snapshot | screenshot [full]
  click <ref> | hover <ref> | type <ref> <text> | select <ref> <value>... | key <combo> | keytype <text>
  scroll [ref] [dy] | drag <from> <to> | upload <ref> <file>... | dialog accept|dismiss [text]
  wait <text> | eval <js> | tabs | tab_new [url] | tab_switch <id> | tab_close [id] | downloads
  emulate <device>|reset | grant <permission>... | state_save [path] | state_load [path]
  any tool also takes one JSON argument: brojev click '{"x":10,"y":20}'`)
	os.Exit(2)
}

// toolParams maps positional CLI args onto tool params.
func toolParams(tool string, args []string) (map[string]any, error) {
	p := map[string]any{}
	if len(args) == 1 && strings.HasPrefix(strings.TrimSpace(args[0]), "{") {
		return p, json.Unmarshal([]byte(args[0]), &p)
	}
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %d argument(s)", tool, n)
		}
		return nil
	}
	switch tool {
	case "navigate", "tab_new":
		if len(args) > 0 {
			p["url"] = args[0]
		}
	case "click", "hover":
		if err := need(1); err != nil {
			return nil, err
		}
		p["ref"] = args[0]
	case "type":
		if err := need(2); err != nil {
			return nil, err
		}
		p["ref"], p["text"] = args[0], strings.Join(args[1:], " ")
	case "select":
		if err := need(2); err != nil {
			return nil, err
		}
		p["ref"], p["values"] = args[0], args[1:]
	case "upload":
		if err := need(2); err != nil {
			return nil, err
		}
		p["ref"], p["files"] = args[0], args[1:]
	case "drag":
		if err := need(2); err != nil {
			return nil, err
		}
		p["from"], p["to"] = args[0], args[1]
	case "key":
		if err := need(1); err != nil {
			return nil, err
		}
		p["combo"] = args[0]
	case "keytype", "wait":
		if err := need(1); err != nil {
			return nil, err
		}
		p["text"] = strings.Join(args, " ")
	case "eval":
		if err := need(1); err != nil {
			return nil, err
		}
		p["js"] = strings.Join(args, " ")
	case "scroll":
		for _, a := range args {
			if n, err := strconv.ParseFloat(a, 64); err == nil {
				p["dy"] = n
			} else {
				p["ref"] = a
			}
		}
	case "dialog":
		if len(args) > 0 {
			p["accept"] = args[0] == "accept"
			if len(args) > 1 {
				p["text"] = strings.Join(args[1:], " ")
			}
		}
	case "tab_switch", "tab_close":
		if len(args) > 0 {
			p["targetId"] = args[0]
		}
	case "screenshot":
		p["full"] = len(args) > 0 && args[0] == "full"
	case "emulate":
		if len(args) > 0 {
			if args[0] == "reset" {
				p["reset"] = true
			} else {
				p["device"] = args[0]
			}
		}
	case "grant":
		if err := need(1); err != nil {
			return nil, err
		}
		p["permissions"] = args
	case "state_save", "state_load":
		if len(args) > 0 {
			p["path"] = args[0]
		}
	case "snapshot", "back", "forward", "reload", "tabs", "downloads":
	default:
		return nil, fmt.Errorf("unknown command %q", tool)
	}
	return p, nil
}

func rpcTool(tool string, params map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := daemon.Ensure(ctx); err != nil {
		return err
	}
	var out json.RawMessage
	if err := daemon.Call(ctx, tool, params, &out); err != nil {
		return err
	}
	if tool == "snapshot" {
		var s struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(out, &s) == nil {
			fmt.Print(s.Text)
			return nil
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// runGoal: brojev run [--url U] [--value k=v]... [--file path]... [--expect text] [--json] <goal...>
func runGoal(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	url := fs.String("url", "", "start URL")
	expect := fs.String("expect", "", "text that must be visible when done")
	steps := fs.Int("max-steps", 0, "max executed actions (default 40)")
	asJSON := fs.Bool("json", false, "print the full outcome as JSON")
	answer := fs.Bool("answer", false, "extract the requested information on done (helper model)")
	var values, files multiFlag
	fs.Var(&values, "value", "known field value, key=value (repeatable)")
	fs.Var(&files, "file", "file to upload (repeatable)")
	fs.Parse(args)
	goal := strings.Join(fs.Args(), " ")
	if goal == "" {
		return fmt.Errorf("run: goal required")
	}
	vals := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("--value wants key=value, got %q", v)
		}
		vals[k] = val
	}
	params := map[string]any{"goal": goal, "url": *url, "values": vals, "files": []string(files), "expect": *expect, "maxSteps": *steps, "answer": *answer}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := daemon.Ensure(ctx); err != nil {
		return err
	}
	var out struct {
		Status   string  `json:"status"`
		Reason   string  `json:"reason"`
		Answer   string  `json:"answer"`
		Verified *bool   `json:"verified"`
		URL      string  `json:"url"`
		Title    string  `json:"title"`
		TotalMS  int64   `json:"totalMs"`
		JevMS    int64   `json:"jevMs"`
		ActMS    int64   `json:"actMs"`
		HelperMS int64   `json:"helperMs"`
		NavMS    int64   `json:"navMs"`
		JevCalls int     `json:"jevCalls"`
		CostUSD  float64 `json:"costUsd"`
		Steps    []struct {
			N          int     `json:"n"`
			Op         string  `json:"op"`
			Target     string  `json:"target"`
			Text       string  `json:"text"`
			Confidence float64 `json:"confidence"`
			Error      string  `json:"error"`
			Note       string  `json:"note"`
			AtMS       int64   `json:"atMs"`
		} `json:"steps"`
	}
	var raw json.RawMessage
	if err := daemon.Call(ctx, "run", params, &raw); err != nil {
		return err
	}
	if *asJSON {
		os.Stdout.Write(raw)
		fmt.Println()
		return nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	for _, s := range out.Steps {
		line := fmt.Sprintf("%6d ms  %-10s %s", s.AtMS, s.Op, s.Target)
		if s.Text != "" {
			line += fmt.Sprintf(" <- %q", s.Text)
		}
		line += fmt.Sprintf("  (p=%.2f)", s.Confidence)
		if s.Error != "" {
			line += "  ERROR " + s.Error
		}
		if s.Note != "" {
			line += "  [" + s.Note + "]"
		}
		fmt.Println(line)
	}
	fmt.Printf("status: %s", out.Status)
	if out.Reason != "" {
		fmt.Printf(" (%s)", out.Reason)
	}
	if out.Verified != nil {
		fmt.Printf(" verified=%v", *out.Verified)
	}
	fmt.Printf("\nfinal: %s | %s\n", out.Title, out.URL)
	if out.Answer != "" {
		fmt.Println("answer:", out.Answer)
	}
	fmt.Printf("time: %d ms total (load %d ms, jev %d ms over %d calls, actions %d ms, helper %d ms), cost $%.5f\n",
		out.TotalMS, out.NavMS, out.JevMS, out.JevCalls, out.ActMS, out.HelperMS, out.CostUSD)
	return nil
}

// runBench: brojev bench [--out results.json] [--only name] tasks.json
// Each task: {name, goal, url, values, files, expect, maxSteps}. A task
// passes when the run ends "done" and expect (if set) is visible.
func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	outPath := fs.String("out", "", "write full results JSON here")
	only := fs.String("only", "", "run only tasks whose name contains this")
	repeat := fs.Int("repeat", 1, "runs per task")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("bench: tasks file required")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var tasks []map[string]any
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	// Relative upload paths are relative to the tasks file.
	base, _ := filepath.Abs(filepath.Dir(fs.Arg(0)))
	for _, t := range tasks {
		if files, ok := t["files"].([]any); ok {
			for i, f := range files {
				if s, ok := f.(string); ok && !filepath.IsAbs(s) {
					files[i] = filepath.Join(base, s)
				}
			}
		}
	}
	ctx := context.Background()
	if err := daemon.Ensure(ctx); err != nil {
		return err
	}
	type row struct {
		Name    string          `json:"name"`
		Pass    bool            `json:"pass"`
		Outcome json.RawMessage `json:"outcome"`
	}
	var rows []row
	pass, total := 0, 0
	var totalMS, jevCalls int64
	var cost float64
	fmt.Printf("%-28s %-6s %-13s %8s %5s %9s  %s\n", "task", "pass", "status", "ms", "jev", "cost", "reason")
	for _, t := range tasks {
		name, _ := t["name"].(string)
		if *only != "" && !strings.Contains(name, *only) {
			continue
		}
		for i := 0; i < *repeat; i++ {
			rctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
			var raw json.RawMessage
			err := daemon.Call(rctx, "run", t, &raw)
			cancel()
			var o struct {
				Status   string  `json:"status"`
				Reason   string  `json:"reason"`
				Verified *bool   `json:"verified"`
				TotalMS  int64   `json:"totalMs"`
				JevCalls int64   `json:"jevCalls"`
				CostUSD  float64 `json:"costUsd"`
			}
			if err != nil {
				o.Status, o.Reason = "error", err.Error()
				raw, _ = json.Marshal(o)
			} else {
				json.Unmarshal(raw, &o)
			}
			ok := o.Status == "done" && (o.Verified == nil || *o.Verified)
			total++
			if ok {
				pass++
			}
			totalMS += o.TotalMS
			jevCalls += o.JevCalls
			cost += o.CostUSD
			reason := o.Reason
			if len(reason) > 70 {
				reason = reason[:70] + "..."
			}
			fmt.Printf("%-28s %-6v %-13s %8d %5d %9.5f  %s\n", name, ok, o.Status, o.TotalMS, o.JevCalls, o.CostUSD, reason)
			rows = append(rows, row{Name: name, Pass: ok, Outcome: raw})
		}
	}
	if total > 0 {
		fmt.Printf("passed %d/%d, mean %d ms, %d jev calls, $%.4f total\n", pass, total, totalMS/int64(total), jevCalls, cost)
	}
	if *outPath != "" {
		b, _ := json.MarshalIndent(rows, "", "  ")
		return os.WriteFile(*outPath, b, 0o644)
	}
	return nil
}

// runProfile: brojev profile [list|current|add|use|remove|chrome-profiles] [name] [flags]
func runProfile(args []string) error {
	action := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	dir := fs.String("dir", "", "Chrome user data dir (add); default <home>/profiles/<name>")
	sub := fs.String("chrome-profile", "", `person inside the data dir, e.g. "Default" or "Profile 3" (see: brojev profile chrome-profiles)`)
	attach := fs.Bool("attach", false, "connect via chrome://inspect remote debugging (no debug-port launch)")
	quit := fs.Bool("quit", false, "use: also close the previous profile's Chrome")
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := daemon.Ensure(ctx); err != nil {
		return err
	}
	params := map[string]any{"action": action, "name": name, "dir": *dir, "chromeProfile": *sub, "attach": *attach, "quit": *quit}
	var raw json.RawMessage
	if err := daemon.Call(ctx, "profile", params, &raw); err != nil {
		return err
	}
	switch action {
	case "list", "remove":
		var ps []struct {
			Name          string `json:"name"`
			Dir           string `json:"dir"`
			ChromeProfile string `json:"chromeProfile"`
			Attach        bool   `json:"attach"`
			Builtin       bool   `json:"builtin"`
			Active        bool   `json:"active"`
			Connected     bool   `json:"connected"`
			Exists        bool   `json:"exists"`
		}
		if json.Unmarshal(raw, &ps) == nil {
			for _, p := range ps {
				mark := " "
				if p.Active {
					mark = "*"
				}
				var tags []string
				if p.Builtin {
					tags = append(tags, "built-in")
				}
				if p.Attach {
					tags = append(tags, "attach")
				}
				if p.Connected {
					tags = append(tags, "connected")
				}
				if !p.Exists {
					tags = append(tags, "new")
				}
				line := fmt.Sprintf("%s %-14s %s", mark, p.Name, p.Dir)
				if p.ChromeProfile != "" {
					line += fmt.Sprintf(" [%s]", p.ChromeProfile)
				}
				if len(tags) > 0 {
					line += "  (" + strings.Join(tags, ", ") + ")"
				}
				fmt.Println(line)
			}
			return nil
		}
	case "chrome-profiles":
		var r struct {
			Dir  string `json:"dir"`
			Subs []struct {
				Directory string `json:"directory"`
				Name      string `json:"name"`
				Email     string `json:"email"`
				LastUsed  bool   `json:"lastUsed"`
			} `json:"chromeProfiles"`
		}
		if json.Unmarshal(raw, &r) == nil {
			fmt.Println(r.Dir)
			for _, x := range r.Subs {
				last := ""
				if x.LastUsed {
					last = "  (last used)"
				}
				fmt.Printf("  %-12s %s %s%s\n", x.Directory, x.Name, x.Email, last)
			}
			return nil
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(raw)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runDaemon(args []string) error {
	daemon.LoadEnvFile() // before flags: BROJEV_HEADLESS may come from the env file
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	headless := fs.Bool("headless", os.Getenv("BROJEV_HEADLESS") == "1", "run Chrome headless")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return (&daemon.Server{Headless: *headless}).Serve(ctx)
}

func rpcPrint(method string, params any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := daemon.Ensure(ctx); err != nil {
		return err
	}
	var out json.RawMessage
	if err := daemon.Call(ctx, method, params, &out); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func doctor() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	failed := false
	check := func(name string, err error, detail string) bool {
		if err != nil {
			failed = true
			fmt.Printf("  [FAIL] %-18s %v\n", name, err)
			return false
		}
		fmt.Printf("  [ok  ] %-18s %s\n", name, detail)
		return true
	}
	fmt.Println("brojev doctor")
	fmt.Printf("  home               %s\n", daemon.Home())

	bin, err := chrome.Find()
	check("chrome binary", err, bin)

	err = daemon.Ensure(ctx)
	var ping struct {
		PID int `json:"pid"`
	}
	if err == nil {
		err = daemon.Call(ctx, "ping", nil, &ping)
	}
	if !check("daemon", err, fmt.Sprintf("pid %d, %s", ping.PID, daemon.SocketPath())) {
		return fmt.Errorf("daemon not reachable; see %s", daemon.LogPath())
	}

	var st struct {
		Product       string `json:"product"`
		Endpoint      string `json:"endpoint"`
		Tabs          int    `json:"tabs"`
		Profile       string `json:"profile"`
		ProfileDir    string `json:"profileDir"`
		ChromeProfile string `json:"chromeProfile"`
	}
	start := time.Now()
	err = daemon.Call(ctx, "status", nil, &st)
	check("browser", err, fmt.Sprintf("%s, %d tabs, %s (%d ms)", st.Product, st.Tabs, st.Endpoint, time.Since(start).Milliseconds()))
	if err == nil {
		sub := ""
		if st.ChromeProfile != "" {
			sub = " [" + st.ChromeProfile + "]"
		}
		fmt.Printf("  [ok  ] %-18s %s %s%s\n", "profile", st.Profile, st.ProfileDir, sub)
	}

	if err == nil {
		page := "data:text/html," + url.PathEscape("<title>brojev ok</title><p>hello")
		var info browser.PageInfo
		start = time.Now()
		err = daemon.Call(ctx, "open", map[string]string{"url": page}, &info)
		if err == nil && info.Title != "brojev ok" {
			err = fmt.Errorf("unexpected title %q", info.Title)
		}
		if check("open+read round trip", err, fmt.Sprintf("%d ms total, load %d ms", time.Since(start).Milliseconds(), info.LoadMS)) {
			_ = daemon.Call(ctx, "close", map[string]string{"targetId": info.TargetID}, nil)
		}
	}

	key := "TYPESAFE_API_KEY"
	if os.Getenv(key) == "" && os.Getenv("JEV_API_KEY") != "" {
		key = "JEV_API_KEY"
	}
	if os.Getenv(key) == "" {
		fmt.Println("  [warn] jev api key       TYPESAFE_API_KEY / JEV_API_KEY not set (needed from M2)")
	} else {
		fmt.Printf("  [ok  ] %-18s %s set\n", "jev api key", key)
	}
	if failed {
		return fmt.Errorf("some checks failed; logs in %s", daemon.Home())
	}
	return nil
}
