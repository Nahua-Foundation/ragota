package heuristic

import (
	"testing"
)

func TestIsBackupOrTempFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		// Backup files
		{"file.bak", true},
		{"backup.bak", true},
		{"data.bak", true},
		{"file.old", true},
		{"backup.old", true},

		// Temp files
		{"file.tmp", true},
		{"temp.tmp", true},
		{"file.swp", true},
		{"file.swo", true},

		// Editor backup files
		{"file~", true},
		{"config~", true},

		// Regular files (should NOT be backup)
		{"file.txt", false},
		{"main.go", false},
		{"app.ts", false},
		{"user.controller.ts", false},
		{"data.json", false},
	}

	for _, tt := range tests {
		got := IsBackupOrTempFile(tt.name)
		if got != tt.want {
			t.Errorf("IsBackupOrTempFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBinaryExts_IncludesBackup(t *testing.T) {
	backupExts := []string{".bak", ".tmp", ".swp", ".swo", ".old"}

	for _, ext := range backupExts {
		if !BinaryExts[ext] {
			t.Errorf("BinaryExts should include %s", ext)
		}
	}
}
