package caching

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"io"
	"os"
	"path"
)

var cli *client.Client

const varnishImage = "varnish:7.7.1-alpine"

type VarnishConfig struct {
	BackendPort  string
	Vcl          string
	DefaultTtl   string
	DefaultGrace string
	DefaultKeep  string
}

func init() {
	var err error
	// create a Docker client
	cli, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}
	reader, err := cli.ImagePull(context.Background(), varnishImage, image.PullOptions{})
	if err != nil {
		panic(err)
	}
	defer reader.Close()
	io.Copy(os.Stdout, reader)
}

func StartVarnishInDocker(config VarnishConfig) (string, func(), error) {
	// write vcl as default.vcl file in a temporary directory
	tmpDir, err := os.MkdirTemp("", "varnish")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmpDir)

	vclFileName := path.Join(tmpDir, "default.vcl")
	err = os.WriteFile(vclFileName, []byte(`vcl 4.1;
backend default {
	.host = "host.docker.internal";
	.port = "`+config.BackendPort+`";
}
`+config.Vcl), 0644)
	if err != nil {
		return "", nil, err
	}

	// create a Varnish container
	containerResponse, err := cli.ContainerCreate(context.Background(), &container.Config{
		Image: varnishImage,
		ExposedPorts: nat.PortSet{
			// Expose an unprivileged port (we use 8080).
			// The image only exposes the privileged port 80 and 8443 by default.
			// We also must expose any port other than the image-declared ports
			// if we want to map these ports to the host.
			"8080/tcp": struct{}{},
		},
		Cmd: []string{
			"-n",
			"/tmp/varnish_workdir",
			"-t",
			withDefault(config.DefaultTtl, "0s"),
			"-p",
			"default_grace=" + withDefault(config.DefaultGrace, "0s"),
			"-p",
			"default_keep=" + withDefault(config.DefaultKeep, "0s"),
		},
		Env: []string{
			// The entrypoint script of the image uses environment variables
			// to override the bind port (we use 8080) and the cache size (we use 1M).
			"VARNISH_HTTP_PORT=8080",
			"VARNISH_SIZE=1M",
		},
	}, &container.HostConfig{
		CapDrop:        []string{"ALL"}, // <- drop all capabilities
		Privileged:     false,           // <- run as unprivileged user
		ReadonlyRootfs: true,            // <- mount the root filesystem as read-only
		AutoRemove:     true,            // <- automatically remove the container when it exits
		ExtraHosts: []string{
			// Make the host's network available to the container
			// via the special DNS name host.docker.internal.
			"host.docker.internal:host-gateway",
		},
		Tmpfs: map[string]string{
			// Mount a tmpfs volume to /tmp for the Varnish workdir.
			"/tmp": "exec,mode=700,uid=1000,gid=1000",
		},
		// Mount the default.vcl file we created above as /etc/varnish/default.vcl
		Binds: []string{vclFileName + ":/etc/varnish/default.vcl"},
		PortBindings: nat.PortMap{
			// Map the container's port 8080 to a random port on the host.
			// We will later figure out the allocated host port.
			"8080/tcp": []nat.PortBinding{{
				HostIP:   "127.0.0.1", // <- bind to loopback interface
				HostPort: "0",         // <- use random host port
			}},
		},
	}, nil, nil, "")
	if err != nil {
		return "", nil, err
	}

	// start the container
	err = cli.ContainerStart(context.Background(), containerResponse.ID, container.StartOptions{})
	if err != nil {
		return "", nil, err
	}

	// tail logs of container
	i, err := cli.ContainerLogs(context.Background(), containerResponse.ID, container.LogsOptions{
		ShowStderr: true,
		ShowStdout: true,
		Timestamps: false,
		Follow:     true,
		Tail:       "40",
	})
	if err != nil {
		return "", nil, err
	}
	hdr := make([]byte, 8)
	go func() {
		fmt.Printf("Start tailing logs for container %s\n", containerResponse.ID)
		for {
			_, err := i.Read(hdr)
			if err != nil {
				break
			}
			var w io.Writer
			switch hdr[0] {
			case 1:
				w = os.Stdout
			default:
				w = os.Stderr
			}
			count := binary.BigEndian.Uint32(hdr[4:])
			dat := make([]byte, count)
			_, err = i.Read(dat)
			fmt.Fprint(w, string(dat))
		}
		fmt.Printf("Stop tailing logs for container %s\n", containerResponse.ID)
	}()

	// figure out the allocated host port (note: we used "0" as port above)
	containerInspect, err := cli.ContainerInspect(context.Background(), containerResponse.ID)
	if err != nil {
		return "", nil, err
	}
	varnishPort := containerInspect.NetworkSettings.Ports["8080/tcp"][0].HostPort

	// return a function that will stop the container
	return varnishPort, func() {
		err = cli.ContainerStop(context.Background(), containerResponse.ID, container.StopOptions{})
	}, nil
}

func withDefault(s string, defaultValue string) string {
	if s == "" {
		return defaultValue
	}
	return s
}

