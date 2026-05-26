package docker

import (
	"os"
	"path/filepath"
	"testing"

	"ragota/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// New() constructor
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	cfg := config.DockerConfig{
		Network: "ragota-net",
		Qdrant: config.DockerContainerCfg{
			Name:  "ragota-qdrant",
			Image: "qdrant/qdrant:latest",
			Ports: []string{"6333:6333"},
		},
	}
	r := New("/workspace", cfg)

	require.NotNil(t, r)
	assert.Equal(t, "/workspace", r.WorkingDir)
	assert.Equal(t, "ragota-net", r.Cfg.Network)
	assert.Equal(t, "ragota-qdrant", r.Cfg.Qdrant.Name)
}

func TestNew_EmptyConfig(t *testing.T) {
	r := New("", config.DockerConfig{})
	require.NotNil(t, r)
	assert.Equal(t, "", r.WorkingDir)
	assert.Empty(t, r.Cfg.Network)
}

// ---------------------------------------------------------------------------
// resolveVolume
// ---------------------------------------------------------------------------

func TestResolveVolume_AbsolutePath(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("/data/storage:/qdrant/storage")
	assert.Equal(t, "/data/storage:/qdrant/storage", result)
}

func TestResolveVolume_RelativePath(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume(".ragota/qdrant_storage:/qdrant/storage")
	assert.Equal(t, filepath.Join("/workspace", ".ragota/qdrant_storage")+":/qdrant/storage", result)
}

func TestResolveVolume_DotSlashPath(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("./data:/container/data")
	assert.Equal(t, filepath.Join("/workspace", "./data")+":/container/data", result)
}

func TestResolveVolume_HomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("~/certs:/certs:ro")
	assert.Equal(t, filepath.Join(home, "certs")+":/certs:ro", result)
}

func TestResolveVolume_NoColon(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("/just/a/path")
	assert.Equal(t, "/just/a/path", result)
}

func TestResolveVolume_Empty(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("")
	assert.Equal(t, "", result)
}

func TestResolveVolume_RelativeWithMultipleColons(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume("./data:/container/data:ro")
	assert.Equal(t, filepath.Join("/workspace", "./data")+":/container/data:ro", result)
}

// ---------------------------------------------------------------------------
// volumesMismatch
// ---------------------------------------------------------------------------

func TestVolumesMismatch_NoMounts(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	st := &containerState{Mounts: nil}
	assert.False(t, r.volumesMismatch(st, []string{"./data:/data"}))
}

func TestVolumesMismatch_NoVolumes(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	st := &containerState{Mounts: []string{"/data:/container"}}
	assert.False(t, r.volumesMismatch(st, nil))
}

func TestVolumesMismatch_BothEmpty(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	st := &containerState{Mounts: nil}
	assert.False(t, r.volumesMismatch(st, nil))
}

func TestVolumesMismatch_Matching(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	resolved := r.resolveVolume("./data:/container")
	st := &containerState{Mounts: []string{resolved}}
	assert.False(t, r.volumesMismatch(st, []string{"./data:/container"}))
}

func TestVolumesMismatch_StaleVolume(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	st := &containerState{Mounts: []string{"/old/path:/container"}}
	assert.True(t, r.volumesMismatch(st, []string{"./new/path:/container"}))
}

func TestVolumesMismatch_ExtraMountInContainer(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	resolved := r.resolveVolume("./data:/container")
	st := &containerState{Mounts: []string{resolved, "/extra:/extra"}}
	// The configured volumes all match, so no mismatch
	assert.False(t, r.volumesMismatch(st, []string{"./data:/container"}))
}

func TestVolumesMismatch_MultipleVolumes(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	resolved1 := r.resolveVolume("./data:/container/data")
	resolved2 := r.resolveVolume("./config:/container/config")
	st := &containerState{Mounts: []string{resolved1, resolved2}}

	// All match
	assert.False(t, r.volumesMismatch(st, []string{"./data:/container/data", "./config:/container/config"}))
	// One doesn't match
	assert.True(t, r.volumesMismatch(st, []string{"./data:/container/data", "./other:/container/other"}))
}

// ---------------------------------------------------------------------------
// ensureVolumes
// ---------------------------------------------------------------------------

func TestEnsureVolumes_CreatesDirectories(t *testing.T) {
	root := t.TempDir()
	r := New(root, config.DockerConfig{})

	err := r.ensureVolumes()
	require.NoError(t, err)

	// .ragota/qdrant_storage should exist
	storagePath := filepath.Join(root, ".ragota", "qdrant_storage")
	info, err := os.Stat(storagePath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEnsureVolumes_Idempotent(t *testing.T) {
	root := t.TempDir()
	r := New(root, config.DockerConfig{})

	require.NoError(t, r.ensureVolumes())
	require.NoError(t, r.ensureVolumes()) // second call should succeed
}

// ---------------------------------------------------------------------------
// PsService struct
// ---------------------------------------------------------------------------

func TestPsService_Fields(t *testing.T) {
	ps := PsService{
		Name:   "ragota-qdrant",
		State:  "running",
		Status: "running=true",
	}
	assert.Equal(t, "ragota-qdrant", ps.Name)
	assert.Equal(t, "running", ps.State)
	assert.Equal(t, "running=true", ps.Status)
}

// ---------------------------------------------------------------------------
// embeddedDockerfiles map
// ---------------------------------------------------------------------------

func TestEmbeddedDockerfiles_IsMap(t *testing.T) {
	assert.NotNil(t, embeddedDockerfiles)
}

// ---------------------------------------------------------------------------
// proxyVars
// ---------------------------------------------------------------------------

func TestProxyVars_ContainsExpectedVars(t *testing.T) {
	expected := []string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
		"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	}
	assert.Equal(t, expected, proxyVars)
}

// ---------------------------------------------------------------------------
// containerState struct
// ---------------------------------------------------------------------------

func TestContainerState_Defaults(t *testing.T) {
	st := containerState{}
	assert.Equal(t, "", st.Status)
	assert.False(t, st.Running)
	assert.False(t, st.OpenStdin)
	assert.Equal(t, "", st.Image)
	assert.Nil(t, st.Mounts)
}

// ---------------------------------------------------------------------------
// runContainer with empty name/image → no-op
// ---------------------------------------------------------------------------

func TestRunContainer_EmptyNameNoOp(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	err := r.runContainer(t.Context(), config.DockerContainerCfg{Name: "", Image: "some:image"})
	assert.NoError(t, err)
}

func TestRunContainer_EmptyImageNoOp(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	err := r.runContainer(t.Context(), config.DockerContainerCfg{Name: "test", Image: ""})
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Down with no containers configured — no error
// ---------------------------------------------------------------------------

func TestDown_EmptyConfig(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	err := r.Down(t.Context())
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Ps with no containers configured
// ---------------------------------------------------------------------------

func TestPs_EmptyConfig(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	result, err := r.Ps(t.Context())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestPs_WithQdrantConfigured(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{
		Qdrant: config.DockerContainerCfg{Name: "nonexistent-container-xyz"},
	})
	result, err := r.Ps(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "nonexistent-container-xyz", result[0].Name)
	assert.Equal(t, "absent", result[0].State)
}

// ---------------------------------------------------------------------------
// Up with empty config (no images)
// ---------------------------------------------------------------------------

func TestUp_EmptyConfig(t *testing.T) {
	root := t.TempDir()
	r := New(root, config.DockerConfig{})
	// Up with no images configured should only create volumes
	err := r.Up(t.Context(), false)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// resolveVolume edge cases
// ---------------------------------------------------------------------------

func TestResolveVolume_OnlyColon(t *testing.T) {
	r := New("/workspace", config.DockerConfig{})
	result := r.resolveVolume(":/container")
	// host = "" which is not abs, so resolves relative to WorkingDir
	assert.Contains(t, result, ":/container")
}

func TestResolveVolume_WorkingDirEmpty(t *testing.T) {
	r := New("", config.DockerConfig{})
	result := r.resolveVolume("./data:/container")
	// filepath.Join("", "./data") = "data"
	assert.Contains(t, result, ":/container")
}

// ---------------------------------------------------------------------------
// runLSPContainer with empty image → no-op
// ---------------------------------------------------------------------------

func TestRunLSPContainer_EmptyImage(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	err := r.runLSPContainer(t.Context(), config.LSPDockerCfg{Image: ""})
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Up with LSP configured but empty image
// ---------------------------------------------------------------------------

func TestUp_WithLSP_AllFlag_EmptyImage(t *testing.T) {
	root := t.TempDir()
	r := New(root, config.DockerConfig{
		LSP: config.LSPDockerCfg{Image: ""},
	})
	err := r.Up(t.Context(), true)
	require.NoError(t, err)
}

func TestUp_WithLSP_NotAllFlag(t *testing.T) {
	root := t.TempDir()
	r := New(root, config.DockerConfig{
		LSP: config.LSPDockerCfg{Image: "some-lsp-image"},
	})
	// all=false should NOT start LSP
	err := r.Up(t.Context(), false)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Down with LSP configured
// ---------------------------------------------------------------------------

func TestDown_WithLSP(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{
		LSP: config.LSPDockerCfg{Image: "some-lsp-image"},
	})
	// Will try to stop "ragota-lsp" container (which doesn't exist — no error expected)
	err := r.Down(t.Context())
	assert.NoError(t, err)
}

func TestDown_WithBothContainers(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{
		Qdrant: config.DockerContainerCfg{Name: "nonexistent-q"},
		LSP:    config.LSPDockerCfg{Image: "some-image"},
	})
	err := r.Down(t.Context())
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Ps with LSP configured
// ---------------------------------------------------------------------------

func TestPs_WithLSP(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{
		Qdrant: config.DockerContainerCfg{Name: "nonexistent-q"},
		LSP:    config.LSPDockerCfg{Image: "some-image"},
	})
	result, err := r.Ps(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "nonexistent-q", result[0].Name)
	assert.Equal(t, "absent", result[0].State)
	assert.Equal(t, "ragota-lsp", result[1].Name)
	// ragota-lsp may exist from previous runs — state can be "absent", "exited", "running" etc.
	assert.NotEmpty(t, result[1].State)
}

// ---------------------------------------------------------------------------
// inspectPs — absent container
// ---------------------------------------------------------------------------

func TestInspectPs_Absent(t *testing.T) {
	r := New(t.TempDir(), config.DockerConfig{})
	ps := r.inspectPs(t.Context(), "nonexistent-container-xyz-123")
	assert.Equal(t, "nonexistent-container-xyz-123", ps.Name)
	assert.Equal(t, "absent", ps.State)
}

// ---------------------------------------------------------------------------
// ensureVolumes error path (invalid path)
// ---------------------------------------------------------------------------

func TestEnsureVolumes_InvalidPath(t *testing.T) {
	// /dev/null is a file, not a directory — MkdirAll should fail
	r := New("/dev/null/impossible", config.DockerConfig{})
	err := r.ensureVolumes()
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Available — may or may not have docker installed
// ---------------------------------------------------------------------------

func TestAvailable_ReturnsErrorOrNil(t *testing.T) {
	// Just ensure it doesn't panic
	_ = Available(t.Context())
}
