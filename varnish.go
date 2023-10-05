package caching

import (
	"context"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"io"
	"os"
	"path"
	"strconv"
)

var cli *client.Client

func init() {
	var err error
	// create a Docker client
	cli, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}
	reader, err := cli.ImagePull(context.Background(), "varnish:7.4.1-alpine", types.ImagePullOptions{})
	if err != nil {
		panic(err)
	}
	defer reader.Close()
	io.Copy(os.Stdout, reader)
}

func StartVarnishInDocker(backendPort, vcl, defaultTtl, defaultGrace string) (int, func(), error) {
	// write vcl as default.vcl file in a temporary directory
	tmpDir, err := os.MkdirTemp("", "varnish")
	if err != nil {
		return 0, nil, err
	}
	defer os.RemoveAll(tmpDir)

	vclFileName := path.Join(tmpDir, "default.vcl")
	err = os.WriteFile(vclFileName, []byte(`vcl 4.1;
backend default {
	.host = "host.docker.internal";
	.port = "`+backendPort+`";
}
`+vcl), 0644)
	if err != nil {
		return 0, nil, err
	}

	// create a Varnish container
	containerResponse, err := cli.ContainerCreate(context.Background(), &container.Config{
		Image: "varnish:7.4.1-alpine",
		ExposedPorts: nat.PortSet{
			"8080/tcp": struct{}{},
		},
		Cmd: []string{
			"-n",
			"/tmp/varnish_workdir",
			"-t",
			defaultTtl,
			"-p",
			"default_grace=" + defaultGrace,
		},
		Env: []string{
			"VARNISH_HTTP_PORT=8080",
			"VARNISH_SIZE=1M",
		},
	}, &container.HostConfig{
		CapDrop:        []string{"ALL"},
		Privileged:     false,
		ReadonlyRootfs: true,
		AutoRemove:     true,
		ExtraHosts: []string{
			"host.docker.internal:host-gateway",
		},
		Tmpfs: map[string]string{
			"/tmp": "exec,mode=777,uid=1000,gid=1000",
		},
		Binds: []string{vclFileName + ":/etc/varnish/default.vcl"},
		PortBindings: nat.PortMap{
			"8080/tcp": []nat.PortBinding{{
				HostIP:   "",
				HostPort: "0",
			}},
		},
	}, nil, nil, "")
	if err != nil {
		return 0, nil, err
	}

	// start the container
	err = cli.ContainerStart(context.Background(), containerResponse.ID, types.ContainerStartOptions{})
	if err != nil {
		return 0, nil, err
	}

	// figure out the allocated port
	containerInspect, err := cli.ContainerInspect(context.Background(), containerResponse.ID)
	if err != nil {
		return 0, nil, err
	}
	varnishPort := containerInspect.NetworkSettings.Ports["8080/tcp"][0].HostPort
	varnishPortAsInt, err := strconv.Atoi(varnishPort)
	if err != nil {
		return 0, nil, err
	}

	// return a function that will stop the container
	return varnishPortAsInt, func() {
		err = cli.ContainerStop(context.Background(), containerResponse.ID, container.StopOptions{})
	}, nil
}
