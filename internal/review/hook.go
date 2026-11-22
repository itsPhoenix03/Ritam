package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ritam/internal/brand"
)

// blockExit is the exit code both CLIs read as "do not run this call"; the
// hook's stderr is the reason they show the agent. Any other non-zero code is
// read as the hook failing, which a CLI may treat as no objection.
const blockExit = 2

// RunHook is `ritam hook pretool`, which an agent CLI runs before each tool
// call with the call on stdin. It asks the review server named in the
// environment and returns the exit code: 0 lets the CLI's own permission
// rules decide, 2 blocks the call with the reason on stderr.
//
// Every failure blocks: no address or token in the environment, a call too
// large to send, a server that does not answer in time or answers something
// unexpected. The CLI then refuses the call, rather than running a call
// nothing checked. stdout is left empty: the CLI reads it for decisions, and
// passing says nothing.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	_ = stdout
	if len(args) == 0 || args[0] != preTool {
		return block(stderr, fmt.Sprintf("unknown hook event %q; the CLI's settings name a hook this %s does not have",
			strings.Join(args, " "), brand.Name))
	}

	body, err := io.ReadAll(io.LimitReader(stdin, MaxPayload+1))
	switch {
	case err != nil:
		return block(stderr, "could not read the tool call: "+err.Error())
	case len(body) > MaxPayload:
		return block(stderr, fmt.Sprintf("the tool call is larger than %d KiB, too large to check; split the work into smaller calls",
			MaxPayload>>10))
	}

	base, token := getenv(URLEnv()), getenv(TokenEnv())
	if base == "" || token == "" {
		return block(stderr, fmt.Sprintf("the hook is not connected to a running %s (%s or %s is not set), so the call cannot be checked",
			brand.Name, URLEnv(), TokenEnv()))
	}

	v, err := ask(base, token, body, waitFrom(getenv(WaitEnv())))
	if err != nil {
		return block(stderr, fmt.Sprintf("could not check the tool call with %s: %v", brand.Name, err))
	}
	switch v.Decision {
	case Pass:
		return 0
	case Deny:
		return block(stderr, v.Reason)
	default:
		return block(stderr, fmt.Sprintf("%s answered %q, which this hook does not understand", brand.Name, v.Decision))
	}
}

// ask sends the call to the review server and returns its verdict. A refusal
// the server explains (no running turn, say) comes back as a Deny verdict
// whatever its HTTP status.
func ask(base, token string, body []byte, wait time.Duration) (Verdict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+hookPath+preTool, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	// Never through a proxy: the proxy settings in the environment are the
	// agent's to change, and the server is on this machine.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Verdict{}, err
	}
	var v Verdict
	if json.Unmarshal(data, &v) != nil || v.Decision == "" {
		return Verdict{}, fmt.Errorf("unexpected answer (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && v.Decision != Deny {
		return Verdict{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return v, nil
}

// block writes reason for the agent and returns the code that refuses the
// call.
func block(stderr io.Writer, reason string) int {
	if strings.TrimSpace(reason) == "" {
		reason = "refused"
	}
	fmt.Fprintf(stderr, "%s: %s\n", brand.Name, reason)
	return blockExit
}

// waitFrom reads how long to wait for the server, in seconds, as the turn
// set it; a missing or odd value means the default, and the bound holds
// whatever the environment says.
func waitFrom(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return hookTimeout
	}
	return min(time.Duration(n)*time.Second, maxHookWait)
}
