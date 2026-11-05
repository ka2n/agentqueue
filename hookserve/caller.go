package hookserve

import (
	"fmt"
	"os"
	"strings"

	"github.com/ka2n/agentqueue"
)

func exportCaller(q *agentqueue.Queue, target agentqueue.Target, getenv func(string) string) error {
	path := strings.TrimSpace(getenv("CLAUDE_ENV_FILE"))
	if path == "" {
		return nil
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "\nexport CROSSAGENT_AGENT=%s\nexport CROSSAGENT_SESSION_ID=%s\nexport AGENTQUEUE_ROOT=%s\n", quote(target.Agent), quote(target.Name), quote(q.Root()))
	return err
}
