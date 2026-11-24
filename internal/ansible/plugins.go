package ansible

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed plugins/connection/*.py
var pluginFS embed.FS

// installPlugins materializes the embedded Ansible plugins so the plugin
// loader can find them, and returns the directory to point Ansible at.
func installPlugins() (string, error) {
	dir, err := os.MkdirTemp("", "strangle-ansible-plugins-*")
	if err != nil {
		return "", err
	}

	entries, err := pluginFS.ReadDir("plugins/connection")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := pluginFS.ReadFile("plugins/connection/" + entry.Name())
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.WriteFile(path, data, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}
