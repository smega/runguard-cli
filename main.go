// runguard-cli: the open-source RunGuard CLI/agent.
//
// This is the entire client. It talks to the RunGuard API over plain HTTPS
// and has no dependencies beyond the Go standard library -- there is
// nothing else in this repo that isn't shown here, which is the point.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Version can be overridden at build-time with:
//
//	go build -ldflags "-X main.Version=v0.1.4"
var Version = "0.1.4" // override via -ldflags at build time

// defaultAPIBase is a placeholder — override it at build time with
// -ldflags "-X main.defaultAPIBase=https://your-runguard-instance.example"
// (same mechanism as Version above), or override per-invocation with
// --api / RUNGUARD_API. Never set to a real value here: this is public
// source, and a real default would silently point every unconfigured
// build at one specific RunGuard instance.
var defaultAPIBase = "https://runguard.example.com"

// heartbeatPayload mirrors the RunGuard backend's HeartbeatPayload struct
// field for field. These two must stay in sync by hand -- there's no shared
// module between this CLI and the (closed-source) server -- so if you add a
// field here, the server needs a matching one to actually use it.
type heartbeatPayload struct {
	Status       string     `json:"status"` // "ok" | "fail"
	ExitCode     *int       `json:"exit_code,omitempty"`
	DurationMs   *int64     `json:"duration_ms,omitempty"`
	Env          string     `json:"env,omitempty"`
	Tags         []string   `json:"tags,omitempty"`
	AgentVersion string     `json:"agent_version,omitempty"`
	StdoutTail   string     `json:"stdout_tail,omitempty"`
	StderrTail   string     `json:"stderr_tail,omitempty"`
	Ts           *time.Time `json:"ts,omitempty"`
}

// stringSliceFlag lets --tag be repeated (--tag=a --tag=b) and/or
// comma-separated (--tag=a,b) on a single flag.FlagSet.
type stringSliceFlag []string

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "self-update" {
		if err := SelfUpdate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "self-update error:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "wrap":
		runWrap(os.Args[2:])
	case "register":
		runRegister(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(Version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `RunGuard CLI

Subcommands:

  runguard wrap [flags] -- command [args...]
      Run a command, capture its output, and post a heartbeat to RunGuard.

  runguard register [flags]
      Create (or look up) a job by name using your account's agent token,
      and print the heartbeat key/URL to use with "wrap".

  runguard self-update [flags]
      Replace the current binary with the latest release.

  runguard version
      Print CLI version.

Examples:

  runguard wrap --key=RUNGUARD_KEY -- /opt/scripts/orders.sh
  runguard wrap --key=RUNGUARD_KEY --retry=2 -- /opt/scripts/flaky_sync.sh
  runguard register --token=$RUNGUARD_AGENT_TOKEN --name="orders sync" --interval=60

Common flags for "wrap":

  --key=RUNGUARD_KEY        Required (or RUNGUARD_KEY env var). Job heartbeat key.
  --endpoint=URL            Optional. Full heartbeat endpoint.
                            If not set, defaults to <api-base>/hb/KEY
  --timeout=SECONDS         HTTP timeout in seconds (default 10).
  --truncate=BYTES          Max bytes kept from the end of stdout/stderr each (default 4000).
  --env=NAME                Optional environment label (e.g. "prod", "staging").
  --tag=TAG                 Optional tag; repeatable or comma-separated.
  --retry=N                 Re-run the command up to N more times if it exits non-zero
                            (default 0 = no retry). The heartbeat reflects the final attempt.
  --retry-delay=SECONDS     Delay between retry attempts (default 2).

Flags for "register":

  --token=TOKEN             Required (or RUNGUARD_AGENT_TOKEN env var). Agent token
                            from the account page.
  --api=URL                 API base URL (or RUNGUARD_API env var). Required unless this
                            binary was built with a default baked in via -ldflags.
  --name=NAME               Required. Job name -- re-running with the same name
                            returns the same job instead of creating a duplicate.
  --interval=MINUTES        Expected heartbeat interval in minutes (default 60, ignored if --cron-expr is set).
  --grace=MINUTES           Grace period in minutes before a missed heartbeat alerts (default 0).
  --severity=LEVEL          low|normal|high|critical (default normal).
  --cron-expr=EXPR          Standard 5-field cron expression (UTC), e.g. "0 3 * * *". Switches the
                            job to schedule-based lateness checking instead of a plain interval --
                            use this to match a Kubernetes CronJob's own spec.schedule exactly.
`)
}

// runWrap implements: runguard wrap --key KEY [flags] -- cmd args...
func runWrap(args []string) {
	fs := flag.NewFlagSet("wrap", flag.ExitOnError)
	key := fs.String("key", "", "RunGuard heartbeat key (required if --endpoint not set)")
	endpoint := fs.String("endpoint", "", "Full heartbeat endpoint URL (optional)")
	timeoutSeconds := fs.Int("timeout", 10, "HTTP timeout in seconds")
	truncateBytes := fs.Int("truncate", 4000, "Max bytes kept from the end of stdout/stderr each")
	env := fs.String("env", "", "Environment label (e.g. prod, staging)")
	retry := fs.Int("retry", 0, "Re-run the command up to N more times if it exits non-zero")
	retryDelaySeconds := fs.Int("retry-delay", 2, "Delay in seconds between retry attempts")
	var tags stringSliceFlag
	fs.Var(&tags, "tag", "Tag to attach to the run (repeatable or comma-separated)")

	// Allow endpoint/key from environment too
	envKey := os.Getenv("RUNGUARD_KEY")
	envEndpoint := os.Getenv("RUNGUARD_ENDPOINT")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: runguard wrap [flags] -- command [args...]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// After flags, we expect a "--" then command
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "No command specified. Use: runguard wrap --key=KEY -- my-command ...")
		os.Exit(1)
	}
	if cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "No command specified after --.")
		os.Exit(1)
	}

	// Resolve key/endpoint
	finalEndpoint := strings.TrimSpace(*endpoint)
	finalKey := strings.TrimSpace(*key)

	if finalEndpoint == "" && envEndpoint != "" {
		finalEndpoint = strings.TrimSpace(envEndpoint)
	}
	if finalKey == "" && envKey != "" {
		finalKey = strings.TrimSpace(envKey)
	}

	if finalEndpoint == "" {
		if finalKey == "" {
			fmt.Fprintln(os.Stderr, "Error: either --endpoint or --key (or RUNGUARD_KEY / RUNGUARD_ENDPOINT) must be set.")
			os.Exit(1)
		}
		finalEndpoint = strings.TrimRight(defaultAPIBase, "/") + "/hb/" + finalKey
	}

	maxBytes := *truncateBytes
	if maxBytes <= 0 {
		maxBytes = 4000
	}

	retries := *retry
	if retries < 0 {
		retries = 0
	}
	retryDelay := time.Duration(*retryDelaySeconds) * time.Second
	if retryDelay < 0 {
		retryDelay = 0
	}

	command := cmdArgs[0]
	commandArgs := cmdArgs[1:]

	wallStart := time.Now()

	var (
		exitCode   int
		stdoutTail string
		stderrTail string
		attempt    int
	)

	for {
		attempt++
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd := exec.Command(command, commandArgs...)
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		cmd.Stdin = os.Stdin

		runErr := cmd.Run()

		exitCode = 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				// Non-exit error (e.g. exec: not found)
				exitCode = 1
			}
		}

		stdoutTail = tailString(stdoutBuf.Bytes(), maxBytes)
		stderrTail = tailString(stderrBuf.Bytes(), maxBytes)

		// Mirror the child's own output so `wrap` is still usable
		// interactively / in CI logs, not just as a silent heartbeat proxy.
		if stdoutBuf.Len() > 0 {
			os.Stdout.Write(stdoutBuf.Bytes())
		}
		if stderrBuf.Len() > 0 {
			os.Stderr.Write(stderrBuf.Bytes())
		}

		if exitCode == 0 || attempt > retries {
			break
		}

		fmt.Fprintf(os.Stderr, "runguard: attempt %d failed (exit %d), retrying in %s...\n", attempt, exitCode, retryDelay)
		if retryDelay > 0 {
			time.Sleep(retryDelay)
		}
	}

	duration := time.Since(wallStart)
	durationMs := duration.Milliseconds()

	status := "ok"
	if exitCode != 0 {
		status = "fail"
	}

	envLabel := strings.TrimSpace(*env)

	finalTags := []string(tags)
	if attempt > 1 {
		finalTags = append(finalTags, fmt.Sprintf("retried:%d", attempt-1))
	}

	now := time.Now().UTC()
	ec := exitCode
	payload := heartbeatPayload{
		Status:       status,
		ExitCode:     &ec,
		DurationMs:   &durationMs,
		Env:          envLabel,
		Tags:         finalTags,
		AgentVersion: Version,
		StdoutTail:   stdoutTail,
		StderrTail:   stderrTail,
		Ts:           &now,
	}

	if err := postHeartbeat(finalEndpoint, payload, time.Duration(*timeoutSeconds)*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to post heartbeat: %v\n", err)
		// Don't override the actual job exit code; caller cares about that
	}

	os.Exit(exitCode)
}

// tailString returns at most maxBytes from the end of raw, as a string.
func tailString(raw []byte, maxBytes int) string {
	if len(raw) > maxBytes {
		raw = raw[len(raw)-maxBytes:]
	}
	return string(raw)
}

func postHeartbeat(endpoint string, payload heartbeatPayload, timeout time.Duration) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "runguard-cli/"+Version)

	client := httpClient()

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat non-2xx: %s", resp.Status)
	}

	return nil
}

func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// ---------- register ----------

type agentRegisterRequest struct {
	Name            string `json:"name"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
	GraceMinutes    int    `json:"grace_minutes,omitempty"`
	Severity        string `json:"severity,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
}

type agentRegisterResponse struct {
	JobID        string `json:"job_id"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	HeartbeatURL string `json:"heartbeat_url"`
	Created      bool   `json:"created"`
	Error        string `json:"error,omitempty"`
}

// runRegister implements: runguard register --token=TOKEN --name=NAME [flags]
func runRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	token := fs.String("token", "", "Agent token from the account page (or RUNGUARD_AGENT_TOKEN env var)")
	apiBase := fs.String("api", envOr("RUNGUARD_API", defaultAPIBase), "API base URL (or RUNGUARD_API env var)")
	name := fs.String("name", "", "Job name (required)")
	interval := fs.Int("interval", 60, "Expected heartbeat interval in minutes (ignored if --cron-expr is set)")
	grace := fs.Int("grace", 0, "Grace period in minutes")
	severity := fs.String("severity", "", "low|normal|high|critical")
	cronExpr := fs.String("cron-expr", "", "Standard 5-field cron expression (UTC); switches the job to schedule-based lateness checking instead of a plain interval")
	timeoutSeconds := fs.Int("timeout", 10, "HTTP timeout in seconds")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: runguard register --token=TOKEN --name=NAME [flags]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	finalToken := strings.TrimSpace(*token)
	if finalToken == "" {
		finalToken = strings.TrimSpace(os.Getenv("RUNGUARD_AGENT_TOKEN"))
	}
	if finalToken == "" {
		fmt.Fprintln(os.Stderr, "Error: --token or RUNGUARD_AGENT_TOKEN is required. Find your agent token on the account page.")
		os.Exit(1)
	}

	finalName := strings.TrimSpace(*name)
	if finalName == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required.")
		os.Exit(1)
	}

	finalCronExpr := strings.TrimSpace(*cronExpr)
	if finalCronExpr == "" && *interval <= 0 {
		*interval = 60
	}
	if *grace < 0 {
		*grace = 0
	}

	reqBody := agentRegisterRequest{
		Name:            finalName,
		IntervalMinutes: *interval,
		GraceMinutes:    *grace,
		Severity:        strings.TrimSpace(*severity),
		CronExpr:        finalCronExpr,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "internal error marshaling request: %v\n", err)
		os.Exit(1)
	}

	url := strings.TrimRight(*apiBase, "/") + "/api/agent/register"

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+finalToken)
	req.Header.Set("User-Agent", "runguard-cli/"+Version)

	resp, err := httpClient().Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var out agentRegisterResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&out); decErr != nil {
		fmt.Fprintf(os.Stderr, "register: unexpected response (status %d): %v\n", resp.StatusCode, decErr)
		os.Exit(1)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := out.Error
		if msg == "" {
			msg = resp.Status
		}
		fmt.Fprintf(os.Stderr, "register failed: %s\n", msg)
		os.Exit(1)
	}

	if out.Created {
		fmt.Printf("Created job %q (id=%s)\n", out.Name, out.JobID)
	} else {
		fmt.Printf("Job %q already exists (id=%s) -- reusing it\n", out.Name, out.JobID)
	}
	fmt.Printf("Heartbeat key : %s\n", out.Key)
	fmt.Printf("Heartbeat URL : %s\n", out.HeartbeatURL)
	fmt.Println()
	fmt.Println("Use it with wrap:")
	fmt.Printf("  runguard wrap --key=%s -- your-command args...\n", out.Key)
}
