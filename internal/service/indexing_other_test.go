//go:build !unix

package service

import "testing"

// mkfifo has no counterpart outside unix; the rest of the classification is
// tested everywhere.
func mkfifo(*testing.T, string) (string, bool) { return "", false }
