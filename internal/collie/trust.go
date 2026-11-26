package collie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Antigravity asks "Do you trust the contents of this project?" the first
// time it runs in a folder, even with --dangerously-skip-permissions: folder
// trust is its own setting. When Collie starts an agy agent with permissions
// bypassed (the human's choice in settings), it trusts the folder first, so a
// new worker doesn't sit on that dialog.

// agySettings is where agy keeps trustedWorkspaces.
func agySettings(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// trustAgyFolder adds dir (and the path it resolves to) to agy's trusted
// workspaces on this computer, keeping every other setting as it is.
func trustAgyFolder(home, dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("not an absolute folder: %q", dir)
	}
	want := []string{filepath.Clean(dir)}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != want[0] {
		want = append(want, r)
	}
	file := agySettings(home)
	cfg := map[string]json.RawMessage{}
	if b, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var trusted []string
	if raw, ok := cfg["trustedWorkspaces"]; ok {
		if err := json.Unmarshal(raw, &trusted); err != nil {
			return fmt.Errorf("%s: trustedWorkspaces: %w", file, err)
		}
	}
	added := false
	for _, p := range want {
		if !slices.Contains(trusted, p) {
			trusted, added = append(trusted, p), true
		}
	}
	if !added {
		return nil
	}
	cfg["trustedWorkspaces"], _ = json.Marshal(trusted)
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".collie-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// trustForStart trusts cwd for an agy agent about to start with bypassed
// permissions, on the machine it runs on. Failures only get logged: the
// worst case is the dialog Collie already shows as "needs you".
func (s *Server) trustForStart(ctx context.Context, machine, herdrKind, cwd string, args []string) {
	if herdrKind != "agy" || !slices.Contains(args, "--dangerously-skip-permissions") {
		return
	}
	var err error
	if mc, e := s.herdr.machine(machine); e != nil {
		err = e
	} else if mc.Local {
		err = trustAgyFolder(s.home, s.expand(cwd))
	} else {
		err = s.trustRemote(ctx, machine, cwd)
	}
	if err != nil {
		log.Printf("agy 目录信任没写上（%s %s）：%v", machine, cwd, err)
	}
}

func (s *Server) trustRemote(ctx context.Context, machine, cwd string) error {
	client, err := s.herdr.Client(machine)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in := map[string]string{"kind": "agy", "dir": cwd}
	return nodeJSONWithStatusError(ctx, client, http.MethodPost, "/api/trust", in, nil, func(resp *http.Response) error {
		return fmt.Errorf("node: %s", resp.Status) // an older node: 404
	})
}

// trustFolder is the node side of trustRemote.
func (s *Server) trustFolder(w http.ResponseWriter, r *http.Request) {
	var body struct{ Kind, Dir string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind != "agy" {
		writeJSON(w, 400, map[string]any{"error": "want {kind: agy, dir}"})
		return
	}
	if err := trustAgyFolder(s.home, s.expand(body.Dir)); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
