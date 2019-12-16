/*
	Copyright 2019 whiteblock Inc.
	This file is a part of the genesis.

	Genesis is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	Genesis is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package repository

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/whiteblock/genesis/pkg/entity"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/pkg/errors"
)

//DockerRepository provides extra functions for docker service, which could be placed inside of docker
//service, but would make the testing more difficult
type DockerRepository interface {
	//WithTLSClientConfig provides the opt for TLS auth
	WithTLSClientConfig(cacertPath, certPath, keyPath string) client.Opt

	//EnsureImagePulled checks if the docker host contains an image and pulls it if it does not
	EnsureImagePulled(ctx context.Context, cli entity.Client,
		imageName string, auth string) error

	//GetContainerByName attempts to find a container with the given name and return information on it.
	GetContainerByName(ctx context.Context, cli entity.Client, containerName string) (types.Container, error)

	//GetNetworkByName attempts to find a network with the given name and return information on it.
	GetNetworkByName(ctx context.Context, cli entity.Client, networkName string) (types.NetworkResource, error)

	//GetVolumeByName attempts to find a volume with the given name and return information on it.
	GetVolumeByName(ctx context.Context, cli entity.Client, volumeName string) (*types.Volume, error)

	//HostHasImage returns true if the docker host has an image matching what was given
	HostHasImage(ctx context.Context, cli entity.Client, image string) (bool, error)
}

type dockerRepository struct {
}

//NewDockerRepository creates a new DockerRepository instance
func NewDockerRepository() DockerRepository {
	return &dockerRepository{}
}

func (da dockerRepository) WithTLSClientConfig(cacertPath, certPath, keyPath string) client.Opt {
	return func(c *client.Client) error {
		opts := tlsconfig.Options{
			CAFile:             cacertPath,
			CertFile:           certPath,
			KeyFile:            keyPath,
			ExclusiveRootPools: true,
			InsecureSkipVerify: true,
		}
		config, err := tlsconfig.Client(opts)
		if err != nil {
			return errors.Wrap(err, "failed to create tls config")
		}
		if transport, ok := c.HTTPClient().Transport.(*http.Transport); ok {
			transport.TLSClientConfig = config
			return nil
		}
		return errors.Errorf("cannot apply tls config to transport: %T", c.HTTPClient().Transport)
	}
}

//HostHasImage returns true if the docker host has an image matching what was given
func (da dockerRepository) HostHasImage(ctx context.Context, cli entity.Client, image string) (bool, error) {
	imgs, err := cli.ImageList(ctx, types.ImageListOptions{All: false})
	if err != nil {
		return false, err
	}
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			if tag == image {
				return true, nil
			}
		}
		for _, digest := range img.RepoDigests {
			if digest == image {
				return true, nil
			}
		}
	}
	return false, nil
}

//EnsureImagePulled checks if the docker host contains an image and pulls it if it does not
func (da dockerRepository) EnsureImagePulled(ctx context.Context, cli entity.Client,
	imageName string, auth string) error {
	exists, err := da.HostHasImage(ctx, cli, imageName)
	if exists || err != nil {
		return err
	}

	rd, err := cli.ImagePull(ctx, imageName, types.ImagePullOptions{
		Platform:     "Linux", //TODO: pull out to a config
		RegistryAuth: auth,
	})
	if err != nil {
		return err
	}
	defer rd.Close()
	_, err = ioutil.ReadAll(rd)
	return err
}

//GetNetworkByName attempts to find a network with the given name and return information on it.
func (da dockerRepository) GetNetworkByName(ctx context.Context, cli entity.Client,
	networkName string) (types.NetworkResource, error) {

	nets, err := cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return types.NetworkResource{}, err
	}
	for _, net := range nets {
		if net.Name == networkName {
			return net, nil
		}
	}
	return types.NetworkResource{}, fmt.Errorf("could not find the network \"%s\"", networkName)
}

//GetContainerByName attempts to find a container with the given name and return information on it.
func (da dockerRepository) GetContainerByName(ctx context.Context, cli entity.Client,
	containerName string) (types.Container, error) {

	cntrs, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		return types.Container{}, err
	}
	for _, cntr := range cntrs {
		for _, name := range cntr.Names {
			if strings.Trim(name, "/") == strings.Trim(containerName, "/") {
				return cntr, nil
			}
		}
	}
	return types.Container{}, fmt.Errorf("could not find the container \"%s\"", containerName)
}

//GetVolumeByName attempts to find a volume with the given name and return information on it.
func (da dockerRepository) GetVolumeByName(ctx context.Context, cli entity.Client,
	volumeName string) (*types.Volume, error) {

	bdy, err := cli.VolumeList(ctx, filters.Args{})
	if err != nil {
		return nil, err
	}

	for _, vol := range bdy.Volumes {
		if vol == nil {
			continue
		}
		if vol.Name == volumeName {
			return vol, nil
		}
	}
	return nil, fmt.Errorf("could not find the volume \"%s\"", volumeName)
}
