package collie

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
)

// Config fields marked x-secret (extensions/README.md §6): kept on the hub in
// COLLIE_HOME/ext/<id>/config-secrets.json (0600), apart from the config in
// the database, never sent back to a page; a program gets them in
// initialize.config.

func (h *extHost) configSecretsFile(id string) string {
	return filepath.Join(extDir(h.home, id), "config-secrets.json")
}

func (h *extHost) configSecrets(id string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(h.configSecretsFile(id))
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		log.Printf("ext %s: config secrets: %v", id, err)
	}
	return out
}

// setConfigSecrets saves the x-secret fields given ("" leaves one as it is)
// and clears the ones named in clear.
func (h *extHost) setConfigSecrets(m *ExtManifest, set map[string]any, clear []string) error {
	if m.Configuration == nil {
		return nil
	}
	cur := h.configSecrets(m.ID)
	changed := false
	for k, p := range m.Configuration.Properties {
		if !p.Secret {
			continue
		}
		if v, ok := set[k].(string); ok && v != "" {
			cur[k], changed = v, true
		}
		if slices.Contains(clear, k) {
			delete(cur, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writePrivate(h.configSecretsFile(m.ID), cur)
}

// extConfigSecret is an x-secret config value of an extension that is on.
func (s *Server) extConfigSecret(extID, key string) (string, error) {
	if s.ext == nil {
		return "", fmt.Errorf("extensions run on the hub")
	}
	m := s.ext.manifest(extID)
	if m == nil || !s.ext.setting(extID).Enabled {
		return "", fmt.Errorf("extension %s is not on", extID)
	}
	if p, ok := m.Configuration.propertyOK(key); !ok || !p.Secret {
		return "", fmt.Errorf("%s has no secret %q", extID, key)
	}
	v, ok := s.ext.configSecrets(extID)[key]
	if !ok {
		return "", fmt.Errorf("%s: %s is not set", extID, key)
	}
	return v, nil
}

// ExtWorkbenchItem is one contributes.workbench entry of an extension that is on.
type ExtWorkbenchItem struct {
	Ext  string          `json:"ext"`
	Item json.RawMessage `json:"item"`
}

// extWorkbench lists the 工作台 modules extensions that are on declare.
func (s *Server) extWorkbench() []ExtWorkbenchItem {
	if s.ext == nil || !s.workbenchOn() {
		return nil
	}
	var out []ExtWorkbenchItem
	for _, v := range s.ext.views() {
		if !v.Setting.Enabled {
			continue
		}
		for _, it := range v.Manifest.Contributes.Workbench {
			out = append(out, ExtWorkbenchItem{Ext: v.Manifest.ID, Item: it})
		}
	}
	return out
}
