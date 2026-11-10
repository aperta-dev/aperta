// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/aperta-dev/aperta/internal/config"
	"github.com/aperta-dev/aperta/internal/provider/anthropic"
	"github.com/aperta-dev/aperta/internal/provider/transport"
)

const (
	freeModel   = "nvidia/nemotron-3.5-lightning:free"
	cheapOpenAI = "openai/gpt-5-nano"
	haiku       = "claude-haiku-5-5"
	stepWait    = 60 * time.Second
)

var (
	binary  string
	secrets []string
	orKey   string
	anKey   string
	only    = flag.String("only", "", "run only flows whose name contains this")
	total   = flag.Duration("timeout", 5*time.Minute, "fail and stop if the whole run takes longer")
	flowMax = flag.Duration("flow-timeout", 3*time.Minute, "fail and stop the run if one flow takes longer")

	procMu sync.Mutex
	procs  []*exec.Cmd
)

func track(c *exec.Cmd) {
	procMu.Lock()
	procs = append(procs, c)
	procMu.Unlock()
}

func killAll() {
	procMu.Lock()
	defer procMu.Unlock()
	for _, c := range procs {
		if c.Process != nil && c.ProcessState == nil {
			killGroup(c.Process.Pid)
			c.Process.Kill()
		}
	}
}

func main() {
	flag.Parse()
	time.AfterFunc(*total, func() {
		killAll()
		fmt.Printf("FAIL the whole run took longer than %s; stopped\n", *total)
		os.Exit(1)
	})
	os.Exit(run())
}

func run() int {
	work, err := os.MkdirTemp("", "aperta-smoke-")
	if err != nil {
		fmt.Println("FAIL setup:", err)
		return 1
	}
	defer os.RemoveAll(work)
	binary = filepath.Join(work, "aperta")
	if out, err := exec.Command("go", "build", "-o", binary, "./cmd/aperta").CombinedOutput(); err != nil {
		fmt.Printf("FAIL build: %s\n", out)
		return 1
	}
	loadKeys()
	preflightFailed := preflight()
	flows := []flow{
		{"first run, /connect with a pasted key", needOR, flowConnect},
		{"/model filter and pick a free model", needOR, nil},
		{"real reply", needOR, nil},
		{"file edit with a permission prompt", needOR, nil},
		{"ctrl+c mid-stream, then quit", needOR, nil},
		{"aperta -c resumes the history", needOR, flowResume},
		{"headless run --json", needOR, flowHeadless},
		{"openai-responses via openrouter (free model)", needOR, flowResponsesFree},
		{"openai-responses via openrouter (" + cheapOpenAI + ", tools, reasoning)", needOR, flowResponsesTools},
		{"anthropic " + haiku + " headless", needAN, flowAnthropic},
		{"narrow 60-column terminal", needOR, flowNarrow},
		{"bad key is refused calmly", needNothing, flowBadKey},
		{"no network ends in a plain message", needNothing, flowNoNetwork},
		{"no key in sessions, logs or caches", needAny, flowNoLeaks},
	}
	env := &shared{work: work, home: filepath.Join(work, "home"), dir: filepath.Join(work, "project")}
	os.MkdirAll(env.dir, 0o755)
	results := map[string]int{}
	start := time.Now()
	var chained []string
	for i, f := range flows {
		if *only != "" && !strings.Contains(f.name, *only) {
			continue
		}
		if why := f.need(); why != "" {
			report("SKIP", f.name, why, 0)
			results["SKIP"]++
			continue
		}
		if f.fn == nil {
			continue
		}
		t0 := time.Now()
		steps := []string{f.name}
		for j := i + 1; j < len(flows) && flows[j].fn == nil; j++ {
			steps = append(steps, flows[j].name)
		}
		env.passed = nil
		err := runFlow(f.fn, env)
		stop := errors.Is(err, errFlowTimeout)
		took := time.Since(t0)
		for k, name := range steps {
			d := took
			if k > 0 {
				d = 0
			}
			switch {
			case contains(env.passed, name) || len(steps) == 1 && err == nil:
				report("PASS", name, "", d)
				results["PASS"]++
			case err != nil && !reported(chained, name):
				report("FAIL", name, scrub(err.Error()), d)
				results["FAIL"]++
				chained = append(chained, name)
				err = errors.New("an earlier step failed")
			}
		}
		if stop {
			for _, rest := range flows[i+1:] {
				if !contains(steps, rest.name) {
					report("SKIP", rest.name, "an earlier flow timed out", 0)
					results["SKIP"]++
				}
			}
			break
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped in %s\n", results["PASS"], results["FAIL"], results["SKIP"], time.Since(start).Round(time.Second))
	if preflightFailed {
		fmt.Println("not green: a key failed the preflight check, so its flows were skipped")
		return 1
	}
	if results["FAIL"] > 0 {
		return 1
	}
	return 0
}

type flow struct {
	name string
	need func() string
	fn   func(*shared) error
}

type shared struct {
	work, home, dir string
	passed          []string
}

func (s *shared) pass(name string) { s.passed = append(s.passed, name) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func reported(list []string, s string) bool { return contains(list, s) }

func runFlow(fn func(*shared) error, s *shared) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("smoke crashed: %v", r)
			}
		}()
		done <- fn(s)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(*flowMax):
		killAll()
		return errFlowTimeout
	}
}

var errFlowTimeout = errors.New("the flow took too long; its processes were stopped and the run ends here, because the flow may still be running")

func report(status, name, why string, d time.Duration) {
	line := fmt.Sprintf("%-4s %s", status, name)
	if d > 0 {
		line += fmt.Sprintf(" (%.1fs)", d.Seconds())
	}
	if why != "" {
		line += "\n     " + strings.ReplaceAll(why, "\n", "\n     ")
	}
	fmt.Println(line)
}

var keyProblem = map[string]string{}

func needNothing() string { return "" }

func needOR() string {
	if orKey == "" {
		return "no OpenRouter key: set OPENROUTER_API_KEY or connect openrouter in ~/.aperta"
	}
	return keyProblem["openrouter"]
}

func preflight() bool {
	failed := false
	for _, c := range []struct {
		name, key, url string
		headers        map[string]string
	}{
		{"openrouter", orKey, "https://openrouter.ai/api/v1/key", map[string]string{"Authorization": "Bearer " + orKey}},
		{"anthropic", anKey, "https://api.anthropic.com/v1/models?limit=1", map[string]string{"x-api-key": anKey, "anthropic-version": anthropic.APIVersion}},
	} {
		if c.key == "" {
			continue
		}
		why := checkKey(c.url, c.headers)
		if why == "" {
			fmt.Printf("key  %s ok (%s)\n", c.name, config.Mask(c.key))
			continue
		}
		failed = true
		keyProblem[c.name] = fmt.Sprintf("%s key %s. Fix: aperta connect %s", c.name, why, c.name)
		fmt.Printf("key  %s\n", scrub(keyProblem[c.name]))
	}
	fmt.Println()
	return failed
}

func checkKey(u string, headers map[string]string) string {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "could not be checked: " + err.Error()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "could not be checked: " + req.URL.Host + " did not answer"
	}
	defer res.Body.Close()
	if res.StatusCode < 400 {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_, msg := transport.ParseError(raw)
	if len(msg) > 120 {
		msg = msg[:120] + "..."
	}
	msg = strings.TrimRight(strings.TrimSpace(msg), ".")
	if msg == "" {
		msg = http.StatusText(res.StatusCode)
	}
	return fmt.Sprintf("rejected (%d: %s)", res.StatusCode, msg)
}

func needAny() string {
	if len(secrets) == 0 {
		return "no keys, so nothing could leak"
	}
	return ""
}

func needAN() string {
	if anKey == "" {
		return "no Anthropic key: set ANTHROPIC_API_KEY or connect anthropic in ~/.aperta"
	}
	return keyProblem["anthropic"]
}

func loadKeys() {
	home := os.Getenv("APERTA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".aperta")
		}
	}
	creds, _ := config.LoadCredentials(filepath.Join(home, "credentials.json"))
	pick := func(env, name string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		c, _ := creds.Get(name)
		return c.APIKey
	}
	orKey, anKey = pick("OPENROUTER_API_KEY", "openrouter"), pick("ANTHROPIC_API_KEY", "anthropic")
	for _, k := range []string{orKey, anKey} {
		if k != "" {
			secrets = append(secrets, k)
		}
	}
}

func scrub(s string) string {
	for _, k := range secrets {
		s = strings.ReplaceAll(s, k, config.Mask(k))
	}
	return s
}

func baseEnv(home string, extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "APERTA_") || strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_BASE_URL") || k == "TERM" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, append([]string{"APERTA_HOME=" + home, "TERM=xterm-256color"}, extra...)...)
}

func seedHome(home string) error {
	creds, err := config.LoadCredentials(filepath.Join(home, "credentials.json"))
	if err != nil {
		return err
	}
	if orKey != "" {
		if err := creds.Set("openrouter", config.Credential{APIKey: orKey}); err != nil {
			return err
		}
	}
	return nil
}

type term struct {
	home string
	mu   sync.Mutex
	e    *vt.Emulator
	f    *os.File
	cmd  *exec.Cmd
	done chan error
	raw  strings.Builder
}

func startTUI(dir, home string, cols, rows int, env []string, args ...string) (*term, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = baseEnv(home, env...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	track(cmd)
	t := &term{home: home, e: vt.NewEmulator(cols, rows), f: f, cmd: cmd, done: make(chan error, 1)}
	t.e.Scrollback().SetMaxLines(10000)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				t.mu.Lock()
				t.e.Write(buf[:n])
				t.raw.Write(buf[:n])
				t.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go io.Copy(f, t.e)
	go func() { t.done <- cmd.Wait() }()
	return t, nil
}

func (t *term) screen() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.e.String()
}

func (t *term) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, l := range t.e.Scrollback().Lines() {
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")
	}
	b.WriteString(t.e.String())
	return b.String()
}

func (t *term) write(s string) { t.f.Write([]byte(s)) }

func (t *term) paste(s string) { t.write("\x1b[200~" + s + "\x1b[201~") }

func (t *term) line(s string) {
	t.paste(s)
	time.Sleep(150 * time.Millisecond)
	t.write("\r")
}

func (t *term) waitFor(what string, ok func() bool, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return nil
		}
		select {
		case err := <-t.done:
			t.done <- err
			return fmt.Errorf("aperta exited (%v) while waiting for %s\n%s", err, what, tail(t.screen(), 12))
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s\nscreen:\n%s", d, what, tail(t.screen(), 14))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (t *term) waitText(want string, d time.Duration) error {
	return t.waitFor(fmt.Sprintf("%q", want), func() bool { return strings.Contains(t.text(), want) }, d)
}

func (t *term) idle() bool {
	s := t.screen()
	return strings.Contains(s, "Ask Aperta") && !strings.Contains(s, "esc to stop")
}

func (t *term) quit() error {
	t.write("\x03")
	time.Sleep(200 * time.Millisecond)
	if t.idle() {
		t.write("\x03")
	}
	select {
	case err := <-t.done:
		t.done <- err
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 130 {
			err = nil
		}
		if err != nil {
			return fmt.Errorf("aperta exited with %v after ctrl+c\n%s", err, tail(t.text(), 6))
		}
		return calm(tail(t.text(), 6))
	case <-time.After(5 * time.Second):
		t.cmd.Process.Kill()
		return errors.New("aperta did not quit within 5s of ctrl+c")
	}
}

func (t *term) close() {
	killGroup(t.cmd.Process.Pid)
	t.f.Close()
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	var keep []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, strings.TrimRight(l, " "))
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return scrub(strings.Join(keep, "\n"))
}

var rawJSON = regexp.MustCompile(`\{"|"error"\s*:`)

func calm(s string) error {
	for _, bad := range []string{"panic:", "goroutine ", "runtime error", "nil pointer"} {
		if strings.Contains(s, bad) {
			return fmt.Errorf("output has %q:\n%s", bad, tail(s, 8))
		}
	}
	if rawJSON.MatchString(s) {
		return fmt.Errorf("output shows raw JSON:\n%s", tail(s, 8))
	}
	return nil
}

func finishedTurns(home string) int {
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*.jsonl"))
	n := 0
	for _, p := range paths {
		raw, _ := os.ReadFile(p)
		n += bytes.Count(raw, []byte(`{"type":"usage"`))
	}
	return n
}

func replied(t *term, prompt, want string) error {
	before := strings.Count(strings.ToLower(t.text()), want)
	turns := finishedTurns(t.home)
	t.line(prompt)
	if err := t.waitFor("the turn to start", func() bool {
		return strings.Contains(t.screen(), "esc to stop") || strings.Count(strings.ToLower(t.text()), want) > before+1
	}, 10*time.Second); err != nil {
		return err
	}
	if err := t.waitFor("the reply", func() bool { return t.idle() }, stepWait); err != nil {
		return err
	}
	text := strings.ToLower(t.text())
	if strings.Count(text, want) < before+2 {
		return fmt.Errorf("no %q in the reply\n%s", want, tail(t.screen(), 10))
	}
	if strings.Contains(t.text(), "error: ") {
		return fmt.Errorf("an error was shown\n%s", tail(t.screen(), 10))
	}
	if finishedTurns(t.home) <= turns {
		return fmt.Errorf("no finished response was saved to the session\n%s", tail(t.screen(), 6))
	}
	return nil
}

func flowConnect(s *shared) error {
	t, err := startTUI(s.dir, s.home, 100, 40, nil)
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.waitText("No model provider is connected yet.", 10*time.Second); err != nil {
		return err
	}
	if err := t.waitText("press 1-6", 5*time.Second); err != nil {
		return err
	}
	t.write("3")
	if err := t.waitText("Get a key at openrouter.ai/keys", 5*time.Second); err != nil {
		return err
	}
	t.paste(orKey)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(t.text(), orKey) {
		return errors.New("the pasted key is visible on screen")
	}
	t.write("\r")
	if err := t.waitText("connected: openrouter", 20*time.Second); err != nil {
		return err
	}
	if err := t.waitText("Pick a model from openrouter", 20*time.Second); err != nil {
		return err
	}
	if err := t.waitFor("the model list", func() bool {
		return strings.Contains(t.screen(), " models") && strings.Contains(t.screen(), "enter pick")
	}, 20*time.Second); err != nil {
		return err
	}
	s.pass("first run, /connect with a pasted key")

	t.write(freeModel)
	if err := t.waitFor("the filter to match "+freeModel, func() bool { return strings.Contains(t.screen(), "> "+freeModel) }, 5*time.Second); err != nil {
		return err
	}
	t.write("\r")
	if err := t.waitText("model: openrouter/"+freeModel, 5*time.Second); err != nil {
		return err
	}
	s.pass("/model filter and pick a free model")

	if err := replied(t, "Reply with exactly one word: pong", "pong"); err != nil {
		return err
	}
	s.pass("real reply")

	note := filepath.Join(s.dir, "note.txt")
	os.WriteFile(note, []byte("hello world\n"), 0o644)
	t.line("Use the edit tool to replace the word world with aperta in note.txt. Do not use shell.")
	asked := false
	err = t.waitFor("the edit permission prompt", func() bool {
		scr := t.screen()
		if strings.Contains(scr, "Allow edit?") {
			return true
		}
		if strings.Contains(scr, "press 1") {
			t.write("3")
			time.Sleep(300 * time.Millisecond)
		}
		if raw, _ := os.ReadFile(note); strings.Contains(string(raw), "aperta") {
			asked = true
		}
		return asked
	}, stepWait)
	if err != nil {
		return err
	}
	if asked {
		return errors.New("the file changed without an edit permission prompt")
	}
	if raw, _ := os.ReadFile(note); string(raw) != "hello world\n" {
		return errors.New("the file changed before the edit was approved")
	}
	t.write("1")
	if err := t.waitFor("the edit to land and the turn to end", func() bool {
		raw, _ := os.ReadFile(note)
		return strings.Contains(string(raw), "hello aperta") && t.idle()
	}, stepWait); err != nil {
		return err
	}
	s.pass("file edit with a permission prompt")

	t.line("Without using any tools, type out the numbers from 1 to 500 yourself, one per line, nothing else.")
	if err := t.waitFor("streaming to start", func() bool {
		if strings.Contains(t.screen(), "press 1") {
			t.write("3")
			time.Sleep(300 * time.Millisecond)
		}
		return strings.Contains(t.screen(), "writing")
	}, stepWait); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	stoppedBefore := strings.Count(t.text(), "stopped")
	t.write("\x03")
	if err := t.waitFor("the turn to stop", func() bool { return strings.Count(t.text(), "stopped") > stoppedBefore && t.idle() }, 5*time.Second); err != nil {
		return err
	}
	if err := calm(t.screen()); err != nil {
		return err
	}
	if err := t.quit(); err != nil {
		return fmt.Errorf("quit: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(t.text(), "resume with: aperta -r") {
		return fmt.Errorf("no resume hint after quitting\n%s", tail(t.text(), 6))
	}
	s.pass("ctrl+c mid-stream, then quit")
	return nil
}

func flowResume(s *shared) error {
	t, err := startTUI(s.dir, s.home, 100, 60, nil, "-c")
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.waitText("resumed session", 10*time.Second); err != nil {
		return err
	}
	for _, want := range []string{"> Reply with exactly one word: pong", "> Use the edit tool", "openrouter/" + freeModel} {
		if !strings.Contains(t.text(), want) {
			return fmt.Errorf("resumed screen is missing %q\n%s", want, tail(t.text(), 14))
		}
	}
	return t.quit()
}

type event struct {
	Type       string                 `json:"type"`
	Message    string                 `json:"message"`
	Kind       string                 `json:"kind"`
	Text       string                 `json:"text"`
	Error      string                 `json:"error"`
	SessionID  string                 `json:"session_id"`
	ToolCall   *struct{ Name string } `json:"tool_call"`
	ToolResult *struct {
		Name    string
		Output  string
		IsError bool `json:"is_error"`
	} `json:"tool_result"`
}

func headless(dir, home string, env []string, args ...string) ([]event, string, error) {
	cmd := exec.Command(binary, append([]string{"run", "--json"}, args...)...)
	cmd.Dir = dir
	cmd.Env = baseEnv(home, env...)
	ownGroup(cmd)
	var stderr, stdout strings.Builder
	cmd.Stderr, cmd.Stdout = &stderr, &stdout
	waitOutput := func(c *exec.Cmd) (string, error) {
		err := c.Wait()
		return stdout.String(), err
	}
	var timedOut atomic.Bool
	limit := stepWait + 30*time.Second
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	track(cmd)
	timer := time.AfterFunc(limit, func() {
		timedOut.Store(true)
		killGroup(cmd.Process.Pid)
	})
	defer timer.Stop()
	out, err := waitOutput(cmd)
	if timedOut.Load() {
		return nil, stderr.String(), fmt.Errorf("aperta run did not finish within %s", limit)
	}
	var events []event
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			return nil, stderr.String(), fmt.Errorf("stdout line is not JSON: %q", tail(sc.Text(), 1))
		}
		events = append(events, ev)
	}
	if err != nil {
		last := ""
		for _, ev := range events {
			switch {
			case ev.Message != "" && ev.Type == "error":
				last = ev.Message
			case ev.Error != "":
				last = ev.Error
			}
		}
		return events, stderr.String(), fmt.Errorf("exit: %v; error event: %s; stderr: %s", err, last, tail(stderr.String(), 4))
	}
	return events, stderr.String(), nil
}

func replyText(events []event) (text string, deltas int, thinking bool) {
	for _, ev := range events {
		if ev.Type == "message_delta" {
			if ev.Kind == "thinking" {
				thinking = true
				continue
			}
			text += ev.Text
			deltas++
		}
	}
	return
}

func finished(events []event) error {
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		return errors.New("the run did not end with a done event")
	}
	return nil
}

func flowHeadless(s *shared) error {
	events, _, err := headless(s.dir, s.home, nil, "Reply with exactly one word: pong")
	if err != nil {
		return err
	}
	if err := finished(events); err != nil {
		return err
	}
	if text, _, _ := replyText(events); !strings.Contains(strings.ToLower(text), "pong") {
		return fmt.Errorf("reply %q has no pong", tail(text, 2))
	}
	return nil
}

func responsesEnv() []string { return []string{"OPENAI_API_KEY=" + orKey} }

func flowResponsesFree(s *shared) error {
	events, _, err := headless(s.dir, s.home, responsesEnv(), "--provider", "openai", "--base-url", "https://openrouter.ai/api/v1", "--model", freeModel, "Reply with exactly one word: pong")
	if err != nil {
		return err
	}
	if err := finished(events); err != nil {
		return err
	}
	text, deltas, _ := replyText(events)
	if !strings.Contains(strings.ToLower(text), "pong") {
		return fmt.Errorf("reply %q has no pong", tail(text, 2))
	}
	if deltas < 1 {
		return errors.New("no streamed text")
	}
	return nil
}

func flowResponsesTools(s *shared) error {
	note := filepath.Join(s.dir, "note.txt")
	if _, err := os.Stat(note); err != nil {
		os.WriteFile(note, []byte("hello aperta\n"), 0o644)
	}
	rec, err := startRecorder("https://openrouter.ai")
	if err != nil {
		return err
	}
	defer rec.close()
	events, _, err := headless(s.dir, s.home, responsesEnv(), "--provider", "openai", "--base-url", rec.url+"/api/v1", "--model", cheapOpenAI, "--effort", "low",
		"Use the read tool to read note.txt. Then write a short paragraph of about 60 words about the file, and quote its first line exactly.")
	if err != nil {
		return err
	}
	if err := finished(events); err != nil {
		return err
	}
	var called, ok bool
	for _, ev := range events {
		if ev.ToolCall != nil && ev.ToolCall.Name == "read" {
			called = true
		}
		if ev.ToolResult != nil && ev.ToolResult.Name == "read" && !ev.ToolResult.IsError {
			ok = true
		}
	}
	if !called || !ok {
		return fmt.Errorf("no successful read tool call (called=%v ok=%v)", called, ok)
	}
	text, deltas, _ := replyText(events)
	if !strings.Contains(strings.ToLower(text), "hello") {
		return fmt.Errorf("reply %q does not quote the file", tail(text, 3))
	}
	if deltas < 3 {
		return fmt.Errorf("reply was not streamed (%d text deltas)", deltas)
	}
	id := events[len(events)-1].SessionID
	raw, err := os.ReadFile(filepath.Join(s.home, "sessions", id+".jsonl"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), `"type":"reasoning"`) {
		return fmt.Errorf("no reasoning item was kept for the next turn; output item types: %v", itemTypes(raw))
	}
	bodies := rec.bodies()
	if len(bodies) < 2 {
		return fmt.Errorf("expected a second request after the tool call, saw %d", len(bodies))
	}
	var next struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(bodies[1], &next); err != nil {
		return fmt.Errorf("second request is not JSON: %v", err)
	}
	var sent []string
	for _, it := range next.Input {
		sent = append(sent, it.Type)
	}
	if !contains(sent, "reasoning") || !contains(sent, "function_call") || !contains(sent, "function_call_output") {
		return fmt.Errorf("the request after the tool call did not send back reasoning, the call and its output: %v", sent)
	}
	return nil
}

type recorder struct {
	url  string
	srv  *http.Server
	mu   sync.Mutex
	seen [][]byte
}

func startRecorder(target string) (*recorder, error) {
	up, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &recorder{url: "http://" + ln.Addr().String()}
	proxy := httputil.NewSingleHostReverseProxy(up)
	base := proxy.Director
	proxy.Director = func(req *http.Request) {
		base(req)
		req.Host = up.Host
	}
	proxy.FlushInterval = -1
	r.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			body, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
			r.mu.Lock()
			r.seen = append(r.seen, body)
			r.mu.Unlock()
		}
		proxy.ServeHTTP(w, req)
	})}
	go r.srv.Serve(ln)
	return r, nil
}

func (r *recorder) bodies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.seen...)
}

func (r *recorder) close() { r.srv.Close() }

func flowNoLeaks(s *shared) error {
	var leaks []string
	err := filepath.Walk(s.work, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || path == binary || filepath.Base(path) == "credentials.json" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, k := range secrets {
			if bytes.Contains(raw, []byte(k)) {
				rel, _ := filepath.Rel(s.work, path)
				leaks = append(leaks, rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(leaks) > 0 {
		return fmt.Errorf("a key was written outside credentials.json: %s", strings.Join(leaks, ", "))
	}
	return nil
}

func itemTypes(raw []byte) []string {
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		var rec struct {
			Message *struct {
				Content []struct {
					Type string          `json:"type"`
					Raw  json.RawMessage `json:"raw"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Message == nil {
			continue
		}
		for _, b := range rec.Message.Content {
			var item struct {
				Type string `json:"type"`
			}
			json.Unmarshal(b.Raw, &item)
			out = append(out, b.Type+"/"+item.Type)
		}
	}
	return out
}

func flowAnthropic(s *shared) error {
	home := filepath.Join(s.work, "home-anthropic")
	events, _, err := headless(s.dir, home, []string{"ANTHROPIC_API_KEY=" + anKey}, "--provider", "anthropic", "--model", haiku, "Reply with exactly one word: pong")
	if err != nil {
		return err
	}
	if err := finished(events); err != nil {
		return err
	}
	if text, _, _ := replyText(events); !strings.Contains(strings.ToLower(text), "pong") {
		return fmt.Errorf("reply %q has no pong", tail(text, 2))
	}
	return nil
}

func flowNarrow(s *shared) error {
	home := filepath.Join(s.work, "home-narrow")
	if err := seedHome(home); err != nil {
		return err
	}
	t, err := startTUI(s.dir, home, 60, 30, nil, "--provider", "openrouter", "--model", freeModel)
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.waitFor("the input box", t.idle, 10*time.Second); err != nil {
		return err
	}
	if err := replied(t, "Reply with exactly one word: pong", "pong"); err != nil {
		return err
	}
	t.line("/model")
	if err := t.waitFor("the model picker", func() bool { return strings.Contains(t.screen(), "enter pick") }, 20*time.Second); err != nil {
		return err
	}
	t.write("\x1b")
	time.Sleep(200 * time.Millisecond)
	if err := calm(t.screen()); err != nil {
		return err
	}
	return t.quit()
}

func flowNoNetwork(s *shared) error {
	home := filepath.Join(s.work, "home-offline")
	start := time.Now()
	events, stderr, err := headless(s.dir, home, []string{"ANTHROPIC_API_KEY=sk-ant-offline-000000000000", "ANTHROPIC_BASE_URL=http://127.0.0.1:9"}, "--provider", "anthropic", "--model", haiku, "hi")
	if err == nil {
		return errors.New("a run with no network succeeded")
	}
	if took := time.Since(start); took > 20*time.Second {
		return fmt.Errorf("giving up took %s", took.Round(time.Second))
	}
	var retries int
	msg := ""
	for _, ev := range events {
		if ev.Type == "retry" {
			retries++
		}
		if ev.Type == "error" {
			msg = ev.Message
		}
	}
	if retries == 0 {
		return errors.New("no retry was shown before giving up")
	}
	if msg != "Can't reach Anthropic. Check your internet connection, then send again." {
		return fmt.Errorf("error message = %q", msg)
	}
	if strings.Contains(err.Error(), "dial tcp") {
		return fmt.Errorf("the run's error shows raw network details: %s", tail(err.Error(), 2))
	}
	return calm(msg + "\n" + stderr)
}

func flowBadKey(s *shared) error {
	home := filepath.Join(s.work, "home-bad")
	bad := "sk-or-v1-" + strings.Repeat("0", 64)
	t, err := startTUI(s.dir, home, 100, 40, nil)
	if err != nil {
		return err
	}
	defer t.close()
	if err := t.waitText("press 1-6", 10*time.Second); err != nil {
		return err
	}
	t.write("3")
	if err := t.waitText("Get a key at", 5*time.Second); err != nil {
		return err
	}
	if strings.Contains(t.text(), "rejected the key") {
		return errors.New("a rejection was on screen before the key was sent")
	}
	t.paste(bad)
	t.write("\r")
	if err := t.waitFor("the rejection of this key", func() bool { return strings.Contains(t.screen(), "rejected the key (401)") }, 30*time.Second); err != nil {
		return err
	}
	if live := strings.Join(strings.Fields(t.screen()), " "); !strings.Contains(live, "sent 73 characters, sk-or-…0000") {
		return fmt.Errorf("the rejection on screen does not say what was sent\n%s", tail(t.screen(), 8))
	}
	if !strings.Contains(t.screen(), "Get a key at") {
		return fmt.Errorf("after a rejection the key step is not shown again\n%s", tail(t.screen(), 8))
	}
	if _, err := os.Stat(filepath.Join(home, "credentials.json")); err == nil {
		return errors.New("a rejected key was saved")
	}
	if err := calm(t.screen()); err != nil {
		return err
	}
	t.close()
	events, stderr, err := headless(s.dir, home, []string{"OPENROUTER_API_KEY=" + bad}, "--provider", "openrouter", "--model", freeModel, "hi")
	if err == nil {
		return errors.New("a run with a bad key succeeded")
	}
	msg := ""
	for _, ev := range events {
		if ev.Error != "" {
			msg = ev.Error
		}
	}
	if msg == "" {
		return fmt.Errorf("headless bad key gave no error event; stderr: %q", tail(stderr, 3))
	}
	if err := calm(msg + "\n" + stderr + "\n" + err.Error()); err != nil {
		return fmt.Errorf("headless bad key: %v", err)
	}
	if !strings.Contains(msg, "openrouter") {
		return fmt.Errorf("headless error does not name the provider: %q", msg)
	}
	return nil
}
