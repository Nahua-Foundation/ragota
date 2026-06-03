package lsp

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// FileURI returns a file:// URI for an absolute filesystem path.
func FileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

// PathFromFileURI parses a file:// URI and returns a local filesystem path.
func PathFromFileURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	if runtime.GOOS == "windows" && strings.HasPrefix(p, "/") && len(p) > 3 && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// ToRemotePath converts a local host path to a server-side path.
// In docker mode, hostRoot prefix is replaced with remoteRoot.
func ToRemotePath(localPath, hostRoot, remoteRoot string) string {
	if hostRoot == "" || remoteRoot == "" {
		return localPath
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		abs = localPath
	}
	abs = filepath.Clean(abs)
	host := filepath.Clean(hostRoot)
	if abs == host {
		return remoteRoot
	}
	if rel, err := filepath.Rel(host, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return remoteRoot + "/" + filepath.ToSlash(rel)
	}
	return localPath
}

// ToLocalPath converts a server URI/path to a local host path.
// In docker mode, remoteRoot prefix is replaced with hostRoot.
func ToLocalPath(remoteURI, remoteRoot, hostRoot string) string {
	path := remoteURI
	if strings.HasPrefix(remoteURI, "file://") {
		u, err := url.Parse(remoteURI)
		if err == nil {
			path = u.Path
			if runtime.GOOS == "windows" && strings.HasPrefix(path, "/") && len(path) > 3 && path[2] == ':' {
				path = path[1:]
			}
		}
	}
	if hostRoot != "" && remoteRoot != "" {
		clean := filepath.ToSlash(filepath.Clean(path))
		remote := filepath.ToSlash(filepath.Clean(remoteRoot))
		if clean == remote {
			return filepath.Clean(hostRoot)
		}
		if strings.HasPrefix(clean, remote+"/") {
			rel := strings.TrimPrefix(clean, remote+"/")
			return filepath.Join(hostRoot, filepath.FromSlash(rel))
		}
	}
	return filepath.FromSlash(path)
}

// SamePath compares two filesystem paths after Abs+Clean normalization.
func SamePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil {
		a = absA
	}
	if errB == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
