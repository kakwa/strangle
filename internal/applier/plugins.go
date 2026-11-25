package applier

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed plugins/connection/*.py plugins/callback/*.py
var pluginFS embed.FS

// installPlugins materializes the embedded Ansible plugins so the plugin
// loader can find them, and returns the directory containing the connection/
// and callback/ subdirectories.
func installPlugins() (string, error) {
	dir, err := os.MkdirTemp("", "strangle-ansible-plugins-*")
	if err != nil {
		return "", err
	}
	for _, kind := range []string{"connection", "callback"} {
		if err := os.MkdirAll(filepath.Join(dir, kind), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		entries, err := pluginFS.ReadDir("plugins/" + kind)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := pluginFS.ReadFile("plugins/" + kind + "/" + entry.Name())
			if err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
			path := filepath.Join(dir, kind, entry.Name())
			if err := os.WriteFile(path, data, 0o600); err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
		}
	}
	return dir, nil
}
