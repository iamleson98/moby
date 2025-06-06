//go:build !windows

package image // import "github.com/docker/docker/integration/image"

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	_ "github.com/docker/docker/daemon/graphdriver/register" // register graph drivers
	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/daemon"
	"github.com/docker/docker/testutil/fakecontext"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

// This is a regression test for #38488
// It ensures that orphan layers can be found and cleaned up
// after unsuccessful image removal
func TestRemoveImageGarbageCollector(t *testing.T) {
	// This test uses very platform specific way to prevent
	// daemon for remove image layer.
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.NotAmd64)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support overlay2 on most distros")
	skip.If(t, testEnv.UsingSnapshotter, "tests the graph driver layer store that's not used with the containerd image store")

	ctx := testutil.StartSpan(baseContext, t)

	// Create daemon with overlay2 graphdriver because vfs uses disk differently
	// and this test case would not work with it.
	d := daemon.New(t, daemon.WithStorageDriver("overlay2"))
	d.Start(t)
	defer d.Stop(t)

	client := d.NewClientT(t)

	layerStore, _ := layer.NewStoreFromOptions(layer.StoreOptions{
		Root:        d.Root,
		GraphDriver: d.StorageDriver(),
	})
	i := images.NewImageService(images.ImageServiceConfig{
		LayerStore: layerStore,
	})

	const imgName = "test-garbage-collector"

	// Build a image with multiple layers
	dockerfile := `FROM busybox
	RUN echo 'echo Running...' > /run.sh`
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()
	resp, err := client.ImageBuild(ctx,
		source.AsTarReader(t),
		build.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{imgName},
		})
	assert.NilError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	img, err := client.ImageInspect(ctx, imgName)
	assert.NilError(t, err)

	// Mark latest image layer to immutable
	data := img.GraphDriver.Data
	file, _ := os.Open(data["UpperDir"])
	attr := 0x00000010
	fsflags := uintptr(0x40086602)
	argp := uintptr(unsafe.Pointer(&attr)) // #nosec G103 -- Ignore "G103: Use of unsafe calls should be audited"
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), fsflags, argp)
	assert.Equal(t, "errno 0", errno.Error())

	// Try to remove the image, it should generate error
	// but marking layer back to mutable before checking errors (so we don't break CI server)
	_, err = client.ImageRemove(ctx, imgName, image.RemoveOptions{})
	attr = 0x00000000
	argp = uintptr(unsafe.Pointer(&attr)) // #nosec G103 -- Ignore "G103: Use of unsafe calls should be audited"
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), fsflags, argp)
	assert.Equal(t, "errno 0", errno.Error())
	assert.Assert(t, err != nil)
	errStr := err.Error()
	if !strings.Contains(errStr, "permission denied") && !strings.Contains(errStr, "operation not permitted") {
		t.Errorf("ImageRemove error not an permission error %s", errStr)
	}

	// Verify that layer remaining on disk
	dir, _ := os.Stat(data["UpperDir"])
	assert.Equal(t, "true", strconv.FormatBool(dir.IsDir()))

	// Run imageService.Cleanup() and make sure that layer was removed from disk
	i.Cleanup()
	_, err = os.Stat(data["UpperDir"])
	assert.Assert(t, os.IsNotExist(err))

	// Make sure that removal pending layers does not exist on layerdb either
	layerdbItems, _ := os.ReadDir(filepath.Join(d.RootDir(), "/image/overlay2/layerdb/sha256"))
	for _, folder := range layerdbItems {
		assert.Equal(t, false, strings.HasSuffix(folder.Name(), "-removing"))
	}
}
