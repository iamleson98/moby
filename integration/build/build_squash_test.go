package build

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/build"
	containertypes "github.com/docker/docker/api/types/container"
	dclient "github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/daemon"
	"github.com/docker/docker/testutil/fakecontext"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestBuildSquashParent(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, testEnv.UsingSnapshotter(), "squash is not implemented for containerd image store")

	ctx := testutil.StartSpan(baseContext, t)

	var client dclient.APIClient
	if !testEnv.DaemonInfo.ExperimentalBuild {
		skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")

		d := daemon.New(t, daemon.WithExperimental())
		d.StartWithBusybox(ctx, t)
		defer d.Stop(t)
		client = d.NewClientT(t)
	} else {
		client = testEnv.APIClient()
	}

	dockerfile := `
		FROM busybox
		RUN echo hello > /hello
		RUN echo world >> /hello
		RUN echo hello > /remove_me
		ENV HELLO world
		RUN rm /remove_me
		`

	// build and get the ID that we can use later for history comparison
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	name := strings.ToLower(t.Name())
	resp, err := client.ImageBuild(ctx,
		source.AsTarReader(t),
		build.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{name},
		})
	assert.NilError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	inspect, err := client.ImageInspect(ctx, name)
	assert.NilError(t, err)
	origID := inspect.ID

	// build with squash
	resp, err = client.ImageBuild(ctx,
		source.AsTarReader(t),
		build.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Squash:      true,
			Tags:        []string{name},
		})
	assert.NilError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	cid := container.Run(ctx, t, client,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", "cat /hello"),
	)

	poll.WaitOn(t, container.IsStopped(ctx, client, cid))
	reader, err := client.ContainerLogs(ctx, cid, containertypes.LogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)

	actualStdout := new(bytes.Buffer)
	actualStderr := io.Discard
	_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(strings.TrimSpace(actualStdout.String()), "hello\nworld"))

	container.Run(ctx, t, client,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", "[ ! -f /remove_me ]"),
	)
	container.Run(ctx, t, client,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", `[ "$(echo $HELLO)" = "world" ]`),
	)

	origHistory, err := client.ImageHistory(ctx, origID)
	assert.NilError(t, err)
	testHistory, err := client.ImageHistory(ctx, name)
	assert.NilError(t, err)

	inspect, err = client.ImageInspect(ctx, name)
	assert.NilError(t, err)
	assert.Check(t, is.Len(testHistory, len(origHistory)+1))
	assert.Check(t, is.Len(inspect.RootFS.Layers, 2))
}
