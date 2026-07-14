package config

import "testing"

// TestKnownNativeToolsMatchesSwitch locks KnownNativeTools to the IsToolEnabled
// switch: flipping each named tool's config MUST flip IsToolEnabled — a name
// the switch doesn't recognize would return the default (true) regardless.
func TestKnownNativeToolsMatchesSwitch(t *testing.T) {
	for _, name := range KnownNativeTools() {
		cfg := &ToolsConfig{}
		setNativeToolEnabled(cfg, name, false)
		if cfg.IsToolEnabled(name) {
			t.Errorf("tool %q: not recognized by IsToolEnabled (default true leaked)", name)
		}
		setNativeToolEnabled(cfg, name, true)
		if !cfg.IsToolEnabled(name) {
			t.Errorf("tool %q: enabling had no effect", name)
		}
	}
}

func setNativeToolEnabled(t *ToolsConfig, name string, v bool) {
	switch name {
	case "web":
		t.Web.Enabled = v
	case "cron":
		t.Cron.Enabled = v
	case "exec":
		t.Exec.Enabled = v
	case "skills":
		t.Skills.Enabled = v
	case "media_cleanup":
		t.MediaCleanup.Enabled = v
	case "append_file":
		t.AppendFile.Enabled = v
	case "edit_file":
		t.EditFile.Enabled = v
	case "find_skills":
		t.FindSkills.Enabled = v
	case "recall":
		t.Recall.Enabled = v
	case "i2c":
		t.I2C.Enabled = v
	case "install_skill":
		t.InstallSkill.Enabled = v
	case "list_dir":
		t.ListDir.Enabled = v
	case "load_image":
		t.LoadImage.Enabled = v
	case "message":
		t.Message.Enabled = v
	case "read_file":
		t.ReadFile.Enabled = v
	case "serial":
		t.Serial.Enabled = v
	case "spawn":
		t.Spawn.Enabled = v
	case "spawn_status":
		t.SpawnStatus.Enabled = v
	case "spi":
		t.SPI.Enabled = v
	case "subagent":
		t.Subagent.Enabled = v
	case "web_fetch":
		t.WebFetch.Enabled = v
	case "send_file":
		t.SendFile.Enabled = v
	case "send_tts":
		t.SendTTS.Enabled = v
	case "write_file":
		t.WriteFile.Enabled = v
	case "mcp":
		t.MCP.Enabled = v
	}
}
