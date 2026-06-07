package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// New() constructor
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	r := New("/workspace")
	require.NotNil(t, r)
	assert.Equal(t, "/workspace", r.WorkingDir)
	assert.Equal(t, 0, r.QdrantPort)
}

// ---------------------------------------------------------------------------
// findFreePort
// ---------------------------------------------------------------------------

func TestFindFreePort(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	assert.Greater(t, port, 0)
}

// ---------------------------------------------------------------------------
// QdrantURL
// ---------------------------------------------------------------------------

func TestQdrantURL(t *testing.T) {
	r := New("/workspace")
	r.QdrantPort = 6333
	assert.Equal(t, "http://127.0.0.1:6333", r.QdrantURL())
}

// ---------------------------------------------------------------------------
// resolveVolume
// ---------------------------------------------------------------------------

func TestResolveVolume_AbsolutePath(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume("/data/storage:/qdrant/storage")
	assert.Equal(t, "/data/storage:/qdrant/storage", result)
}

func TestResolveVolume_RelativePath(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume(".ragota/qdrant_storage:/qdrant/storage")
	assert.Equal(t, filepath.Join("/workspace", ".ragota/qdrant_storage")+":/qdrant/storage", result)
}

func TestResolveVolume_DotSlashPath(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume("./data:/container/data")
	assert.Equal(t, filepath.Join("/workspace", "./data")+":/container/data", result)
}

func TestResolveVolume_HomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	r := New("/workspace")
	result := r.resolveVolume("~/certs:/certs:ro")
	assert.Equal(t, filepath.Join(home, "certs")+":/certs:ro", result)
}

func TestResolveVolume_NoColon(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume("/just/a/path")
	assert.Equal(t, "/just/a/path", result)
}

func TestResolveVolume_Empty(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume("")
	assert.Equal(t, "", result)
}

func TestResolveVolume_RelativeWithMultipleColons(t *testing.T) {
	r := New("/workspace")
	result := r.resolveVolume("./data:/container/data:ro")
	assert.Equal(t, filepath.Join("/workspace", "./data")+":/container/data:ro", result)
}

// ---------------------------------------------------------------------------
// volumesMismatch
// ---------------------------------------------------------------------------

func TestVolumesMismatch_NoMounts(t *testing.T) {
	r := New("/workspace")
	st := &containerState{Mounts: nil}
	assert.False(t, r.volumesMismatch(st, []string{"./data:/data"}))
}

func TestVolumesMismatch_NoVolumes(t *testing.T) {
	r := New("/workspace")
	st := &containerState{Mounts: []string{"/data:/container"}}
	assert.False(t, r.volumesMismatch(st, nil))
}

func TestVolumesMismatch_BothEmpty(t *testing.T) {
	r := New("/workspace")
	st := &containerState{Mounts: nil}
	assert.False(t, r.volumesMismatch(st, nil))
}

func TestVolumesMismatch_Matching(t *testing.T) {
	r := New("/workspace")
	resolved := r.resolveVolume("./data:/container")
	st := &containerState{Mounts: []string{resolved}}
	assert.False(t, r.volumesMismatch(st, []string{"./data:/container"}))
}

func TestVolumesMismatch_StaleVolume(t *testing.T) {
	r := New("/workspace")
	st := &containerState{Mounts: []string{"/old/path:/container"}}
	assert.True(t, r.volumesMismatch(st, []string{"./new/path:/container"}))
}

func TestVolumesMismatch_ExtraMountInContainer(t *testing.T) {
	r := New("/workspace")
	resolved := r.resolveVolume("./data:/container")
	st := &containerState{Mounts: []string{resolved, "/extra:/extra"}}
	assert.False(t, r.volumesMismatch(st, []string{"./data:/container"}))
}

func TestVolumesMismatch_MultipleVolumes(t *testing.T) {
	r := New("/workspace")
	resolved1 := r.resolveVolume("./data:/container/data")
	resolved2 := r.resolveVolume("./config:/container/config")
	st := &containerState{Mounts: []string{resolved1, resolved2}}

	assert.False(t, r.volumesMismatch(st, []string{"./data:/container/data", "./config:/container/config"}))
	assert.True(t, r.volumesMismatch(st, []string{"./data:/container/data", "./other:/container/other"}))
}

// ---------------------------------------------------------------------------
// ensureVolumes
// ---------------------------------------------------------------------------

func TestEnsureVolumes_CreatesDirectories(t *testing.T) {
	root := t.TempDir()
	r := New(root)

	err := r.ensureVolumes()
	require.NoError(t, err)

	storagePath := filepath.Join(root, ".ragota", "qdrant_storage")
	info, err := os.Stat(storagePath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEnsureVolumes_Idempotent(t *testing.T) {
	root := t.TempDir()
	r := New(root)

	require.NoError(t, r.ensureVolumes())
	require.NoError(t, r.ensureVolumes())
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
// runQdrant with empty name → no-op (no container to start)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Down — no error
// ---------------------------------------------------------------------------

func TestDown(t *testing.T) {
	r := New(t.TempDir())
	err := r.Down(t.Context())
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Ps
// ---------------------------------------------------------------------------

func TestPs(t *testing.T) {
	r := New(t.TempDir())
	result, err := r.Ps(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "ragota-qdrant", result[0].Name)
	assert.Equal(t, "ragota-lsp", result[1].Name)
	// Both should be "absent" or "exited" since docker isn't running them
	assert.Contains(t, []string{"absent", "exited"}, result[0].State)
}

// ---------------------------------------------------------------------------
// inspectPs — absent container
// ---------------------------------------------------------------------------

func TestInspectPs_Absent(t *testing.T) {
	r := New(t.TempDir())
	ps := r.inspectPs(t.Context(), "nonexistent-container-xyz-123")
	assert.Equal(t, "nonexistent-container-xyz-123", ps.Name)
	assert.Equal(t, "absent", ps.State)
}

// ---------------------------------------------------------------------------
// ensureVolumes error path (invalid path)
// ---------------------------------------------------------------------------

func TestEnsureVolumes_InvalidPath(t *testing.T) {
	r := New("/dev/null/impossible")
	err := r.ensureVolumes()
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Available — may or may not have docker installed
// ---------------------------------------------------------------------------

func TestAvailable_ReturnsErrorOrNil(t *testing.T) {
	_ = Available(t.Context())
}
