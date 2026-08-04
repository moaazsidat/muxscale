package attach

import "encoding/json"

// parseKittyLS extracts agent-capable windows from `kitty @ ls` JSON.
//
// The structure is os-window -> tabs -> windows, and the fields muxscale needs
// (id, foreground process, cwd) live at the innermost level.
func parseKittyLS(data []byte) ([]KittyPane, error) {
	var osWindows []struct {
		Tabs []struct {
			Windows []struct {
				ID                  int    `json:"id"`
				Title               string `json:"title"`
				Cwd                 string `json:"cwd"`
				ForegroundProcesses []struct {
					Cmdline []string `json:"cmdline"`
					Cwd     string   `json:"cwd"`
				} `json:"foreground_processes"`
			} `json:"windows"`
		} `json:"tabs"`
	}
	if err := json.Unmarshal(data, &osWindows); err != nil {
		return nil, err
	}

	var out []KittyPane
	for _, osw := range osWindows {
		for _, tab := range osw.Tabs {
			for _, w := range tab.Windows {
				p := KittyPane{ID: w.ID, Title: w.Title, Cwd: w.Cwd}
				if len(w.ForegroundProcesses) > 0 {
					fp := w.ForegroundProcesses[0]
					if len(fp.Cmdline) > 0 {
						p.Command = baseName(fp.Cmdline[0])
					}
					if p.Cwd == "" {
						p.Cwd = fp.Cwd
					}
				}
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
